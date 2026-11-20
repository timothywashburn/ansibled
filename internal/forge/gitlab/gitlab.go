// Package gitlab is the GitLab (API v4) implementation of forge.Forge: webhook
// parsing, the REST calls ansibled needs, and "Sign in with GitLab".
package gitlab

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/timothywashburn/ansibled/internal/forge"
)

var _ forge.Forge = (*Client)(nil)

const (
	trustTTL = 60 * time.Second
	// PusherOf looks at most this many pages of push events, and no further back
	// than pushLookback.
	pushMaxPages = 5
	pushLookback = 30 * 24 * time.Hour
	// maxStatusDescription is the commit status description limit in runes.
	maxStatusDescription = 140
	// accessDeveloper is GitLab's Developer access level, needed for commit statuses.
	accessDeveloper = 30
	// accessGuest is the lowest real membership; Minimal Access (5) is not trusted.
	accessGuest = 10
)

// Options configure a Client.
type Options struct {
	BaseURL       string // e.g. https://gitlab.example.org
	Project       string // "myorg/infra" or a numeric project id
	Token         string // project access token, sent as PRIVATE-TOKEN
	WebhookSecret string // expected X-Gitlab-Token
	TrustedGroup  string // e.g. "myorg/sysadmins" (path or numeric id)
	// CloneURL overrides the fetch URL returned by GitAuth (tests, mirrors).
	CloneURL string
	// HTTPClient is used for all API calls. Default: 30 s timeout.
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Client is a GitLab project. It is safe for concurrent use.
type Client struct {
	opts       Options
	base       *url.URL
	baseURL    string // BaseURL without trailing slash
	pid        string // escaped project id
	gid        string // escaped group id
	http       *http.Client
	log        *slog.Logger
	retryDelay time.Duration
	now        func() time.Time

	mu      sync.Mutex
	project *apiProject
	bot     *forge.User
	trust   map[int64]trustEntry
	notes   map[noteKey]int64 // id of our comment per MR and marker

	upsertMu sync.Mutex // serialises UpsertMRComment so two runs can't both create a note
}

type noteKey struct {
	iid    int
	marker string
}

type trustEntry struct {
	ok bool
	at time.Time
}

// New validates the options and returns a client. It makes no network calls.
func New(o Options) (*Client, error) {
	var errs []error
	o.BaseURL = strings.TrimRight(strings.TrimSpace(o.BaseURL), "/")
	base, err := url.Parse(o.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		errs = append(errs, errors.New("gitlab: BaseURL must be an absolute http(s) URL"))
	}
	o.Project = strings.Trim(strings.TrimSpace(o.Project), "/")
	o.TrustedGroup = strings.Trim(strings.TrimSpace(o.TrustedGroup), "/")
	for name, v := range map[string]string{
		"Project": o.Project, "Token": o.Token, "WebhookSecret": o.WebhookSecret, "TrustedGroup": o.TrustedGroup,
	} {
		if v == "" {
			errs = append(errs, fmt.Errorf("gitlab: %s is required", name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	c := &Client{
		opts:       o,
		base:       base,
		baseURL:    o.BaseURL,
		pid:        url.PathEscape(o.Project),
		gid:        url.PathEscape(o.TrustedGroup),
		http:       o.HTTPClient,
		log:        o.Logger,
		retryDelay: time.Second,
		now:        time.Now,
		trust:      map[int64]trustEntry{},
		notes:      map[noteKey]int64{},
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: requestTimeout}
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	c.log = c.log.With("forge", "gitlab")
	return c, nil
}

// Name implements forge.Forge.
func (c *Client) Name() string { return "gitlab" }

// MRRef implements forge.Forge.
func (c *Client) MRRef(iid int) string { return fmt.Sprintf("refs/merge-requests/%d/head", iid) }

// GitAuth implements forge.Forge. The URL is <BaseURL>/<project path>.git
// (or Options.CloneURL). If Project is a numeric id, the path is resolved from
// GET /projects/:id: the result cached by DefaultBranch/Check is used, and if
// there is none GitAuth makes that call itself (bounded by the request
// timeout). If it cannot be resolved the URL is empty and the git fetch fails.
func (c *Client) GitAuth() (fetchURL, authHeader string) {
	authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+c.opts.Token))
	switch {
	case c.opts.CloneURL != "":
		fetchURL = c.opts.CloneURL
	case !isNumeric(c.opts.Project):
		fetchURL = c.baseURL + "/" + c.opts.Project + ".git"
	default:
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		p, err := c.projectInfo(ctx)
		if err != nil {
			c.log.Error("gitlab: cannot resolve project path for git fetch", "project", c.opts.Project, "err", err)
		} else {
			fetchURL = c.baseURL + "/" + p.PathWithNamespace + ".git"
		}
	}
	return fetchURL, authHeader
}

func isNumeric(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0
}

type apiUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func (u apiUser) forge() forge.User { return forge.User{ID: u.ID, Username: u.Username} }

type accessLevel struct {
	AccessLevel int `json:"access_level"`
}

type apiProject struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	WebURL            string `json:"web_url"`
	Permissions       *struct {
		ProjectAccess *accessLevel `json:"project_access"`
		GroupAccess   *accessLevel `json:"group_access"`
	} `json:"permissions"`
}

// projectWebURL is the project's page, or "" while unknown (a numeric project
// id before the first API call).
func (c *Client) projectWebURL() string {
	c.mu.Lock()
	p := c.project
	c.mu.Unlock()
	if p != nil && p.WebURL != "" {
		return strings.TrimRight(p.WebURL, "/")
	}
	if _, err := strconv.ParseInt(c.opts.Project, 10, 64); err == nil {
		return ""
	}
	return c.baseURL + "/" + c.opts.Project
}

// CommitURL is the web page of a commit, or "" if unknown.
func (c *Client) CommitURL(sha string) string {
	if p := c.projectWebURL(); p != "" && sha != "" {
		return p + "/-/commit/" + url.PathEscape(sha)
	}
	return ""
}

// MRURL is the web page of a merge request, or "" if unknown.
func (c *Client) MRURL(iid int) string {
	if p := c.projectWebURL(); p != "" && iid > 0 {
		return p + "/-/merge_requests/" + strconv.Itoa(iid)
	}
	return ""
}

// projectInfo returns GET /projects/:id, cached after the first success.
func (c *Client) projectInfo(ctx context.Context) (*apiProject, error) {
	c.mu.Lock()
	p := c.project
	c.mu.Unlock()
	if p != nil {
		return p, nil
	}
	var np apiProject
	if err := c.get(ctx, "/projects/"+c.pid, nil, &np); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.project = &np
	c.mu.Unlock()
	return &np, nil
}

// DefaultBranch implements forge.Forge. The answer is cached for the
// lifetime of the client.
func (c *Client) DefaultBranch(ctx context.Context) (string, error) {
	p, err := c.projectInfo(ctx)
	if err != nil {
		return "", err
	}
	if p.DefaultBranch == "" {
		return "", fmt.Errorf("gitlab: project %s has no default branch (empty repository?)", c.opts.Project)
	}
	return p.DefaultBranch, nil
}

// BranchHead implements forge.Forge.
func (c *Client) BranchHead(ctx context.Context, branch string) (string, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := c.get(ctx, "/projects/"+c.pid+"/repository/branches/"+url.PathEscape(branch), nil, &b); err != nil {
		return "", err
	}
	if b.Commit.ID == "" {
		return "", fmt.Errorf("gitlab: branch %q has no head commit", branch)
	}
	return b.Commit.ID, nil
}

type apiMR struct {
	IID             int    `json:"iid"`
	SourceBranch    string `json:"source_branch"`
	SHA             string `json:"sha"`
	SourceProjectID int64  `json:"source_project_id"`
	TargetProjectID int64  `json:"target_project_id"`
	State           string `json:"state"`
	WebURL          string `json:"web_url"`
}

func (m apiMR) forge() forge.MR {
	return forge.MR{
		IID:          m.IID,
		SourceBranch: m.SourceBranch,
		HeadSHA:      m.SHA,
		FromFork:     m.SourceProjectID != m.TargetProjectID,
		State:        m.State,
		WebURL:       m.WebURL,
	}
}

func (c *Client) listMRs(ctx context.Context, q url.Values) ([]forge.MR, error) {
	var out []forge.MR
	err := getPages(ctx, c, "/projects/"+c.pid+"/merge_requests", q, defaultMaxPages, func(page []apiMR) bool {
		for _, m := range page {
			out = append(out, m.forge())
		}
		return true
	})
	return out, err
}

// OpenMRs implements forge.Forge: every open MR targeting this project,
// including MRs from forks.
func (c *Client) OpenMRs(ctx context.Context) ([]forge.MR, error) {
	return c.listMRs(ctx, url.Values{"state": {"opened"}})
}

// MRsForBranch implements forge.Forge: open MRs whose source is branch in
// this project (MRs from forks with a branch of the same name are excluded).
func (c *Client) MRsForBranch(ctx context.Context, branch string) ([]forge.MR, error) {
	all, err := c.listMRs(ctx, url.Values{"state": {"opened"}, "source_branch": {branch}})
	if err != nil {
		return nil, err
	}
	var out []forge.MR
	for _, m := range all {
		if !m.FromFork && m.SourceBranch == branch {
			out = append(out, m)
		}
	}
	return out, nil
}

// GetMR implements forge.Forge.
func (c *Client) GetMR(ctx context.Context, iid int) (forge.MR, error) {
	var m apiMR
	if err := c.get(ctx, "/projects/"+c.pid+"/merge_requests/"+strconv.Itoa(iid), nil, &m); err != nil {
		return forge.MR{}, err
	}
	return m.forge(), nil
}

type apiEvent struct {
	CreatedAt      time.Time `json:"created_at"`
	AuthorID       int64     `json:"author_id"`
	AuthorUsername string    `json:"author_username"`
	Author         *apiUser  `json:"author"`
	PushData       *struct {
		Ref      string `json:"ref"`
		RefType  string `json:"ref_type"`
		CommitTo string `json:"commit_to"`
	} `json:"push_data"`
}

// PusherOf implements forge.Forge using the project events API (newest
// first, at most 5 pages of 100 and 30 days back).
func (c *Client) PusherOf(ctx context.Context, branch, sha string) (*forge.User, error) {
	cutoff := c.now().Add(-pushLookback)
	q := url.Values{
		"action": {"pushed"},
		"sort":   {"desc"},
		// "after" is a date (exclusive); the cutoff check below is exact.
		"after": {cutoff.AddDate(0, 0, -1).UTC().Format("2006-01-02")},
	}
	var found *forge.User
	err := getPages(ctx, c, "/projects/"+c.pid+"/events", q, pushMaxPages, func(page []apiEvent) bool {
		for _, e := range page {
			if !e.CreatedAt.IsZero() && e.CreatedAt.Before(cutoff) {
				return false
			}
			pd := e.PushData
			if pd == nil || (pd.RefType != "" && pd.RefType != "branch") {
				continue
			}
			if pd.Ref == branch && strings.EqualFold(pd.CommitTo, sha) {
				u := forge.User{ID: e.AuthorID, Username: e.AuthorUsername}
				if e.Author != nil {
					if u.ID == 0 {
						u.ID = e.Author.ID
					}
					if u.Username == "" {
						u.Username = e.Author.Username
					}
				}
				found = &u
				return false
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

type apiMember struct {
	ID              int64  `json:"id"`
	Username        string `json:"username"`
	State           string `json:"state"`
	MembershipState string `json:"membership_state"`
	AccessLevel     int    `json:"access_level"`
	ExpiresAt       string `json:"expires_at"`
}

// active reports whether the membership counts: the user is active, the
// membership is not awaiting approval, not Minimal Access and not expired.
func (m apiMember) active(now time.Time) bool {
	if m.State != "" && m.State != "active" {
		return false
	}
	if m.MembershipState != "" && m.MembershipState != "active" {
		return false
	}
	if m.AccessLevel != 0 && m.AccessLevel < accessGuest {
		return false
	}
	if m.ExpiresAt != "" {
		if t, err := time.Parse("2006-01-02", m.ExpiresAt); err == nil && !now.UTC().Before(t) {
			return false
		}
	}
	return true
}

// IsTrusted implements forge.Forge via GET /groups/:group/members/all/:user_id
// (direct and inherited members). Answers are cached for 60 s; errors are not.
func (c *Client) IsTrusted(ctx context.Context, u forge.User) (bool, error) {
	if u.ID <= 0 {
		return false, nil
	}
	now := c.now()
	c.mu.Lock()
	e, ok := c.trust[u.ID]
	c.mu.Unlock()
	if ok && now.Sub(e.at) < trustTTL {
		return e.ok, nil
	}

	var m apiMember
	err := c.get(ctx, "/groups/"+c.gid+"/members/all/"+strconv.FormatInt(u.ID, 10), nil, &m)
	var trusted bool
	switch {
	case err == nil:
		trusted = m.active(now)
	case IsNotFound(err):
		trusted = false
	default:
		return false, err
	}
	c.mu.Lock()
	c.trust[u.ID] = trustEntry{ok: trusted, at: now}
	c.mu.Unlock()
	return trusted, nil
}

// BotUser implements forge.Forge (GET /user, cached).
func (c *Client) BotUser(ctx context.Context) (forge.User, error) {
	c.mu.Lock()
	b := c.bot
	c.mu.Unlock()
	if b != nil {
		return *b, nil
	}
	var u apiUser
	if err := c.get(ctx, "/user", nil, &u); err != nil {
		return forge.User{}, err
	}
	fu := u.forge()
	c.mu.Lock()
	c.bot = &fu
	c.mu.Unlock()
	return fu, nil
}

// SetStatus implements forge.Forge. A 400 because the status already has
// that state ("Cannot transition status via :run from :running") counts as
// success.
func (c *Client) SetStatus(ctx context.Context, sha string, s forge.Status) error {
	desc := s.Description
	if r := []rune(desc); len(r) > maxStatusDescription {
		desc = string(r[:maxStatusDescription])
	}
	body := map[string]string{
		"state":       string(s.State),
		"name":        s.Name,
		"description": desc,
	}
	if s.TargetURL != "" {
		body["target_url"] = s.TargetURL
	}
	if s.Ref != "" {
		body["ref"] = s.Ref
	}
	_, err := c.do(ctx, request{method: http.MethodPost, path: "/projects/" + c.pid + "/statuses/" + url.PathEscape(sha), body: body}, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.StatusCode == http.StatusBadRequest && strings.Contains(ae.Message, "Cannot transition status") {
		c.log.Debug("gitlab: commit status already in that state", "sha", sha, "name", s.Name, "state", s.State, "msg", ae.Message)
		return nil
	}
	return err
}

type apiNote struct {
	ID     int64   `json:"id"`
	Body   string  `json:"body"`
	Author apiUser `json:"author"`
	System bool    `json:"system"`
}

// UpsertMRComment implements forge.Forge. If body lacks marker, marker is
// prepended on its own line. The note id is cached per MR and marker; if the cached note
// has been deleted the notes are searched again and, failing that, a new note
// is created.
func (c *Client) UpsertMRComment(ctx context.Context, iid int, marker, body string) error {
	if marker == "" {
		return errors.New("gitlab: UpsertMRComment: empty marker")
	}
	if !strings.Contains(body, marker) {
		body = marker + "\n" + body
	}
	c.upsertMu.Lock()
	defer c.upsertMu.Unlock()

	key := noteKey{iid, marker}
	notesPath := "/projects/" + c.pid + "/merge_requests/" + strconv.Itoa(iid) + "/notes"
	payload := map[string]string{"body": body}

	c.mu.Lock()
	id, cached := c.notes[key]
	c.mu.Unlock()
	if cached {
		err := c.editNote(ctx, key, notesPath, id, payload)
		if !IsNotFound(err) {
			return err
		}
		c.log.Info("gitlab: cached MR comment is gone, searching again", "mr", iid, "note", id)
		c.forgetNote(key)
	}

	bot, err := c.BotUser(ctx)
	if err != nil {
		return err
	}
	var found int64
	err = getPages(ctx, c, notesPath, url.Values{"sort": {"asc"}, "order_by": {"created_at"}}, defaultMaxPages, func(page []apiNote) bool {
		for _, n := range page {
			if !n.System && n.Author.ID == bot.ID && strings.Contains(n.Body, marker) {
				found = n.ID
				return false
			}
		}
		return true
	})
	if err != nil {
		return err
	}
	if found != 0 {
		if err := c.editNote(ctx, key, notesPath, found, payload); !IsNotFound(err) {
			return err
		}
		// Deleted between the listing and the edit: create a new one.
	}

	var n apiNote
	if _, err := c.do(ctx, request{method: http.MethodPost, path: notesPath, body: payload}, &n); err != nil {
		return err
	}
	c.rememberNote(key, n.ID)
	return nil
}

// editNote replaces the body of note id and remembers it as key's comment.
func (c *Client) editNote(ctx context.Context, key noteKey, notesPath string, id int64, payload map[string]string) error {
	_, err := c.do(ctx, request{method: http.MethodPut, path: notesPath + "/" + strconv.FormatInt(id, 10), body: payload}, nil)
	if err == nil {
		c.rememberNote(key, id)
	}
	return err
}

func (c *Client) rememberNote(key noteKey, id int64) {
	if id == 0 {
		return
	}
	c.mu.Lock()
	c.notes[key] = id
	c.mu.Unlock()
}

func (c *Client) forgetNote(key noteKey) {
	c.mu.Lock()
	delete(c.notes, key)
	c.mu.Unlock()
}

// Check verifies the online prerequisites for `check-config --online`: the
// token works, the project is readable (with at least Developer access when
// GitLab reports it), and the trusted group's members are visible. It returns
// every problem found.
func (c *Client) Check(ctx context.Context) []error {
	var errs []error
	if u, err := c.BotUser(ctx); err != nil {
		errs = append(errs, fmt.Errorf("token: GET /user failed: %w", err))
	} else {
		c.log.Debug("gitlab: token belongs to", "user", u.Username, "id", u.ID)
	}

	if p, err := c.projectInfo(ctx); err != nil {
		errs = append(errs, fmt.Errorf("project %s: not readable with the token: %w", c.opts.Project, err))
	} else if p.Permissions != nil {
		lvl := 0
		for _, a := range []*accessLevel{p.Permissions.ProjectAccess, p.Permissions.GroupAccess} {
			if a != nil && a.AccessLevel > lvl {
				lvl = a.AccessLevel
			}
		}
		if lvl < accessDeveloper {
			errs = append(errs, fmt.Errorf("project %s: token user has access level %d, needs Developer (30) to set commit statuses", c.opts.Project, lvl))
		}
	}

	var members []apiMember
	if err := c.get(ctx, "/groups/"+c.gid+"/members/all", url.Values{"per_page": {"1"}}, &members); err != nil {
		errs = append(errs, fmt.Errorf("trusted group %s: members not visible to the token user: %w", c.opts.TrustedGroup, err))
	} else if len(members) == 0 {
		errs = append(errs, fmt.Errorf("trusted group %s: no members visible to the token user (make the group visible to it or add the bot to the group)", c.opts.TrustedGroup))
	}
	return errs
}

// Package gitlabtest is an in-memory fake GitLab for tests. It implements
// exactly the API v4 endpoints, pagination and path encoding the gitlab
// adapter uses, the OAuth authorize/token endpoints, and it records the
// writes (commit statuses, MR notes) for assertions.
//
// API requests must carry "PRIVATE-TOKEN: <Token>" (401 otherwise); GET
// /api/v4/user also accepts "Authorization: Bearer <oauth token>" issued by
// the fake token endpoint.
package gitlabtest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/timothywashburn/ansibled/internal/forge"
)

// Credentials the fake accepts.
const (
	Token        = "glpat-fake-project-token"
	ClientID     = "fake-oauth-client-id"
	ClientSecret = "fake-oauth-client-secret"
)

// Defaults before any setter is called.
const (
	DefaultProjectPath   = "myorg/infra"
	DefaultProjectID     = 1
	DefaultBranch        = "main"
	DefaultBotID         = 100
	DefaultBotUsername   = "project_1_bot"
	forkProjectIDOffset  = 1000
	timeFormat           = "2006-01-02T15:04:05.000Z"
	defaultPerPage       = 20
	maxPerPage           = 100
	zeroSHA              = "0000000000000000000000000000000000000000"
	webhookPath          = "/webhook/gitlab"
	webhookHost          = "ansibled.invalid"
	cannotTransitionTmpl = "Cannot transition status via :%s from :%s (Reason(s): Status cannot transition via \"%s\")"
)

// RecordedStatus is one accepted POST /projects/:id/statuses/:sha.
type RecordedStatus struct {
	SHA         string
	Name        string
	State       forge.StatusState
	Description string
	TargetURL   string
	Ref         string
}

// RecordedNote is an MR note as currently stored.
type RecordedNote struct {
	ID     int64
	Body   string
	Author forge.User
	Edits  int // number of successful PUTs
}

type note struct {
	RecordedNote
	created time.Time
	updated time.Time
	deleted bool
}

type pushEvent struct {
	id       int64
	branch   string
	from, to string
	user     forge.User
	at       time.Time
}

type oauthGrant struct {
	code       string
	user       forge.User
	authorized bool
	challenge  string
	redirect   string
	used       bool
}

type failure struct {
	n      int
	status int
}

// Server is a fake GitLab. All methods are safe for concurrent use.
type Server struct {
	srv *httptest.Server

	mu            sync.Mutex
	now           func() time.Time
	projectPath   string
	projectID     int64
	defaultBranch string
	bot           forge.User
	branches      map[string]string
	mrs           []forge.MR
	events        []pushEvent
	members       map[string][]forge.User
	notes         map[int][]*note
	statuses      []RecordedStatus
	grants        []*oauthGrant
	oauthTokens   map[string]forge.User
	requests      []string
	fail          failure
	nextID        int64
}

// New starts a fake GitLab with project myorg/infra (id 1, default branch
// main) and bot user project_1_bot (id 100).
func New() *Server {
	s := newServer()
	s.srv = httptest.NewServer(s)
	return s
}

// NewWithListener is New, but serves on l instead of a random loopback port,
// e.g. on an address a container can reach (end-to-end tests). The server
// takes ownership of l; Close closes it.
func NewWithListener(l net.Listener) *Server {
	s := newServer()
	s.srv = httptest.NewUnstartedServer(s)
	_ = s.srv.Listener.Close()
	s.srv.Listener = l
	s.srv.Start()
	return s
}

func newServer() *Server {
	return &Server{
		now:           time.Now,
		projectPath:   DefaultProjectPath,
		projectID:     DefaultProjectID,
		defaultBranch: DefaultBranch,
		bot:           forge.User{ID: DefaultBotID, Username: DefaultBotUsername},
		branches:      map[string]string{},
		members:       map[string][]forge.User{},
		notes:         map[int][]*note{},
		oauthTokens:   map[string]forge.User{},
		nextID:        1,
	}
}

// URL is the base URL (use as the adapter's BaseURL).
func (s *Server) URL() string { return s.srv.URL }

// Client returns an HTTP client for the server.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Close shuts the server down.
func (s *Server) Close() { s.srv.Close() }

// SetProject sets the single project's path, numeric id and default branch.
// It is reachable as /projects/<url-encoded path> and /projects/<id>.
func (s *Server) SetProject(path string, id int64, defaultBranch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectPath, s.projectID, s.defaultBranch = path, id, defaultBranch
}

// SetBranch creates or moves a branch.
func (s *Server) SetBranch(name, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.branches[name] = sha
}

// AddMR adds (or replaces, by IID) a merge request. An empty State means
// "opened"; an empty WebURL is filled in.
func (s *Server) AddMR(mr forge.MR) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mr.State == "" {
		mr.State = "opened"
	}
	if mr.WebURL == "" {
		mr.WebURL = fmt.Sprintf("%s/%s/-/merge_requests/%d", s.srv.URL, s.projectPath, mr.IID)
	}
	for i, m := range s.mrs {
		if m.IID == mr.IID {
			s.mrs[i] = mr
			return
		}
	}
	s.mrs = append(s.mrs, mr)
}

// SetMRState changes an MR's state ("opened", "closed", "merged", "locked").
func (s *Server) SetMRState(iid int, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.mrs {
		if s.mrs[i].IID == iid {
			s.mrs[i].State = state
		}
	}
}

// AddPushEvent records that u pushed sha to branch, now.
func (s *Server) AddPushEvent(branch, sha string, u forge.User) {
	s.AddPushEventAt(branch, sha, u, time.Time{})
}

// AddPushEventAt is AddPushEvent with an explicit time (zero = now).
func (s *Server) AddPushEventAt(branch, sha string, u forge.User, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.IsZero() {
		at = s.now()
	}
	from := zeroSHA
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i].branch == branch {
			from = s.events[i].to
			break
		}
	}
	s.events = append(s.events, pushEvent{id: s.id(), branch: branch, from: from, to: sha, user: u, at: at.UTC()})
}

// AddGroupMember makes u an active Developer of group (by full path).
func (s *Server) AddGroupMember(group string, u forge.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members[group] = append(s.members[group], u)
}

// RemoveGroupMember removes u from group.
func (s *Server) RemoveGroupMember(group string, u forge.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members[group] = slices.DeleteFunc(s.members[group], func(m forge.User) bool { return m.ID == u.ID })
}

// SetBotUser sets the user the Token belongs to.
func (s *Server) SetBotUser(u forge.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bot = u
}

// AddOAuthUser queues a sign-in: the next GET /oauth/authorize redirects to
// redirect_uri?code=<code>&state=<state>, the token endpoint trades code for
// a token, and GET /api/v4/user with that bearer token returns u.
func (s *Server) AddOAuthUser(code string, u forge.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, &oauthGrant{code: code, user: u})
}

// AddNote adds a note to an MR as author u (e.g. a pre-existing comment) and
// returns its id.
func (s *Server) AddNote(iid int, u forge.User, body string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := &note{RecordedNote: RecordedNote{ID: s.id(), Body: body, Author: u}, created: s.now(), updated: s.now()}
	s.notes[iid] = append(s.notes[iid], n)
	return n.ID
}

// DeleteNote deletes an MR note (as a user would in the UI).
func (s *Server) DeleteNote(iid int, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.notes[iid] {
		if n.ID == id {
			n.deleted = true
		}
	}
}

// FailNext makes the next n API requests answer with status (e.g. 502).
func (s *Server) FailNext(n, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = failure{n: n, status: status}
}

// Statuses returns every accepted commit status, oldest first.
func (s *Server) Statuses() []RecordedStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.statuses)
}

// Notes returns the (non-deleted) notes on an MR, oldest first.
func (s *Server) Notes(iid int) []RecordedNote {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RecordedNote
	for _, n := range s.notes[iid] {
		if !n.deleted {
			out = append(out, n.RecordedNote)
		}
	}
	return out
}

// Requests returns every request received, as "METHOD /escaped/path?query".
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// CountRequests counts received requests whose "METHOD /escaped/path" (no
// query) starts with prefix.
func (s *Server) CountRequests(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		r, _, _ = strings.Cut(r, "?")
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (s *Server) id() int64 {
	id := s.nextID
	s.nextID++
	return id
}

// ---- HTTP ----

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	esc := r.URL.EscapedPath()
	rec := r.Method + " " + esc
	if r.URL.RawQuery != "" {
		rec += "?" + r.URL.RawQuery
	}
	s.requests = append(s.requests, rec)

	switch {
	case strings.HasPrefix(esc, "/api/v4/"):
		if s.fail.n > 0 {
			s.fail.n--
			writeMessage(w, s.fail.status, fmt.Sprintf("%d %s", s.fail.status, http.StatusText(s.fail.status)))
			return
		}
		s.serveAPI(w, r, strings.TrimPrefix(esc, "/api/v4/"))
	case esc == "/oauth/authorize":
		s.serveAuthorize(w, r)
	case esc == "/oauth/token":
		s.serveToken(w, r)
	default:
		writeMessage(w, http.StatusNotFound, "404 Not Found")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeMessage(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"message": msg})
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request, rest string) {
	raw := strings.Split(rest, "/")
	segs := make([]string, len(raw))
	for i, p := range raw {
		u, err := url.PathUnescape(p)
		if err != nil {
			writeMessage(w, http.StatusBadRequest, "400 Bad request - invalid path")
			return
		}
		segs[i] = u
	}

	// Authentication.
	if r.Header.Get("PRIVATE-TOKEN") != Token {
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		u, known := s.oauthTokens[bearer]
		if ok && known && r.Method == http.MethodGet && len(segs) == 1 && segs[0] == "user" {
			writeJSON(w, http.StatusOK, s.userJSON(u))
			return
		}
		writeMessage(w, http.StatusUnauthorized, "401 Unauthorized")
		return
	}

	switch {
	case len(segs) == 1 && segs[0] == "user":
		s.route(w, r, http.MethodGet, func() { writeJSON(w, http.StatusOK, s.userJSON(s.bot)) })
	case len(segs) >= 2 && segs[0] == "projects":
		if !s.isProject(segs[1]) {
			writeMessage(w, http.StatusNotFound, "404 Project Not Found")
			return
		}
		s.serveProject(w, r, segs[2:])
	case len(segs) >= 4 && segs[0] == "groups" && segs[2] == "members" && segs[3] == "all":
		s.serveMembers(w, r, segs[1], segs[4:])
	default:
		writeMessage(w, http.StatusNotFound, "404 Not Found")
	}
}

// route runs h if the method matches, else answers 405.
func (s *Server) route(w http.ResponseWriter, r *http.Request, method string, h func()) {
	if r.Method != method {
		writeMessage(w, http.StatusMethodNotAllowed, "405 Method Not Allowed")
		return
	}
	h()
}

func (s *Server) isProject(id string) bool {
	return id == s.projectPath || id == strconv.FormatInt(s.projectID, 10)
}

func (s *Server) serveProject(w http.ResponseWriter, r *http.Request, segs []string) {
	switch {
	case len(segs) == 0:
		s.route(w, r, http.MethodGet, func() { writeJSON(w, http.StatusOK, s.projectJSON()) })
	case len(segs) == 3 && segs[0] == "repository" && segs[1] == "branches":
		s.route(w, r, http.MethodGet, func() {
			sha, ok := s.branches[segs[2]]
			if !ok {
				writeMessage(w, http.StatusNotFound, "404 Branch Not Found")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"name": segs[2],
				"commit": map[string]any{
					"id": sha, "short_id": short(sha), "title": "commit " + short(sha),
					"web_url": fmt.Sprintf("%s/%s/-/commit/%s", s.srv.URL, s.projectPath, sha),
				},
				"merged": false, "protected": segs[2] == s.defaultBranch, "default": segs[2] == s.defaultBranch,
				"developers_can_push": false, "developers_can_merge": false, "can_push": true,
				"web_url": fmt.Sprintf("%s/%s/-/tree/%s", s.srv.URL, s.projectPath, segs[2]),
			})
		})
	case len(segs) == 1 && segs[0] == "merge_requests":
		s.route(w, r, http.MethodGet, func() { s.listMRs(w, r) })
	case len(segs) >= 2 && segs[0] == "merge_requests":
		iid, err := strconv.Atoi(segs[1])
		mr, ok := s.mr(iid)
		if err != nil || !ok {
			writeMessage(w, http.StatusNotFound, "404 Not found")
			return
		}
		switch {
		case len(segs) == 2:
			s.route(w, r, http.MethodGet, func() { writeJSON(w, http.StatusOK, s.mrJSON(mr)) })
		case len(segs) == 3 && segs[2] == "notes":
			switch r.Method {
			case http.MethodGet:
				s.listNotes(w, r, iid)
			case http.MethodPost:
				s.createNote(w, r, iid)
			default:
				writeMessage(w, http.StatusMethodNotAllowed, "405 Method Not Allowed")
			}
		case len(segs) == 4 && segs[2] == "notes":
			s.serveNote(w, r, iid, segs[3])
		default:
			writeMessage(w, http.StatusNotFound, "404 Not Found")
		}
	case len(segs) == 1 && segs[0] == "events":
		s.route(w, r, http.MethodGet, func() { s.listEvents(w, r) })
	case len(segs) == 2 && segs[0] == "statuses":
		s.route(w, r, http.MethodPost, func() { s.postStatus(w, r, segs[1]) })
	default:
		writeMessage(w, http.StatusNotFound, "404 Not Found")
	}
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// paginate applies GitLab offset pagination (page, per_page; X-* headers).
func paginate[T any](w http.ResponseWriter, r *http.Request, items []T) []T {
	q := r.URL.Query()
	per, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || per <= 0 {
		per = defaultPerPage
	}
	per = min(per, maxPerPage)
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page <= 0 {
		page = 1
	}
	total := len(items)
	pages := max(1, (total+per-1)/per)
	h := w.Header()
	h.Set("X-Page", strconv.Itoa(page))
	h.Set("X-Per-Page", strconv.Itoa(per))
	h.Set("X-Total", strconv.Itoa(total))
	h.Set("X-Total-Pages", strconv.Itoa(pages))
	if page < pages {
		h.Set("X-Next-Page", strconv.Itoa(page+1))
	} else {
		h.Set("X-Next-Page", "")
	}
	if page > 1 {
		h.Set("X-Prev-Page", strconv.Itoa(page-1))
	} else {
		h.Set("X-Prev-Page", "")
	}
	lo := min((page-1)*per, total)
	hi := min(lo+per, total)
	out := items[lo:hi]
	if out == nil {
		out = []T{}
	}
	return out
}

func (s *Server) mr(iid int) (forge.MR, bool) {
	for _, m := range s.mrs {
		if m.IID == iid {
			return m, true
		}
	}
	return forge.MR{}, false
}

func (s *Server) listMRs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state, branch := q.Get("state"), q.Get("source_branch")
	var list []map[string]any
	// GitLab's default order is created_at desc.
	for i := len(s.mrs) - 1; i >= 0; i-- {
		m := s.mrs[i]
		if state != "" && state != "all" && m.State != state {
			continue
		}
		if branch != "" && m.SourceBranch != branch {
			continue
		}
		list = append(list, s.mrJSON(m))
	}
	writeJSON(w, http.StatusOK, paginate(w, r, list))
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	evs := slices.Clone(s.events)
	asc := q.Get("sort") == "asc"
	slices.SortStableFunc(evs, func(a, b pushEvent) int {
		c := a.at.Compare(b.at)
		if c == 0 {
			c = int(a.id - b.id)
		}
		if asc {
			return c
		}
		return -c
	})
	var after, before time.Time
	if v := q.Get("after"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeMessage(w, http.StatusBadRequest, "400 Bad request - after is invalid")
			return
		}
		after = t.AddDate(0, 0, 1) // strictly after that day
	}
	if v := q.Get("before"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeMessage(w, http.StatusBadRequest, "400 Bad request - before is invalid")
			return
		}
		before = t
	}
	action := q.Get("action")
	var list []map[string]any
	for _, e := range evs {
		if action != "" && action != "pushed" {
			continue
		}
		if !after.IsZero() && e.at.Before(after) {
			continue
		}
		if !before.IsZero() && !e.at.Before(before) {
			continue
		}
		list = append(list, s.eventJSON(e))
	}
	writeJSON(w, http.StatusOK, paginate(w, r, list))
}

func (s *Server) serveMembers(w http.ResponseWriter, r *http.Request, group string, rest []string) {
	members, ok := s.members[group]
	if !ok {
		writeMessage(w, http.StatusNotFound, "404 Group Not Found")
		return
	}
	switch len(rest) {
	case 0:
		s.route(w, r, http.MethodGet, func() {
			list := make([]map[string]any, 0, len(members))
			for _, m := range members {
				list = append(list, s.memberJSON(m))
			}
			writeJSON(w, http.StatusOK, paginate(w, r, list))
		})
	case 1:
		s.route(w, r, http.MethodGet, func() {
			id, err := strconv.ParseInt(rest[0], 10, 64)
			if err != nil {
				writeMessage(w, http.StatusBadRequest, "400 Bad request - user_id is invalid")
				return
			}
			for _, m := range members {
				if m.ID == id {
					writeJSON(w, http.StatusOK, s.memberJSON(m))
					return
				}
			}
			writeMessage(w, http.StatusNotFound, "404 Not found")
		})
	default:
		writeMessage(w, http.StatusNotFound, "404 Not Found")
	}
}

// params merges query parameters and a JSON or form body into strings.
func params(r *http.Request) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		out[k] = v[0]
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			return nil, err
		}
		for k, v := range m {
			switch v := v.(type) {
			case string:
				out[k] = v
			case nil:
			default:
				out[k] = fmt.Sprint(v)
			}
		}
		return out, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	for k, v := range r.PostForm {
		out[k] = v[0]
	}
	return out, nil
}

var validStates = map[string]string{
	// GitLab state -> state-machine event used to reach it
	"pending": "enqueue", "running": "run", "success": "succeed", "failed": "drop", "canceled": "cancel",
}

func (s *Server) postStatus(w http.ResponseWriter, r *http.Request, sha string) {
	p, err := params(r)
	if err != nil {
		writeMessage(w, http.StatusBadRequest, "400 Bad request - "+err.Error())
		return
	}
	state := p["state"]
	event, ok := validStates[state]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state does not have a valid value"})
		return
	}
	name := p["name"]
	if name == "" {
		name = "default"
	}
	ref := p["ref"]
	// GitLab updates the latest pending/running status with the same
	// name+ref+sha; moving it to the state it already has is a 400.
	for i := len(s.statuses) - 1; i >= 0; i-- {
		st := s.statuses[i]
		if st.SHA == sha && st.Name == name && st.Ref == ref {
			if (st.State == forge.StatusPending || st.State == forge.StatusRunning) && string(st.State) == state {
				writeMessage(w, http.StatusBadRequest, fmt.Sprintf(cannotTransitionTmpl, event, st.State, event))
				return
			}
			break
		}
	}
	rs := RecordedStatus{SHA: sha, Name: name, State: forge.StatusState(state), Description: p["description"], TargetURL: p["target_url"], Ref: ref}
	s.statuses = append(s.statuses, rs)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": s.id(), "sha": sha, "ref": ref, "status": state, "name": name,
		"target_url": nullable(rs.TargetURL), "description": nullable(rs.Description),
		"created_at": s.now().UTC().Format(timeFormat), "started_at": nil, "finished_at": nil,
		"allow_failure": false, "coverage": nil, "pipeline_id": 1,
		"author": s.userJSON(s.bot),
	})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Server) listNotes(w http.ResponseWriter, r *http.Request, iid int) {
	var list []map[string]any
	for _, n := range s.notes[iid] {
		if !n.deleted {
			list = append(list, s.noteJSON(iid, n))
		}
	}
	// GitLab's default order is created_at desc.
	if r.URL.Query().Get("sort") != "asc" {
		slices.Reverse(list)
	}
	writeJSON(w, http.StatusOK, paginate(w, r, list))
}

func (s *Server) createNote(w http.ResponseWriter, r *http.Request, iid int) {
	p, err := params(r)
	if err != nil || p["body"] == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body is missing"})
		return
	}
	now := s.now()
	n := &note{RecordedNote: RecordedNote{ID: s.id(), Body: p["body"], Author: s.bot}, created: now, updated: now}
	s.notes[iid] = append(s.notes[iid], n)
	writeJSON(w, http.StatusCreated, s.noteJSON(iid, n))
}

func (s *Server) serveNote(w http.ResponseWriter, r *http.Request, iid int, idStr string) {
	id, _ := strconv.ParseInt(idStr, 10, 64)
	var n *note
	for _, x := range s.notes[iid] {
		if x.ID == id && !x.deleted {
			n = x
		}
	}
	if n == nil {
		writeMessage(w, http.StatusNotFound, "404 Note Not Found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.noteJSON(iid, n))
	case http.MethodPut:
		if n.Author.ID != s.bot.ID {
			writeMessage(w, http.StatusForbidden, "403 Forbidden")
			return
		}
		p, err := params(r)
		if err != nil || p["body"] == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body is missing"})
			return
		}
		n.Body = p["body"]
		n.Edits++
		n.updated = s.now()
		writeJSON(w, http.StatusOK, s.noteJSON(iid, n))
	default:
		writeMessage(w, http.StatusMethodNotAllowed, "405 Method Not Allowed")
	}
}

// ---- OAuth ----

func (s *Server) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bad := func(msg string) { http.Error(w, msg, http.StatusBadRequest) }
	switch {
	case r.Method != http.MethodGet:
		bad("method")
	case q.Get("client_id") != ClientID:
		bad("unknown client_id")
	case q.Get("response_type") != "code":
		bad("response_type must be code")
	case q.Get("redirect_uri") == "":
		bad("redirect_uri missing")
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		bad("PKCE S256 code_challenge required")
	case !slices.Contains(strings.Fields(q.Get("scope")), "read_user"):
		bad("scope read_user required")
	default:
		for _, g := range s.grants {
			if !g.authorized {
				g.authorized, g.challenge, g.redirect = true, q.Get("code_challenge"), q.Get("redirect_uri")
				u, err := url.Parse(g.redirect)
				if err != nil {
					bad("bad redirect_uri")
					return
				}
				rq := u.Query()
				rq.Set("code", g.code)
				if st := q.Get("state"); st != "" {
					rq.Set("state", st)
				}
				u.RawQuery = rq.Encode()
				http.Redirect(w, r, u.String(), http.StatusFound)
				return
			}
		}
		bad("no pending OAuth user (call AddOAuthUser)")
	}
}

func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	oauthErr := func(status int, code, desc string) {
		writeJSON(w, status, map[string]any{"error": code, "error_description": desc})
	}
	if r.Method != http.MethodPost {
		oauthErr(http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthErr(http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != ClientID || secret != ClientSecret {
		oauthErr(http.StatusUnauthorized, "invalid_client", "Client authentication failed")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		oauthErr(http.StatusBadRequest, "unsupported_grant_type", "only authorization_code")
		return
	}
	var g *oauthGrant
	for _, x := range s.grants {
		if x.code == r.PostForm.Get("code") && !x.used {
			g = x
		}
	}
	invalid := func() {
		oauthErr(http.StatusBadRequest, "invalid_grant", "The provided authorization grant is invalid, expired, revoked, does not match the redirection URI used in the authorization request, or was issued to another client.")
	}
	if g == nil {
		invalid()
		return
	}
	if g.authorized {
		if r.PostForm.Get("redirect_uri") != g.redirect {
			invalid()
			return
		}
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			invalid()
			return
		}
	}
	g.used = true
	tok := fmt.Sprintf("fake-oauth-token-%d", s.id())
	s.oauthTokens[tok] = g.user
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok, "token_type": "Bearer", "expires_in": 7200,
		"refresh_token": tok + "-refresh", "scope": "read_user", "created_at": s.now().Unix(),
	})
}

// ---- JSON shapes ----

func (s *Server) userJSON(u forge.User) map[string]any {
	return map[string]any{
		"id": u.ID, "username": u.Username, "name": u.Username, "state": "active", "locked": false,
		"avatar_url": nil, "web_url": s.srv.URL + "/" + u.Username,
	}
}

func (s *Server) projectJSON() map[string]any {
	name := s.projectPath[strings.LastIndex(s.projectPath, "/")+1:]
	return map[string]any{
		"id": s.projectID, "name": name, "path": name, "path_with_namespace": s.projectPath,
		"name_with_namespace": strings.ReplaceAll(s.projectPath, "/", " / "),
		"default_branch":      nullable(s.defaultBranch), "visibility": "private",
		"web_url":          s.srv.URL + "/" + s.projectPath,
		"http_url_to_repo": s.srv.URL + "/" + s.projectPath + ".git",
		"ssh_url_to_repo":  "git@" + strings.TrimPrefix(strings.TrimPrefix(s.srv.URL, "http://"), "https://") + ":" + s.projectPath + ".git",
		"permissions": map[string]any{
			"project_access": map[string]any{"access_level": 30, "notification_level": 3},
			"group_access":   nil,
		},
	}
}

func (s *Server) mrJSON(m forge.MR) map[string]any {
	src := s.projectID
	if m.FromFork {
		src = s.projectID + forkProjectIDOffset
	}
	return map[string]any{
		"id": 10000 + m.IID, "iid": m.IID, "project_id": s.projectID, "title": fmt.Sprintf("MR !%d", m.IID),
		"state": m.State, "source_branch": m.SourceBranch, "target_branch": s.defaultBranch,
		"source_project_id": src, "target_project_id": s.projectID, "sha": m.HeadSHA,
		"draft": false, "work_in_progress": false, "merge_status": "can_be_merged",
		"detailed_merge_status": "mergeable", "web_url": m.WebURL,
		"references": map[string]any{"short": fmt.Sprintf("!%d", m.IID)},
		"author":     s.userJSON(forge.User{ID: 2, Username: "author"}),
	}
}

func (s *Server) eventJSON(e pushEvent) map[string]any {
	action := "pushed to"
	if e.from == zeroSHA {
		action = "pushed new"
	}
	return map[string]any{
		"id": e.id, "project_id": s.projectID, "action_name": action,
		"target_id": nil, "target_iid": nil, "target_type": nil, "target_title": nil,
		"author_id": e.user.ID, "author_username": e.user.Username, "author": s.userJSON(e.user),
		"created_at": e.at.Format(timeFormat), "imported": false, "imported_from": "none",
		"push_data": map[string]any{
			"commit_count": 1, "action": map[bool]string{true: "created", false: "pushed"}[e.from == zeroSHA],
			"ref_type": "branch", "commit_from": map[bool]any{true: nil, false: e.from}[e.from == zeroSHA],
			"commit_to": e.to, "ref": e.branch, "commit_title": "commit " + short(e.to), "ref_count": nil,
		},
	}
}

func (s *Server) memberJSON(u forge.User) map[string]any {
	m := s.userJSON(u)
	m["access_level"] = 30
	m["membership_state"] = "active"
	m["expires_at"] = nil
	m["created_at"] = "2025-01-01T00:00:00.000Z"
	return m
}

func (s *Server) noteJSON(iid int, n *note) map[string]any {
	return map[string]any{
		"id": n.ID, "type": nil, "body": n.Body, "attachment": nil, "author": s.userJSON(n.Author),
		"created_at": n.created.UTC().Format(timeFormat), "updated_at": n.updated.UTC().Format(timeFormat),
		"system": false, "noteable_id": 10000 + iid, "noteable_type": "MergeRequest", "noteable_iid": iid,
		"resolvable": false, "confidential": false, "internal": false, "project_id": s.projectID,
	}
}

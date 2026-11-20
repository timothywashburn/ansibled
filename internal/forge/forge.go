// Package forge defines what ansibled needs from a code-hosting site (GitLab today,
// GitHub later). Everything forge-specific lives behind this interface.
package forge

import (
	"context"
	"errors"
	"net/http"
)

// ErrUnauthorized is returned by HandleWebhook when the webhook secret does not match.
var ErrUnauthorized = errors.New("webhook secret mismatch")

// User on the forge.
type User struct {
	ID       int64
	Username string
}

// MR is a merge request (pull request).
type MR struct {
	IID          int
	SourceBranch string
	HeadSHA      string
	FromFork     bool   // source project differs from this project
	State        string // "opened", "closed", "merged", "locked"
	WebURL       string
}

// EventType of a parsed webhook event.
type EventType string

const (
	EventPush    EventType = "push"    // branch push (tag pushes and deletions are dropped by the adapter)
	EventMR      EventType = "mr"      // merge request opened/reopened/closed/merged
	EventComment EventType = "comment" // comment on a merge request
	EventIgnored EventType = "ignored" // valid but irrelevant (counted in metrics)
)

// MR actions carried by EventMR.
const (
	MROpen   = "open"
	MRReopen = "reopen"
	MRClose  = "close"
	MRMerge  = "merge"
)

// Event is a forge webhook normalised to what ansibled cares about.
type Event struct {
	Type EventType
	// Raw event name as the forge calls it, for metrics (e.g. "Push Hook").
	Name string

	// EventPush
	Branch string
	SHA    string
	Pusher User

	// EventMR
	MR       MR
	MRAction string

	// EventComment
	MRIID  int
	Author User
	Body   string
}

// StatusState is a commit status state.
type StatusState string

const (
	StatusPending  StatusState = "pending"
	StatusRunning  StatusState = "running"
	StatusSuccess  StatusState = "success"
	StatusFailed   StatusState = "failed"
	StatusCanceled StatusState = "canceled"
)

// Status is a commit status (check mark) on one commit.
type Status struct {
	Name        string // "ansibled/<site>"
	State       StatusState
	Description string // <= 140 chars
	TargetURL   string
	Ref         string // branch
}

// Forge is a code-hosting site for one project.
type Forge interface {
	// Name is the forge type, e.g. "gitlab". It is also the webhook path suffix.
	Name() string
	// HandleWebhook verifies the request's secret (ErrUnauthorized on mismatch)
	// and parses the body. It must not call the forge API.
	HandleWebhook(r *http.Request) ([]Event, error)

	DefaultBranch(ctx context.Context) (string, error)
	BranchHead(ctx context.Context, branch string) (string, error)
	OpenMRs(ctx context.Context) ([]MR, error)
	MRsForBranch(ctx context.Context, branch string) ([]MR, error) // open MRs from this project's branch
	GetMR(ctx context.Context, iid int) (MR, error)
	// PusherOf finds who pushed sha to branch. Nil if unknown.
	PusherOf(ctx context.Context, branch, sha string) (*User, error)
	// IsTrusted reports trusted-group membership (cached briefly).
	IsTrusted(ctx context.Context, u User) (bool, error)
	// BotUser is the user the API token belongs to.
	BotUser(ctx context.Context) (User, error)

	SetStatus(ctx context.Context, sha string, s Status) error
	// UpsertMRComment edits the bot's comment containing marker, or creates it.
	UpsertMRComment(ctx context.Context, iid int, marker, body string) error

	// GitAuth returns the HTTPS fetch URL and the value of an HTTP Authorization header.
	GitAuth() (fetchURL, authHeader string)
	// MRRef is the git ref holding an MR's head, e.g. "refs/merge-requests/7/head".
	MRRef(iid int) string

	OAuth() OAuthProvider
}

// OAuthProvider signs users in to the web UI.
type OAuthProvider interface {
	// AuthCodeURL builds the authorize URL. verifier is the PKCE code verifier.
	AuthCodeURL(state, verifier, redirectURL string) string
	// Exchange trades the code for a token and returns the signed-in user.
	Exchange(ctx context.Context, code, verifier, redirectURL string) (User, error)
}

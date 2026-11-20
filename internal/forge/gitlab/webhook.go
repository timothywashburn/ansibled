package gitlab

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/timothywashburn/ansibled/internal/forge"
)

// MaxWebhookBody is the largest webhook body accepted (5 MiB).
const MaxWebhookBody = 5 << 20

// GitLab X-Gitlab-Event values ansibled understands.
const (
	EventPushHook         = "Push Hook"
	EventMergeRequestHook = "Merge Request Hook"
	EventNoteHook         = "Note Hook"
	// EventConfidentialNoteHook is sent for internal notes; treated like Note Hook.
	EventConfidentialNoteHook = "Confidential Note Hook"
)

// HandleWebhook implements forge.Forge. It checks X-Gitlab-Token in constant
// time (forge.ErrUnauthorized if missing or wrong), reads at most 5 MiB and
// parses the body according to X-Gitlab-Event. It never calls the API.
func (c *Client) HandleWebhook(r *http.Request) ([]forge.Event, error) {
	if !c.secretOK(r.Header.Get("X-Gitlab-Token")) {
		return nil, forge.ErrUnauthorized
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxWebhookBody+1))
	if err != nil {
		return nil, fmt.Errorf("gitlab webhook: read body: %w", err)
	}
	if len(body) > MaxWebhookBody {
		return nil, fmt.Errorf("gitlab webhook: body larger than %d bytes", MaxWebhookBody)
	}
	name := r.Header.Get("X-Gitlab-Event")
	var ev forge.Event
	switch name {
	case EventPushHook:
		ev, err = parsePush(body)
	case EventMergeRequestHook:
		ev, err = parseMR(body)
	case EventNoteHook, EventConfidentialNoteHook:
		ev, err = parseNote(body)
	default:
		ev = forge.Event{Type: forge.EventIgnored}
	}
	if err != nil {
		return nil, fmt.Errorf("gitlab webhook %q: %w", name, err)
	}
	ev.Name = name
	return []forge.Event{ev}, nil
}

// secretOK compares the hashes so the comparison does not leak the length.
func (c *Client) secretOK(got string) bool {
	if got == "" {
		return false
	}
	a := sha256.Sum256([]byte(got))
	b := sha256.Sum256([]byte(c.opts.WebhookSecret))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func ignored() forge.Event { return forge.Event{Type: forge.EventIgnored} }

// isZeroSHA reports an all-zero object id (branch deletion), SHA-1 or SHA-256.
func isZeroSHA(s string) bool { return s != "" && strings.Trim(s, "0") == "" }

type pushHook struct {
	ObjectKind   string `json:"object_kind"`
	Ref          string `json:"ref"`
	After        string `json:"after"`
	UserID       int64  `json:"user_id"`
	UserUsername string `json:"user_username"`
}

func parsePush(body []byte) (forge.Event, error) {
	var p pushHook
	if err := json.Unmarshal(body, &p); err != nil {
		return forge.Event{}, err
	}
	if p.ObjectKind == "tag_push" {
		return ignored(), nil
	}
	branch, ok := strings.CutPrefix(p.Ref, "refs/heads/")
	if !ok || branch == "" {
		if p.Ref == "" {
			return forge.Event{}, errors.New("missing ref")
		}
		return ignored(), nil // refs/tags/..., or anything that isn't a branch
	}
	if p.After == "" {
		return forge.Event{}, errors.New("missing after")
	}
	if isZeroSHA(p.After) {
		return ignored(), nil
	}
	return forge.Event{
		Type:   forge.EventPush,
		Branch: branch,
		SHA:    p.After,
		Pusher: forge.User{ID: p.UserID, Username: p.UserUsername},
	}, nil
}

// hookMR is the merge request as webhooks describe it (MR hook
// object_attributes, Note hook merge_request).
type hookMR struct {
	IID             int    `json:"iid"`
	Action          string `json:"action"`
	State           string `json:"state"`
	SourceBranch    string `json:"source_branch"`
	SourceProjectID int64  `json:"source_project_id"`
	TargetProjectID int64  `json:"target_project_id"`
	URL             string `json:"url"`
	LastCommit      struct {
		ID string `json:"id"`
	} `json:"last_commit"`
}

func (m hookMR) forge() forge.MR {
	return forge.MR{
		IID:          m.IID,
		SourceBranch: m.SourceBranch,
		HeadSHA:      m.LastCommit.ID,
		FromFork:     m.SourceProjectID != m.TargetProjectID,
		State:        m.State,
		WebURL:       m.URL,
	}
}

func parseMR(body []byte) (forge.Event, error) {
	var p struct {
		ObjectAttributes *hookMR `json:"object_attributes"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return forge.Event{}, err
	}
	a := p.ObjectAttributes
	if a == nil || a.IID <= 0 {
		return forge.Event{}, errors.New("missing object_attributes.iid")
	}
	switch a.Action {
	case forge.MROpen, forge.MRReopen, forge.MRClose, forge.MRMerge:
	default: // update, approved, unapproved, approval, unapproval, ...
		return ignored(), nil
	}
	return forge.Event{Type: forge.EventMR, MR: a.forge(), MRAction: a.Action}, nil
}

func parseNote(body []byte) (forge.Event, error) {
	var p struct {
		User             apiUser `json:"user"`
		ObjectAttributes struct {
			Note         string `json:"note"`
			NoteableType string `json:"noteable_type"`
			System       bool   `json:"system"`
		} `json:"object_attributes"`
		MergeRequest *hookMR `json:"merge_request"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return forge.Event{}, err
	}
	if p.ObjectAttributes.NoteableType != "MergeRequest" || p.ObjectAttributes.System {
		return ignored(), nil
	}
	if p.MergeRequest == nil || p.MergeRequest.IID <= 0 {
		return forge.Event{}, errors.New("merge request note without merge_request.iid")
	}
	return forge.Event{
		Type:   forge.EventComment,
		MRIID:  p.MergeRequest.IID,
		MR:     p.MergeRequest.forge(), // informational; the head must come from the API
		Author: p.User.forge(),
		Body:   p.ObjectAttributes.Note,
	}, nil
}

package gitlabtest

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/timothywashburn/ansibled/internal/forge"
)

// hookProject describes the project a webhook comes from.
type hookProject struct {
	id            int64
	path          string
	webBase       string // GitLab base URL
	defaultBranch string
}

var defaultHookProject = hookProject{
	id:            DefaultProjectID,
	path:          DefaultProjectPath,
	webBase:       "https://gitlab.example.org",
	defaultBranch: DefaultBranch,
}

func (s *Server) hookProject() hookProject {
	s.mu.Lock()
	defer s.mu.Unlock()
	return hookProject{id: s.projectID, path: s.projectPath, webBase: s.srv.URL, defaultBranch: s.defaultBranch}
}

// PushHook builds a signed "Push Hook" request (POST /webhook/gitlab) for
// project myorg/infra. sha all zeros is a branch deletion.
func PushHook(secret, branch, sha string, u forge.User) *http.Request {
	return defaultHookProject.push(secret, branch, sha, u)
}

// MRHook builds a signed "Merge Request Hook" request with the given action
// (open, reopen, close, merge, update, approved, ...).
func MRHook(secret string, mr forge.MR, action string) *http.Request {
	return defaultHookProject.mr(secret, mr, action)
}

// NoteHook builds a signed "Note Hook" request for a comment by u on MR iid.
func NoteHook(secret string, iid int, u forge.User, body string) *http.Request {
	return defaultHookProject.note(secret, iid, u, body)
}

// PushHook is like the package-level PushHook but describes this server's project.
func (s *Server) PushHook(secret, branch, sha string, u forge.User) *http.Request {
	return s.hookProject().push(secret, branch, sha, u)
}

// MRHook is like the package-level MRHook but describes this server's project.
func (s *Server) MRHook(secret string, mr forge.MR, action string) *http.Request {
	return s.hookProject().mr(secret, mr, action)
}

// NoteHook is like the package-level NoteHook but describes this server's
// project (and the MR's details, if it was added with AddMR).
func (s *Server) NoteHook(secret string, iid int, u forge.User, body string) *http.Request {
	s.mu.Lock()
	mr, _ := s.mr(iid)
	s.mu.Unlock()
	return s.hookProject().noteFor(secret, iid, mr, u, body)
}

// WebhookRequest builds a POST /webhook/gitlab request with the given
// X-Gitlab-Event, X-Gitlab-Token (omitted if empty) and JSON body.
func WebhookRequest(event, secret string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://"+webhookHost+webhookPath, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "GitLab/17.5.0")
	r.Header.Set("X-Gitlab-Event", event)
	r.Header.Set("X-Gitlab-Instance", defaultHookProject.webBase)
	r.Header.Set("X-Gitlab-Webhook-UUID", "5ba7c4a2-7d6e-4b1f-9d0e-8c3b2a1f0e9d")
	r.Header.Set("X-Gitlab-Event-UUID", fmt.Sprintf("%x", sha1.Sum(body))[:32])
	if secret != "" {
		r.Header.Set("X-Gitlab-Token", secret)
	}
	return r
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// fakeParent derives a stable "before" sha from sha.
func fakeParent(sha string) string {
	h := sha1.Sum([]byte("parent of " + sha))
	return hex.EncodeToString(h[:])
}

func (p hookProject) projectJSON(id int64, path string) map[string]any {
	name := path[strings.LastIndex(path, "/")+1:]
	ns := strings.TrimSuffix(path, "/"+name)
	return map[string]any{
		"id": id, "name": name, "description": "", "web_url": p.webBase + "/" + path,
		"avatar_url": nil, "git_ssh_url": "git@gitlab.example.org:" + path + ".git",
		"git_http_url": p.webBase + "/" + path + ".git", "namespace": ns, "visibility_level": 0,
		"path_with_namespace": path, "default_branch": p.defaultBranch, "ci_config_path": "",
		"homepage": p.webBase + "/" + path, "url": "git@gitlab.example.org:" + path + ".git",
		"ssh_url": "git@gitlab.example.org:" + path + ".git", "http_url": p.webBase + "/" + path + ".git",
	}
}

func hookUser(p hookProject, u forge.User) map[string]any {
	return map[string]any{
		"id": u.ID, "name": u.Username, "username": u.Username,
		"avatar_url": p.webBase + "/uploads/-/system/user/avatar/" + fmt.Sprint(u.ID) + "/avatar.png",
		"email":      "[REDACTED]",
	}
}

func (p hookProject) push(secret, branch, sha string, u forge.User) *http.Request {
	before := fakeParent(sha)
	var commits []map[string]any
	total := 0
	if strings.Trim(sha, "0") != "" {
		total = 1
		commits = append(commits, map[string]any{
			"id": sha, "message": "update " + branch + "\n", "title": "update " + branch,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"url":       p.webBase + "/" + p.path + "/-/commit/" + sha,
			"author":    map[string]any{"name": u.Username, "email": u.Username + "@example.org"},
			"added":     []string{}, "modified": []string{"site.yml"}, "removed": []string{},
		})
	}
	if commits == nil {
		commits = []map[string]any{}
	}
	checkout := any(sha)
	if total == 0 {
		checkout = nil
	}
	body := map[string]any{
		"object_kind": "push", "event_name": "push", "before": before, "after": sha,
		"ref": "refs/heads/" + branch, "ref_protected": branch == p.defaultBranch,
		"checkout_sha": checkout, "message": nil,
		"user_id": u.ID, "user_name": u.Username, "user_username": u.Username,
		"user_email": "", "user_avatar": nil,
		"project_id": p.id, "project": p.projectJSON(p.id, p.path),
		"commits": commits, "total_commits_count": total,
		"push_options": map[string]any{},
		"repository": map[string]any{
			"name": p.path[strings.LastIndex(p.path, "/")+1:], "url": "git@gitlab.example.org:" + p.path + ".git",
			"description": "", "homepage": p.webBase + "/" + p.path,
			"git_http_url": p.webBase + "/" + p.path + ".git", "git_ssh_url": "git@gitlab.example.org:" + p.path + ".git",
			"visibility_level": 0,
		},
	}
	return WebhookRequest("Push Hook", secret, mustJSON(body))
}

func mrState(mr forge.MR, action string) string {
	switch action {
	case forge.MRClose:
		return "closed"
	case forge.MRMerge:
		return "merged"
	case forge.MROpen, forge.MRReopen:
		return "opened"
	}
	if mr.State != "" {
		return mr.State
	}
	return "opened"
}

// mrAttrs is the merge request object shared by MR hooks
// (object_attributes) and note hooks (merge_request).
func (p hookProject) mrAttrs(mr forge.MR, state string) map[string]any {
	src := p.id
	if mr.FromFork {
		src = p.id + forkProjectIDOffset
	}
	url := mr.WebURL
	if url == "" {
		url = fmt.Sprintf("%s/%s/-/merge_requests/%d", p.webBase, p.path, mr.IID)
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	return map[string]any{
		"id": 10000 + mr.IID, "iid": mr.IID, "target_branch": p.defaultBranch, "source_branch": mr.SourceBranch,
		"source_project_id": src, "target_project_id": p.id, "author_id": 2, "assignee_ids": []int{},
		"title": fmt.Sprintf("MR !%d", mr.IID), "created_at": now, "updated_at": now,
		"state": state, "state_id": map[string]int{"opened": 1, "closed": 2, "merged": 3, "locked": 4}[state],
		"merge_status": "can_be_merged", "detailed_merge_status": "mergeable", "draft": false,
		"work_in_progress": false, "description": "", "url": url,
		"source": p.projectJSON(src, map[bool]string{true: "contributor/infra", false: p.path}[mr.FromFork]),
		"target": p.projectJSON(p.id, p.path),
		"last_commit": map[string]any{
			"id": mr.HeadSHA, "message": "change\n", "title": "change", "timestamp": time.Now().UTC().Format(time.RFC3339),
			"url":    p.webBase + "/" + p.path + "/-/commit/" + mr.HeadSHA,
			"author": map[string]any{"name": "author", "email": "author@example.org"},
		},
		"labels": []any{}, "blocking_discussions_resolved": true,
	}
}

func (p hookProject) mr(secret string, mr forge.MR, action string) *http.Request {
	attrs := p.mrAttrs(mr, mrState(mr, action))
	attrs["action"] = action
	if action == "update" {
		attrs["oldrev"] = fakeParent(mr.HeadSHA)
	}
	body := map[string]any{
		"object_kind": "merge_request", "event_type": "merge_request",
		"user":    hookUser(p, forge.User{ID: 2, Username: "author"}),
		"project": p.projectJSON(p.id, p.path), "object_attributes": attrs,
		"labels": []any{}, "changes": map[string]any{},
		"repository": map[string]any{"name": p.path[strings.LastIndex(p.path, "/")+1:], "url": "git@gitlab.example.org:" + p.path + ".git", "homepage": p.webBase + "/" + p.path},
	}
	return WebhookRequest("Merge Request Hook", secret, mustJSON(body))
}

func (p hookProject) note(secret string, iid int, u forge.User, text string) *http.Request {
	return p.noteFor(secret, iid, forge.MR{}, u, text)
}

func (p hookProject) noteFor(secret string, iid int, mr forge.MR, u forge.User, text string) *http.Request {
	mr.IID = iid
	if mr.SourceBranch == "" {
		mr.SourceBranch = fmt.Sprintf("mr-%d", iid)
	}
	if mr.HeadSHA == "" {
		mr.HeadSHA = fakeParent(fmt.Sprint(iid))
	}
	state := mr.State
	if state == "" {
		state = "opened"
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	noteURL := fmt.Sprintf("%s/%s/-/merge_requests/%d#note_%d", p.webBase, p.path, iid, 5000+iid)
	body := map[string]any{
		"object_kind": "note", "event_type": "note",
		"user": hookUser(p, u), "project_id": p.id, "project": p.projectJSON(p.id, p.path),
		"object_attributes": map[string]any{
			"id": 5000 + iid, "note": text, "noteable_type": "MergeRequest", "author_id": u.ID,
			"created_at": now, "updated_at": now, "project_id": p.id, "attachment": nil,
			"line_code": nil, "commit_id": "", "noteable_id": 10000 + iid, "system": false,
			"st_diff": nil, "url": noteURL, "type": nil, "internal": false, "action": "create",
		},
		"repository":    map[string]any{"name": p.path[strings.LastIndex(p.path, "/")+1:], "url": "git@gitlab.example.org:" + p.path + ".git", "homepage": p.webBase + "/" + p.path},
		"merge_request": p.mrAttrs(mr, state),
	}
	return WebhookRequest("Note Hook", secret, mustJSON(body))
}

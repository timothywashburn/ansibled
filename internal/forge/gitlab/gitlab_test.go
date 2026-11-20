package gitlab

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/forge/gitlab/gitlabtest"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var (
	alice = forge.User{ID: 42, Username: "alice"}
	bob   = forge.User{ID: 43, Username: "bob"}
	ctx   = context.Background()
)

func sha(n int) string { return fmt.Sprintf("%040x", n) }

func newTest(t *testing.T, mod ...func(*Options)) (*Client, *gitlabtest.Server) {
	t.Helper()
	srv := gitlabtest.New()
	t.Cleanup(srv.Close)
	o := Options{
		BaseURL: srv.URL() + "/", Project: "myorg/infra", Token: gitlabtest.Token,
		WebhookSecret: testSecret, TrustedGroup: "myorg/sysadmins",
		HTTPClient: srv.Client(), Logger: discardLogger(),
	}
	for _, m := range mod {
		m(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	c.retryDelay = time.Millisecond
	return c, srv
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("empty options accepted")
	} else {
		for _, f := range []string{"BaseURL", "Project", "Token", "WebhookSecret", "TrustedGroup"} {
			if !strings.Contains(err.Error(), f) {
				t.Errorf("error %q does not mention %s", err, f)
			}
		}
	}
	if _, err := New(Options{BaseURL: "gitlab.example.org", Project: "p", Token: "t", WebhookSecret: "s", TrustedGroup: "g"}); err == nil {
		t.Error("relative BaseURL accepted")
	}
}

func TestBasics(t *testing.T) {
	c, _ := newTest(t)
	if c.Name() != "gitlab" {
		t.Errorf("Name = %q", c.Name())
	}
	if got := c.MRRef(7); got != "refs/merge-requests/7/head" {
		t.Errorf("MRRef = %q", got)
	}
	u, h := c.GitAuth()
	if want := strings.TrimRight(c.baseURL, "/") + "/myorg/infra.git"; u != want {
		t.Errorf("fetch URL %q, want %q", u, want)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+gitlabtest.Token)); h != want {
		t.Errorf("auth header %q", h)
	}
	c2, _ := newTest(t, func(o *Options) { o.CloneURL = "http://git.invalid/repo.git" })
	if u, _ := c2.GitAuth(); u != "http://git.invalid/repo.git" {
		t.Errorf("CloneURL override: %q", u)
	}
}

func TestURLEncodingAndProject(t *testing.T) {
	c, srv := newTest(t)
	srv.SetProject("myorg/infra", 17, "trunk")
	srv.SetBranch("feature/vlan-42", sha(1))

	b, err := c.DefaultBranch(ctx)
	if err != nil || b != "trunk" {
		t.Fatalf("DefaultBranch = %q, %v", b, err)
	}
	if _, err := c.DefaultBranch(ctx); err != nil {
		t.Fatal(err)
	}
	if n := srv.CountRequests("GET /api/v4/projects/myorg%2Finfra"); n != 1 {
		t.Errorf("project fetched %d times, want 1 (cached): %v", n, srv.Requests())
	}
	h, err := c.BranchHead(ctx, "feature/vlan-42")
	if err != nil || h != sha(1) {
		t.Fatalf("BranchHead = %q, %v", h, err)
	}
	if !slices.Contains(srv.Requests(), "GET /api/v4/projects/myorg%2Finfra/repository/branches/feature%2Fvlan-42") {
		t.Errorf("branch path not encoded: %v", srv.Requests())
	}
	if _, err := c.BranchHead(ctx, "nope"); !IsNotFound(err) {
		t.Errorf("missing branch: %v", err)
	}
}

func TestNumericProject(t *testing.T) {
	c, srv := newTest(t, func(o *Options) { o.Project = "17" })
	srv.SetProject("myorg/infra", 17, "main")
	u, _ := c.GitAuth() // resolves the path lazily
	if want := c.baseURL + "/myorg/infra.git"; u != want {
		t.Errorf("fetch URL %q, want %q", u, want)
	}
	if b, err := c.DefaultBranch(ctx); err != nil || b != "main" {
		t.Errorf("DefaultBranch = %q, %v", b, err)
	}
	if n := srv.CountRequests("GET /api/v4/projects/17"); n != 1 {
		t.Errorf("project fetched %d times", n)
	}
}

func TestMRs(t *testing.T) {
	c, srv := newTest(t)
	for i := 1; i <= 230; i++ {
		srv.AddMR(forge.MR{IID: i, SourceBranch: fmt.Sprintf("b%d", i), HeadSHA: sha(i)})
	}
	srv.AddMR(forge.MR{IID: 300, SourceBranch: "topic", HeadSHA: sha(300)})
	srv.AddMR(forge.MR{IID: 301, SourceBranch: "topic", HeadSHA: sha(301), FromFork: true})
	srv.AddMR(forge.MR{IID: 302, SourceBranch: "topic", HeadSHA: sha(302), State: "merged"})

	open, err := c.OpenMRs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 232 {
		t.Errorf("OpenMRs: %d, want 232", len(open))
	}
	if n := srv.CountRequests("GET /api/v4/projects/myorg%2Finfra/merge_requests"); n != 3 {
		t.Errorf("pages fetched: %d, want 3", n)
	}
	fork := 0
	for _, m := range open {
		if m.FromFork {
			fork++
		}
	}
	if fork != 1 {
		t.Errorf("fork MRs in OpenMRs: %d", fork)
	}

	mine, err := c.MRsForBranch(ctx, "topic")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].IID != 300 || mine[0].HeadSHA != sha(300) || mine[0].FromFork || mine[0].State != "opened" {
		t.Errorf("MRsForBranch = %+v", mine)
	}

	m, err := c.GetMR(ctx, 301)
	if err != nil {
		t.Fatal(err)
	}
	if m.IID != 301 || m.HeadSHA != sha(301) || !m.FromFork || m.SourceBranch != "topic" || m.WebURL == "" {
		t.Errorf("GetMR = %+v", m)
	}
	srv.SetMRState(301, "closed")
	if m, _ := c.GetMR(ctx, 301); m.State != "closed" {
		t.Errorf("state after SetMRState: %q", m.State)
	}
	if _, err := c.GetMR(ctx, 999); !IsNotFound(err) {
		t.Errorf("missing MR: %v", err)
	}
}

func TestPusherOf(t *testing.T) {
	c, srv := newTest(t)
	now := time.Now()
	srv.AddPushEventAt("topic", sha(1), alice, now.Add(-2*time.Hour))
	srv.AddPushEventAt("other", sha(2), bob, now.Add(-time.Hour)) // same sha elsewhere doesn't matter
	srv.AddPushEventAt("topic", sha(2), bob, now.Add(-30*time.Minute))
	srv.AddPushEventAt("old", sha(3), alice, now.Add(-40*24*time.Hour))

	u, err := c.PusherOf(ctx, "topic", sha(2))
	if err != nil || u == nil || *u != bob {
		t.Errorf("PusherOf(topic, 2) = %v, %v", u, err)
	}
	u, err = c.PusherOf(ctx, "topic", strings.ToUpper(sha(1)))
	if err != nil || u == nil || *u != alice {
		t.Errorf("PusherOf(topic, 1) = %v, %v", u, err)
	}
	if u, err := c.PusherOf(ctx, "topic", sha(9)); err != nil || u != nil {
		t.Errorf("unknown sha: %v, %v", u, err)
	}
	if u, err := c.PusherOf(ctx, "old", sha(3)); err != nil || u != nil {
		t.Errorf("event older than 30 days: %v, %v", u, err)
	}
	for _, r := range srv.Requests() {
		if strings.Contains(r, "/events") && (!strings.Contains(r, "action=pushed") || !strings.Contains(r, "after=")) {
			t.Errorf("events query %q", r)
		}
	}
}

func TestPusherOfPageBound(t *testing.T) {
	c, srv := newTest(t)
	srv.AddPushEvent("topic", sha(1), alice)
	for i := 0; i < 600; i++ {
		srv.AddPushEvent(fmt.Sprintf("b%d", i), sha(1000+i), bob)
	}
	if u, err := c.PusherOf(ctx, "topic", sha(1)); err != nil || u != nil {
		t.Errorf("beyond page bound: %v, %v", u, err)
	}
	if n := srv.CountRequests("GET /api/v4/projects/myorg%2Finfra/events"); n != pushMaxPages {
		t.Errorf("event pages fetched: %d, want %d", n, pushMaxPages)
	}
}

func TestIsTrustedCache(t *testing.T) {
	c, srv := newTest(t)
	now := time.Now()
	var mu sync.Mutex
	c.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	srv.AddGroupMember("myorg/sysadmins", alice)
	srv.AddGroupMember("myorg/developers", bob)
	calls := func() int { return srv.CountRequests("GET /api/v4/groups/myorg%2Fsysadmins/members/all/") }

	if ok, err := c.IsTrusted(ctx, alice); err != nil || !ok {
		t.Fatalf("alice: %v, %v", ok, err)
	}
	if ok, err := c.IsTrusted(ctx, bob); err != nil || ok {
		t.Fatalf("bob: %v, %v", ok, err)
	}
	if calls() != 2 {
		t.Fatalf("calls = %d", calls())
	}
	// Cached, positive and negative.
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := c.IsTrusted(ctx, alice); !ok {
				t.Error("alice not trusted from cache")
			}
			if ok, _ := c.IsTrusted(ctx, bob); ok {
				t.Error("bob trusted from cache")
			}
		}()
	}
	wg.Wait()
	if calls() != 2 {
		t.Errorf("cache missed: calls = %d", calls())
	}

	// After 60 s, membership is looked up again.
	srv.RemoveGroupMember("myorg/sysadmins", alice)
	advance(61 * time.Second)
	if ok, err := c.IsTrusted(ctx, alice); err != nil || ok {
		t.Errorf("alice after removal: %v, %v", ok, err)
	}
	if calls() != 3 {
		t.Errorf("calls = %d, want 3", calls())
	}

	// Errors are returned and not cached (two 500s defeat the single retry).
	advance(61 * time.Second)
	srv.AddGroupMember("myorg/sysadmins", alice)
	srv.FailNext(2, http.StatusInternalServerError)
	if _, err := c.IsTrusted(ctx, alice); err == nil {
		t.Error("expected error")
	}
	if ok, err := c.IsTrusted(ctx, alice); err != nil || !ok {
		t.Errorf("after error: %v, %v", ok, err)
	}

	if ok, err := c.IsTrusted(ctx, forge.User{}); ok || err != nil {
		t.Errorf("zero user: %v, %v", ok, err)
	}
}

func TestMemberActive(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		m    apiMember
		want bool
	}{
		{apiMember{}, true},
		{apiMember{State: "active", MembershipState: "active", AccessLevel: 30}, true},
		{apiMember{State: "blocked"}, false},
		{apiMember{MembershipState: "awaiting"}, false},
		{apiMember{AccessLevel: 5}, false},
		{apiMember{ExpiresAt: "2026-11-19"}, false},
		{apiMember{ExpiresAt: "2026-11-20"}, true},
	} {
		if got := tc.m.active(now); got != tc.want {
			t.Errorf("%+v: %v, want %v", tc.m, got, tc.want)
		}
	}
}

func TestBotUser(t *testing.T) {
	c, srv := newTest(t)
	bot := forge.User{ID: 555, Username: "project_17_bot_0123abcd"}
	srv.SetBotUser(bot)
	for range 3 {
		if u, err := c.BotUser(ctx); err != nil || u != bot {
			t.Fatalf("BotUser = %v, %v", u, err)
		}
	}
	if n := srv.CountRequests("GET /api/v4/user"); n != 1 {
		t.Errorf("GET /user %d times", n)
	}
}

func TestSetStatus(t *testing.T) {
	c, srv := newTest(t)
	long := strings.Repeat("é", 200)
	st := forge.Status{Name: "ansibled/site1", State: forge.StatusRunning, Description: long, TargetURL: "http://ansibled.invalid/runs/1", Ref: "topic"}
	if err := c.SetStatus(ctx, sha(1), st); err != nil {
		t.Fatal(err)
	}
	// Same state again: GitLab says 400 "Cannot transition", which is fine.
	if err := c.SetStatus(ctx, sha(1), st); err != nil {
		t.Fatalf("repeat running: %v", err)
	}
	st.State, st.Description = forge.StatusSuccess, "would change 4 tasks on 2 hosts"
	if err := c.SetStatus(ctx, sha(1), st); err != nil {
		t.Fatal(err)
	}
	got := srv.Statuses()
	if len(got) != 2 {
		t.Fatalf("statuses = %+v", got)
	}
	want0 := gitlabtest.RecordedStatus{SHA: sha(1), Name: "ansibled/site1", State: forge.StatusRunning, Description: strings.Repeat("é", 140), TargetURL: st.TargetURL, Ref: "topic"}
	if got[0] != want0 {
		t.Errorf("status 0 = %+v", got[0])
	}
	if got[1].State != forge.StatusSuccess || got[1].Description != "would change 4 tasks on 2 hosts" {
		t.Errorf("status 1 = %+v", got[1])
	}
	if !slices.Contains(srv.Requests(), "POST /api/v4/projects/myorg%2Finfra/statuses/"+sha(1)) {
		t.Errorf("requests: %v", srv.Requests())
	}

	// Other 400s are errors; POSTs are not retried.
	if err := c.SetStatus(ctx, sha(2), forge.Status{Name: "x", State: "bogus"}); err == nil {
		t.Error("invalid state accepted")
	}
	srv.FailNext(1, http.StatusBadGateway)
	if err := c.SetStatus(ctx, sha(3), forge.Status{Name: "x", State: forge.StatusPending}); err == nil {
		t.Error("502 not reported")
	}
	if len(srv.Statuses()) != 2 {
		t.Errorf("POST was retried: %+v", srv.Statuses())
	}
}

func TestUpsertMRComment(t *testing.T) {
	c, srv := newTest(t)
	const marker = "<!-- ansibled:site=site1 -->"
	srv.AddMR(forge.MR{IID: 7, SourceBranch: "topic", HeadSHA: sha(1)})
	// Someone quoting the marker doesn't make it ours.
	srv.AddNote(7, alice, "look: "+marker)

	if err := c.UpsertMRComment(ctx, 7, marker, "### queued"); err != nil {
		t.Fatal(err)
	}
	notes := srv.Notes(7)
	if len(notes) != 2 || notes[1].Body != marker+"\n### queued" || notes[1].Author.ID != gitlabtest.DefaultBotID {
		t.Fatalf("after create: %+v", notes)
	}
	ours := notes[1].ID

	// Edit in place via the cached id (no listing).
	before := srv.CountRequests("GET /api/v4/projects/myorg%2Finfra/merge_requests/7/notes")
	if err := c.UpsertMRComment(ctx, 7, marker, marker+"\n### passed"); err != nil {
		t.Fatal(err)
	}
	notes = srv.Notes(7)
	if len(notes) != 2 || notes[1].ID != ours || notes[1].Body != marker+"\n### passed" || notes[1].Edits != 1 {
		t.Fatalf("after edit: %+v", notes)
	}
	if after := srv.CountRequests("GET /api/v4/projects/myorg%2Finfra/merge_requests/7/notes"); after != before {
		t.Errorf("listed notes despite cache")
	}

	// A fresh client (restart) finds the note by searching.
	c2, err := New(c.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.UpsertMRComment(ctx, 7, marker, "### failed"); err != nil {
		t.Fatal(err)
	}
	notes = srv.Notes(7)
	if len(notes) != 2 || notes[1].ID != ours || notes[1].Edits != 2 {
		t.Fatalf("after restart edit: %+v", notes)
	}

	// The cached note was deleted: fall back to search, then create.
	srv.DeleteNote(7, ours)
	if err := c.UpsertMRComment(ctx, 7, marker, "### again"); err != nil {
		t.Fatal(err)
	}
	notes = srv.Notes(7)
	if len(notes) != 2 || notes[1].ID == ours || notes[1].Body != marker+"\n### again" || notes[1].Edits != 0 {
		t.Fatalf("after delete: %+v", notes)
	}
	// And the new one is cached.
	if err := c.UpsertMRComment(ctx, 7, marker, "### and again"); err != nil {
		t.Fatal(err)
	}
	if notes = srv.Notes(7); len(notes) != 2 || notes[1].Edits != 1 {
		t.Fatalf("edit of recreated note: %+v", notes)
	}

	// Another site's marker is a different comment.
	if err := c.UpsertMRComment(ctx, 7, "<!-- ansibled:site=site2 -->", "### other"); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Notes(7)); n != 3 {
		t.Errorf("notes = %d, want 3", n)
	}
}

func TestUpsertMRCommentPaginatedSearch(t *testing.T) {
	c, srv := newTest(t)
	const marker = "<!-- ansibled:site=site1 -->"
	srv.AddMR(forge.MR{IID: 3, SourceBranch: "topic", HeadSHA: sha(1)})
	for i := range 150 {
		srv.AddNote(3, alice, fmt.Sprintf("comment %d", i))
	}
	id := srv.AddNote(3, forge.User{ID: gitlabtest.DefaultBotID, Username: gitlabtest.DefaultBotUsername}, marker+"\nold")
	if err := c.UpsertMRComment(ctx, 3, marker, "new"); err != nil {
		t.Fatal(err)
	}
	notes := srv.Notes(3)
	if len(notes) != 151 || notes[150].ID != id || notes[150].Body != marker+"\nnew" {
		t.Errorf("last note %+v (of %d)", notes[len(notes)-1], len(notes))
	}
	if n := srv.CountRequests("GET /api/v4/projects/myorg%2Finfra/merge_requests/3/notes"); n != 2 {
		t.Errorf("note pages: %d", n)
	}
	for _, r := range srv.Requests() {
		if strings.Contains(r, "/notes?") && !strings.Contains(r, "sort=asc") {
			t.Errorf("notes listed without sort=asc: %s", r)
		}
	}
}

func TestRetry(t *testing.T) {
	c, srv := newTest(t)
	srv.SetBranch("main", sha(1))
	srv.FailNext(1, http.StatusBadGateway)
	if h, err := c.BranchHead(ctx, "main"); err != nil || h != sha(1) {
		t.Errorf("after one 502: %q, %v", h, err)
	}
	srv.FailNext(2, http.StatusServiceUnavailable)
	_, err := c.BranchHead(ctx, "main")
	if err == nil {
		t.Fatal("two 503s succeeded")
	}
	msg := err.Error()
	for _, want := range []string{"GET", "/projects/myorg%2Finfra/repository/branches/main", "503"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	// 4xx is not retried.
	before := len(srv.Requests())
	_, _ = c.BranchHead(ctx, "missing")
	if n := len(srv.Requests()) - before; n != 1 {
		t.Errorf("404 tried %d times", n)
	}
}

func TestErrorsHideToken(t *testing.T) {
	c, _ := newTest(t, func(o *Options) { o.Token = "glpat-very-secret" })
	_, err := c.BotUser(ctx)
	if err == nil {
		t.Fatal("bad token accepted")
	}
	if strings.Contains(err.Error(), "glpat-very-secret") || !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q", err)
	}
	// Network errors too.
	c2, _ := newTest(t, func(o *Options) { o.Token = "glpat-very-secret"; o.BaseURL = "http://127.0.0.1:1" })
	if _, err := c2.BotUser(ctx); err == nil || strings.Contains(err.Error(), "glpat-very-secret") {
		t.Errorf("network error %v", err)
	}
}

func TestCheck(t *testing.T) {
	c, srv := newTest(t)
	srv.AddGroupMember("myorg/sysadmins", alice)
	if errs := c.Check(ctx); len(errs) != 0 {
		t.Errorf("Check: %v", errs)
	}

	c2, srv2 := newTest(t, func(o *Options) { o.Token = "wrong" })
	srv2.AddGroupMember("myorg/sysadmins", alice)
	if errs := c2.Check(ctx); len(errs) != 3 {
		t.Errorf("Check with bad token: %d errors: %v", len(errs), errs)
	}

	c3, _ := newTest(t, func(o *Options) { o.Project = "myorg/missing"; o.TrustedGroup = "myorg/nobody" })
	errs := c3.Check(ctx)
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), "myorg/missing") || !strings.Contains(errs[1].Error(), "myorg/nobody") {
		t.Errorf("Check: %v", errs)
	}

	c4, srv4 := newTest(t)
	srv4.AddGroupMember("myorg/sysadmins", alice)
	srv4.RemoveGroupMember("myorg/sysadmins", alice) // group exists, nobody visible
	if errs := c4.Check(ctx); len(errs) != 1 || !strings.Contains(errs[0].Error(), "no members") {
		t.Errorf("Check empty group: %v", errs)
	}
	if !slices.Contains(srv4.Requests(), "GET /api/v4/groups/myorg%2Fsysadmins/members/all?per_page=1") {
		t.Errorf("requests: %v", srv4.Requests())
	}
}

func TestWebLinks(t *testing.T) {
	c, _ := newTest(t)
	if got, want := c.CommitURL("abc"), c.baseURL+"/myorg/infra/-/commit/abc"; got != want {
		t.Errorf("CommitURL = %q, want %q", got, want)
	}
	if got, want := c.MRURL(7), c.baseURL+"/myorg/infra/-/merge_requests/7"; got != want {
		t.Errorf("MRURL = %q, want %q", got, want)
	}
	n, srv := newTest(t, func(o *Options) { o.Project = "17" })
	srv.SetProject("myorg/infra", 17, "main")
	if u := n.CommitURL("abc"); u != "" {
		t.Errorf("numeric project before lookup: %q", u)
	}
	if _, err := n.DefaultBranch(ctx); err != nil {
		t.Fatal(err)
	}
	if u := n.MRURL(3); !strings.HasSuffix(u, "/-/merge_requests/3") {
		t.Errorf("numeric project after lookup: %q", u)
	}
}

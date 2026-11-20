package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/config"
	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/forge/gitlab"
	"github.com/timothywashburn/ansibled/internal/forge/gitlab/gitlabtest"
	"github.com/timothywashburn/ansibled/internal/metrics"
	"github.com/timothywashburn/ansibled/internal/model"
	"github.com/timothywashburn/ansibled/internal/queue"
	"github.com/timothywashburn/ansibled/internal/report"
	"github.com/timothywashburn/ansibled/internal/store"
)

const (
	secret = "hook-secret"
	group  = "myorg/sysadmins"
	shaA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaM   = "cccccccccccccccccccccccccccccccccccccccc"
)

var (
	alice = forge.User{ID: 11, Username: "alice"} // trusted
	bob   = forge.User{ID: 12, Username: "bob"}   // not trusted
)

// fakeRunner succeeds with one would-change task, unless gate is set, in which
// case it blocks until gate is closed or ctx ends.
type fakeRunner struct {
	mu   sync.Mutex
	gate chan struct{}
	ran  []string
}

func (f *fakeRunner) Run(ctx context.Context, r *model.Run) (model.State, *model.Summary) {
	f.mu.Lock()
	f.ran = append(f.ran, r.Key+"@"+r.ShortSHA())
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return model.StateCanceled, nil
		}
	}
	return model.StateSuccess, &model.Summary{Playbooks: []model.PlaybookResult{{
		Playbook: "site.yml",
		Hosts:    []model.HostResult{{Host: "web1", OK: 3, Changed: 1, ChangedTasks: []string{"copy motd"}}},
	}}}
}

type fixture struct {
	t      *testing.T
	gl     *gitlabtest.Server
	client *gitlab.Client
	store  *store.Store
	d      *Daemon
	runner *fakeRunner
	cancel context.CancelFunc
	q      *queue.Queue
}

type noFiles struct{}

func (noFiles) DeleteRunFiles(int64) error    { return nil }
func (noFiles) Housekeep(time.Duration) error { return nil }

func newFixture(t *testing.T, gate bool) *fixture {
	t.Helper()
	gl := gitlabtest.New()
	t.Cleanup(gl.Close)
	gl.AddGroupMember(group, alice)
	gl.SetBranch("main", shaM)

	client, err := gitlab.New(gitlab.Options{
		BaseURL: gl.URL(), Project: "myorg/infra", Token: gitlabtest.Token,
		WebhookSecret: secret, TrustedGroup: group, HTTPClient: gl.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{Site: "test", PublicURL: "http://10.0.0.1:9251"}
	cfg.Retention.Duration = time.Hour
	log := slog.New(slog.DiscardHandler)
	runner := &fakeRunner{}
	if gate {
		runner.gate = make(chan struct{})
	}
	d := New(cfg, client, st, metrics.New(), noFiles{}, log)
	q := queue.New(queue.Options{Store: st, Runner: runner, Hooks: d, Describe: report.Describe, Logger: log})
	d.Queue = q
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, gl: gl, client: client, store: st, d: d, runner: runner, cancel: cancel, q: q}
	t.Cleanup(func() {
		if runner.gate != nil {
			select {
			case <-runner.gate:
			default:
				close(runner.gate)
			}
		}
		cancel()
		q.Wait()
	})
	return f
}

// hook delivers a webhook request through the GitLab adapter to the daemon.
func (f *fixture) hook(r *http.Request) {
	f.t.Helper()
	evs, err := f.client.HandleWebhook(r)
	if err != nil {
		f.t.Fatalf("HandleWebhook: %v", err)
	}
	f.d.HandleEvents(context.Background(), evs)
}

func (f *fixture) runs() []*model.Run {
	f.t.Helper()
	rs, err := f.store.ListRuns(context.Background(), 0, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return rs
}

// waitFor polls the store until cond holds.
func (f *fixture) waitFor(what string, cond func([]*model.Run) bool) []*model.Run {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rs := f.runs()
		if cond(rs) {
			return rs
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s; runs: %s", what, describe(rs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func describe(rs []*model.Run) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(r.Key + "@" + r.ShortSHA() + "=" + string(r.State) + " ")
	}
	return b.String()
}

func allFinished(n int) func([]*model.Run) bool {
	return func(rs []*model.Run) bool {
		if len(rs) != n {
			return false
		}
		for _, r := range rs {
			if !r.State.Finished() {
				return false
			}
		}
		return true
	}
}

func TestTrustedPushToMRBranch(t *testing.T) {
	f := newFixture(t, false)
	f.gl.AddMR(forge.MR{IID: 3, SourceBranch: "feat", HeadSHA: shaA, State: "opened"})
	f.hook(f.gl.PushHook(secret, "feat", shaA, alice))

	rs := f.waitFor("mr run", allFinished(1))
	r := rs[0]
	if r.Kind != model.KindMR || r.MRIID != 3 || r.SHA != shaA || r.State != model.StateSuccess || r.TriggeredBy != "alice" {
		t.Fatalf("unexpected run %+v", r)
	}
	var states []string
	for _, s := range f.gl.Statuses() {
		if s.SHA != shaA || s.Name != "ansibled/test" || s.Ref != "feat" {
			t.Errorf("unexpected status %+v", s)
		}
		states = append(states, string(s.State))
	}
	if got := strings.Join(states, ","); got != "pending,running,success" {
		t.Errorf("status sequence = %s", got)
	}
	notes := f.gl.Notes(3)
	if len(notes) != 1 || !strings.Contains(notes[0].Body, "<!-- ansibled:site=test -->") || !strings.Contains(notes[0].Body, "web1") {
		t.Fatalf("MR comment: %+v", notes)
	}
	if notes[0].Edits < 2 {
		t.Errorf("comment should be edited in place, edits=%d", notes[0].Edits)
	}
}

func TestUntrustedPushIgnored(t *testing.T) {
	f := newFixture(t, false)
	f.gl.AddMR(forge.MR{IID: 3, SourceBranch: "feat", HeadSHA: shaA, State: "opened"})
	f.hook(f.gl.PushHook(secret, "feat", shaA, bob))
	f.hook(f.gl.PushHook(secret, "main", shaM, bob))
	time.Sleep(50 * time.Millisecond)
	if rs := f.runs(); len(rs) != 0 {
		t.Fatalf("untrusted pushes queued runs: %s", describe(rs))
	}
	if len(f.gl.Statuses()) != 0 {
		t.Error("statuses posted for untrusted push")
	}
}

func TestPushToMainNeverCommentsOnMR(t *testing.T) {
	f := newFixture(t, false)
	f.gl.AddMR(forge.MR{IID: 4, SourceBranch: "feat", HeadSHA: shaM, State: "opened"}) // fast-forward: same sha
	f.hook(f.gl.PushHook(secret, "main", shaM, alice))
	rs := f.waitFor("main run", allFinished(1))
	if rs[0].Kind != model.KindMain || rs[0].Ref != "main" {
		t.Fatalf("run %+v", rs[0])
	}
	if n := f.gl.Notes(4); len(n) != 0 {
		t.Fatalf("main run commented on MR: %+v", n)
	}
	if len(f.gl.Statuses()) == 0 {
		t.Error("no commit status for main run")
	}
}

func TestPushWithoutMRDoesNothing(t *testing.T) {
	f := newFixture(t, false)
	f.hook(f.gl.PushHook(secret, "lonely", shaA, alice))
	time.Sleep(50 * time.Millisecond)
	if rs := f.runs(); len(rs) != 0 {
		t.Fatalf("runs: %s", describe(rs))
	}
}

func TestMROpenUsesPusher(t *testing.T) {
	f := newFixture(t, false)
	trusted := forge.MR{IID: 5, SourceBranch: "a", HeadSHA: shaA, State: "opened"}
	untrusted := forge.MR{IID: 6, SourceBranch: "b", HeadSHA: shaB, State: "opened"}
	f.gl.AddMR(trusted)
	f.gl.AddMR(untrusted)
	f.gl.AddPushEvent("a", shaA, alice)
	f.gl.AddPushEvent("b", shaB, bob)

	f.hook(f.gl.MRHook(secret, trusted, forge.MROpen))
	f.hook(f.gl.MRHook(secret, untrusted, forge.MROpen))
	rs := f.waitFor("mr open run", allFinished(1))
	if rs[0].MRIID != 5 || rs[0].Trigger != model.TriggerMROpen {
		t.Fatalf("run %+v", rs[0])
	}
}

func TestCommentCommand(t *testing.T) {
	f := newFixture(t, false)
	mr := forge.MR{IID: 7, SourceBranch: "x", HeadSHA: shaA, State: "opened"}
	f.gl.AddMR(mr)
	bot, err := f.client.BotUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.gl.AddGroupMember(group, bot)

	f.hook(f.gl.NoteHook(secret, 7, bob, "/ansibled check"))           // untrusted
	f.hook(f.gl.NoteHook(secret, 7, alice, "looks good"))              // not a command
	f.hook(f.gl.NoteHook(secret, 7, alice, "/ansibled checks please")) // not the command
	f.hook(f.gl.NoteHook(secret, 7, bot, "/ansibled check"))           // ansibled itself
	time.Sleep(50 * time.Millisecond)
	if rs := f.runs(); len(rs) != 0 {
		t.Fatalf("runs: %s", describe(rs))
	}

	f.hook(f.gl.NoteHook(secret, 7, alice, "  /ansibled check\nplease"))
	f.waitFor("first comment run", allFinished(1))
	// Same sha again: a comment always runs (Force), unlike a push.
	f.hook(f.gl.NoteHook(secret, 7, alice, "/ansibled check"))
	rs := f.waitFor("second comment run", allFinished(2))
	for _, r := range rs {
		if r.Trigger != model.TriggerComment || r.TriggeredBy != "alice" || r.SHA != shaA {
			t.Errorf("run %+v", r)
		}
	}
	f.hook(f.gl.PushHook(secret, "x", shaA, alice)) // deduped
	time.Sleep(50 * time.Millisecond)
	if n := len(f.runs()); n != 2 {
		t.Fatalf("push of an already-run sha queued another run (%d)", n)
	}
}

func TestMRCloseCancelsRunning(t *testing.T) {
	f := newFixture(t, true)
	mr := forge.MR{IID: 8, SourceBranch: "y", HeadSHA: shaA, State: "opened"}
	f.gl.AddMR(mr)
	f.hook(f.gl.PushHook(secret, "y", shaA, alice))
	f.waitFor("running", func(rs []*model.Run) bool { return len(rs) == 1 && rs[0].State == model.StateRunning })

	f.hook(f.gl.MRHook(secret, mr, forge.MRMerge))
	rs := f.waitFor("canceled", allFinished(1))
	if rs[0].State != model.StateCanceled || rs[0].Description != "MR merged" {
		t.Fatalf("run %+v", rs[0])
	}
	last := f.gl.Statuses()[len(f.gl.Statuses())-1]
	if last.State != forge.StatusCanceled {
		t.Errorf("last status %+v", last)
	}
}

func TestSupersededCommentNotOverwritten(t *testing.T) {
	f := newFixture(t, true)
	f.gl.AddMR(forge.MR{IID: 9, SourceBranch: "z", HeadSHA: shaB, State: "opened"})
	f.hook(f.gl.PushHook(secret, "z", shaA, alice))
	f.waitFor("running", func(rs []*model.Run) bool { return len(rs) == 1 && rs[0].State == model.StateRunning })
	f.hook(f.gl.PushHook(secret, "z", shaB, alice))
	f.waitFor("superseded", func(rs []*model.Run) bool {
		for _, r := range rs {
			if r.SHA == shaA && r.State == model.StateCanceled {
				return true
			}
		}
		return false
	})
	notes := f.gl.Notes(9)
	if len(notes) != 1 {
		t.Fatalf("notes: %+v", notes)
	}
	if !strings.Contains(notes[0].Body, model.Short(shaB)) || strings.Contains(notes[0].Body, "canceled") {
		t.Fatalf("comment should describe the new run, got:\n%s", notes[0].Body)
	}
}

func TestCatchUp(t *testing.T) {
	f := newFixture(t, false)
	f.gl.AddPushEvent("main", shaM, alice)
	f.gl.AddMR(forge.MR{IID: 10, SourceBranch: "t", HeadSHA: shaA, State: "opened"})
	f.gl.AddPushEvent("t", shaA, alice)
	f.gl.AddMR(forge.MR{IID: 11, SourceBranch: "u", HeadSHA: shaB, State: "opened"})
	f.gl.AddPushEvent("u", shaB, bob)
	f.gl.AddMR(forge.MR{IID: 12, SourceBranch: "v", HeadSHA: shaB, State: "opened", FromFork: true})

	ctx := context.Background()
	if err := f.d.catchUpOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rs := f.waitFor("catch-up runs", allFinished(2))
	keys := map[string]bool{}
	for _, r := range rs {
		keys[r.Key] = true
		if r.Trigger != model.TriggerStartup {
			t.Errorf("trigger %s", r.Trigger)
		}
	}
	if !keys["main"] || !keys["mr:10"] {
		t.Fatalf("keys %v", keys)
	}
	if err := f.d.catchUpOnce(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(f.runs()); n != 2 {
		t.Fatalf("second catch-up queued again (%d runs)", n)
	}
}

func TestCatchUpSkipsUntrustedMainPusher(t *testing.T) {
	f := newFixture(t, false)
	f.gl.AddPushEvent("main", shaM, bob)
	if err := f.d.catchUpOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if rs := f.runs(); len(rs) != 0 {
		t.Fatalf("runs: %s", describe(rs))
	}
}

func TestNightly(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	f.d.Nightly(ctx)
	f.waitFor("nightly 1", allFinished(1))
	f.d.Nightly(ctx) // same sha: still runs (drift detection)
	rs := f.waitFor("nightly 2", allFinished(2))
	for _, r := range rs {
		if r.Kind != model.KindNightly || r.SHA != shaM {
			t.Errorf("run %+v", r)
		}
	}
	if len(f.gl.Statuses()) != 0 {
		t.Error("nightly runs must not post commit statuses")
	}
}

func TestNightlySkippedWhenMainPending(t *testing.T) {
	f := newFixture(t, true)
	f.hook(f.gl.PushHook(secret, "main", shaM, alice))
	f.waitFor("main running", func(rs []*model.Run) bool { return len(rs) == 1 && rs[0].State == model.StateRunning })
	f.d.Nightly(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n := len(f.runs()); n != 1 {
		t.Fatalf("nightly queued although main for the same sha is running (%d runs)", n)
	}
}

func TestBotNeverTrusted(t *testing.T) {
	f := newFixture(t, false)
	bot, err := f.client.BotUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.gl.AddGroupMember(group, bot) // even as a group member
	f.gl.AddMR(forge.MR{IID: 13, SourceBranch: "w", HeadSHA: shaA, State: "opened"})
	f.hook(f.gl.PushHook(secret, "w", shaA, bot))
	f.hook(f.gl.PushHook(secret, "main", shaM, bot))
	time.Sleep(50 * time.Millisecond)
	if rs := f.runs(); len(rs) != 0 {
		t.Fatalf("pushes by the bot queued runs: %s", describe(rs))
	}
}

func TestIsCommand(t *testing.T) {
	for body, want := range map[string]bool{
		"/ansibled check":         true,
		"  /ansibled check  ":     true,
		"/ansibled check\nthanks": true,
		"/ansibled check please":  true,
		"/ansibled checkout":      false,
		"please /ansibled check":  false,
		"/ansibled":               false,
		"":                        false,
	} {
		if got := IsCommand(body); got != want {
			t.Errorf("IsCommand(%q) = %v", body, got)
		}
	}
}

package queue

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/model"
	"github.com/timothywashburn/ansibled/internal/store"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
)

// fakeRunner blocks every run until released or cancelled.
type fakeRunner struct {
	started chan *model.Run
	release chan model.State

	// ignoreCancel: on cancellation return StateSuccess instead of StateCanceled.
	ignoreCancel bool
	// holdAfterCancel: after cancellation, still wait for a release (a slow kill).
	holdAfterCancel bool

	mu        sync.Mutex
	active    int
	maxActive int
	causes    []error
}

func newRunner() *fakeRunner {
	return &fakeRunner{started: make(chan *model.Run, 100), release: make(chan model.State)}
}

func (f *fakeRunner) Run(ctx context.Context, r *model.Run) (model.State, *model.Summary) {
	f.mu.Lock()
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

	f.started <- r
	select {
	case st := <-f.release:
		return st, &model.Summary{Playbooks: []model.PlaybookResult{{Playbook: "site.yml"}}}
	case <-ctx.Done():
		f.mu.Lock()
		f.causes = append(f.causes, context.Cause(ctx))
		f.mu.Unlock()
		if f.holdAfterCancel {
			<-f.release
		}
		if f.ignoreCancel {
			return model.StateSuccess, nil
		}
		return model.StateCanceled, nil
	}
}

func (f *fakeRunner) waitStarted(t *testing.T) *model.Run {
	t.Helper()
	select {
	case r := <-f.started:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a run to start")
		return nil
	}
}

func (f *fakeRunner) finish(t *testing.T, st model.State) {
	t.Helper()
	select {
	case f.release <- st:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out releasing a run")
	}
}

func (f *fakeRunner) noneStarted(t *testing.T) {
	t.Helper()
	select {
	case r := <-f.started:
		t.Fatalf("unexpected start of run %d", r.ID)
	case <-time.After(50 * time.Millisecond):
	}
}

type event struct {
	hook  string
	id    int64
	state model.State
	desc  string
}

// recHooks records every hook call.
type recHooks struct {
	mu     sync.Mutex
	events []event
}

func (h *recHooks) add(hook string, r *model.Run) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event{hook, r.ID, r.State, r.Description})
}

func (h *recHooks) OnQueued(_ context.Context, r *model.Run)   { h.add("queued", r) }
func (h *recHooks) OnStarted(_ context.Context, r *model.Run)  { h.add("started", r) }
func (h *recHooks) OnFinished(_ context.Context, r *model.Run) { h.add("finished", r) }

func (h *recHooks) of(id int64) []event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []event
	for _, e := range h.events {
		if e.id == id {
			out = append(out, e)
		}
	}
	return out
}

func (h *recHooks) finished(id int64) []event {
	var out []event
	for _, e := range h.of(id) {
		if e.hook == "finished" {
			out = append(out, e)
		}
	}
	return out
}

// checkFinishedOnce asserts that every run in the store that is finished got
// exactly one OnFinished, and unfinished runs got none.
func (h *recHooks) checkFinishedOnce(t *testing.T, s *store.Store) {
	t.Helper()
	runs, err := s.ListRuns(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		n := len(h.finished(r.ID))
		want := 0
		if r.State.Finished() {
			want = 1
		}
		if n != want {
			t.Errorf("run %d (%s): OnFinished called %d times, want %d", r.ID, r.State, n, want)
		}
	}
}

type harness struct {
	q      *Queue
	s      *store.Store
	runner *fakeRunner
	hooks  *recHooks
	cancel context.CancelFunc

	lmu     sync.Mutex
	lengths []int
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ansibled.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newHarness(t *testing.T, s *store.Store, runner *fakeRunner) *harness {
	t.Helper()
	if s == nil {
		s = openStore(t)
	}
	if runner == nil {
		runner = newRunner()
	}
	h := &harness{s: s, runner: runner, hooks: &recHooks{}}
	h.q = New(Options{
		Store:    s,
		Runner:   runner,
		Hooks:    h.hooks,
		Describe: func(r *model.Run) string { return "desc:" + string(r.State) },
		OnQueueLength: func(n int) {
			h.lmu.Lock()
			h.lengths = append(h.lengths, n)
			h.lmu.Unlock()
		},
	})
	return h
}

// start starts the worker; cleanup stops it and waits for it.
func (h *harness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	if err := h.q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		h.q.Wait()
	})
}

func (h *harness) enqueue(t *testing.T, req Request) *model.Run {
	t.Helper()
	r, ok, err := h.q.Enqueue(context.Background(), req)
	if err != nil || !ok || r == nil {
		t.Fatalf("Enqueue(%+v) = %v, %v, %v", req, r, ok, err)
	}
	return r
}

func (h *harness) get(t *testing.T, id int64) *model.Run {
	t.Helper()
	r, err := h.s.GetRun(context.Background(), id)
	if err != nil || r == nil {
		t.Fatalf("GetRun(%d) = %v, %v", id, r, err)
	}
	return r
}

// waitState polls until run id is in state st.
func (h *harness) waitState(t *testing.T, id int64, st model.State) *model.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := h.get(t, id)
		if r.State == st && (!st.Finished() || len(h.hooks.finished(id)) > 0) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %d: state %s, want %s", id, r.State, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func mr(iid int, sha string) Request {
	return Request{Kind: model.KindMR, MRIID: iid, SHA: sha, Ref: fmt.Sprintf("branch-%d", iid), Trigger: model.TriggerPush, TriggeredBy: "alice"}
}

func mainReq(sha string) Request {
	return Request{Kind: model.KindMain, SHA: sha, Ref: "main", Trigger: model.TriggerPush}
}

func TestEnqueueFields(t *testing.T) {
	h := newHarness(t, nil, nil)
	r := h.enqueue(t, mr(3, shaA))
	got := h.get(t, r.ID)
	if got.Key != "mr:3" || got.State != model.StateQueued || got.Mode != model.ModeCheck ||
		got.Description != "desc:queued" || got.Ref != "branch-3" || got.TriggeredBy != "alice" || got.QueuedAt.IsZero() {
		t.Fatalf("unexpected run %+v", got)
	}
	if ev := h.hooks.of(r.ID); !reflect.DeepEqual(ev, []event{{"queued", r.ID, model.StateQueued, "desc:queued"}}) {
		t.Fatalf("hooks = %+v", ev)
	}
}

func TestDedupeAndForce(t *testing.T) {
	h := newHarness(t, nil, nil)
	ctx := context.Background()
	r1 := h.enqueue(t, mainReq(shaA))

	// Same key+sha while queued: no-op, the queued run is untouched.
	r, ok, err := h.q.Enqueue(ctx, mainReq(shaA))
	if r != nil || ok || err != nil {
		t.Fatalf("dedupe while queued = %v, %v, %v", r, ok, err)
	}
	if got := h.get(t, r1.ID); got.State != model.StateQueued {
		t.Fatalf("deduped enqueue touched run: %+v", got)
	}

	// Same sha, other key: not a duplicate.
	h.enqueue(t, Request{Kind: model.KindNightly, SHA: shaA, Ref: "main", Trigger: model.TriggerSchedule})

	// Finished runs dedupe too.
	h.start(t)
	h.runner.waitStarted(t)
	h.runner.finish(t, model.StateSuccess)
	h.waitState(t, r1.ID, model.StateSuccess)
	if r, ok, _ := h.q.Enqueue(ctx, mainReq(shaA)); r != nil || ok {
		t.Fatal("dedupe after finish failed")
	}

	// Force always queues.
	forced := mainReq(shaA)
	forced.Force = true
	forced.Trigger = model.TriggerComment
	r3 := h.enqueue(t, forced)
	if r3.ID == r1.ID {
		t.Fatal("force reused the old run")
	}

	// A canceled run does not dedupe.
	h.runner.waitStarted(t) // nightly
	h.runner.finish(t, model.StateSuccess)
	h.runner.waitStarted(t) // forced main
	if err := h.q.CancelKey(ctx, "main", "test"); err != nil {
		t.Fatal(err)
	}
	h.waitState(t, r3.ID, model.StateCanceled)
	r4 := h.enqueue(t, mainReq(shaB))
	h.runner.waitStarted(t)
	h.runner.finish(t, model.StateFailed)
	h.waitState(t, r4.ID, model.StateFailed)
	if _, ok, _ := h.q.Enqueue(ctx, mr(1, shaC)); !ok {
		t.Fatal("new sha should queue")
	}
	h.hooks.checkFinishedOnce(t, h.s)
}

func TestCanceledRunDoesNotDedupe(t *testing.T) {
	h := newHarness(t, nil, nil)
	ctx := context.Background()
	r1 := h.enqueue(t, mr(1, shaA))
	if err := h.q.CancelKey(ctx, "mr:1", "closed"); err != nil {
		t.Fatal(err)
	}
	r2 := h.enqueue(t, mr(1, shaA))
	if r2.ID == r1.ID {
		t.Fatal("expected a new run")
	}
}

func TestSupersedeQueued(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.enqueue(t, mr(1, shaA))
	other := h.enqueue(t, mr(2, shaA))
	b := h.enqueue(t, mr(1, shaB))

	got := h.get(t, a.ID)
	if got.State != model.StateCanceled || got.Description != "superseded by bbbbbbbb" || got.FinishedAt.IsZero() {
		t.Fatalf("superseded run = %+v", got)
	}
	if ev := h.hooks.finished(a.ID); len(ev) != 1 || ev[0].state != model.StateCanceled || ev[0].desc != "superseded by bbbbbbbb" {
		t.Fatalf("OnFinished for superseded = %+v", ev)
	}
	if h.get(t, other.ID).State != model.StateQueued || h.get(t, b.ID).State != model.StateQueued {
		t.Fatal("other runs should stay queued")
	}
	h.hooks.checkFinishedOnce(t, h.s)
}

func TestSupersedeRunning(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.start(t)
	a := h.enqueue(t, mr(1, shaA))
	if s := h.runner.waitStarted(t); s.ID != a.ID {
		t.Fatalf("started %d, want %d", s.ID, a.ID)
	}
	if got := h.get(t, a.ID); got.State != model.StateRunning || got.StartedAt.IsZero() || got.Description != "desc:running" {
		t.Fatalf("running run = %+v", got)
	}

	b := h.enqueue(t, mr(1, shaB))
	got := h.waitState(t, a.ID, model.StateCanceled)
	if got.Description != "superseded by bbbbbbbb" || got.FinishedAt.IsZero() {
		t.Fatalf("superseded running run = %+v", got)
	}
	h.runner.mu.Lock()
	cause := h.runner.causes[0]
	h.runner.mu.Unlock()
	var cr cancelReason
	if !errors.As(cause, &cr) || string(cr) != "superseded by bbbbbbbb" {
		t.Fatalf("context cause = %v", cause)
	}

	if s := h.runner.waitStarted(t); s.ID != b.ID {
		t.Fatalf("started %d, want %d", s.ID, b.ID)
	}
	h.runner.finish(t, model.StateSuccess)
	got = h.waitState(t, b.ID, model.StateSuccess)
	if got.Summary == nil || len(got.Summary.Playbooks) != 1 || got.Description != "desc:success" {
		t.Fatalf("finished run = %+v", got)
	}

	want := []event{
		{"queued", a.ID, model.StateQueued, "desc:queued"},
		{"started", a.ID, model.StateRunning, "desc:running"},
		{"finished", a.ID, model.StateCanceled, "superseded by bbbbbbbb"},
	}
	if ev := h.hooks.of(a.ID); !reflect.DeepEqual(ev, want) {
		t.Fatalf("hooks for a = %+v", ev)
	}
	h.hooks.checkFinishedOnce(t, h.s)
}

func TestCancelKey(t *testing.T) {
	h := newHarness(t, nil, nil)
	ctx := context.Background()
	h.start(t)
	running := h.enqueue(t, mainReq(shaA))
	h.runner.waitStarted(t)
	queued := h.enqueue(t, mr(5, shaB))
	untouched := h.enqueue(t, mr(6, shaB))

	if err := h.q.CancelKey(ctx, "mr:5", "merge request closed"); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, queued.ID); got.State != model.StateCanceled || got.Description != "merge request closed" {
		t.Fatalf("queued after CancelKey = %+v", got)
	}
	if err := h.q.CancelKey(ctx, "main", "stop"); err != nil {
		t.Fatal(err)
	}
	// A second cancel of the same running run keeps the first reason.
	if err := h.q.CancelKey(ctx, "main", "again"); err != nil {
		t.Fatal(err)
	}
	if got := h.waitState(t, running.ID, model.StateCanceled); got.Description != "stop" {
		t.Fatalf("running after CancelKey = %+v", got)
	}
	// Cancelling a key with nothing active is fine.
	if err := h.q.CancelKey(ctx, "mr:99", "nothing"); err != nil {
		t.Fatal(err)
	}

	if s := h.runner.waitStarted(t); s.ID != untouched.ID {
		t.Fatalf("started %d, want %d", s.ID, untouched.ID)
	}
	h.runner.finish(t, model.StateSuccess)
	h.waitState(t, untouched.ID, model.StateSuccess)
	h.hooks.checkFinishedOnce(t, h.s)
}

func TestRunnerIgnoringCancelIsStillCanceled(t *testing.T) {
	r := newRunner()
	r.ignoreCancel = true
	h := newHarness(t, nil, r)
	h.start(t)
	a := h.enqueue(t, mr(1, shaA))
	h.runner.waitStarted(t)
	if err := h.q.CancelKey(context.Background(), "mr:1", "closed"); err != nil {
		t.Fatal(err)
	}
	if got := h.waitState(t, a.ID, model.StateCanceled); got.Description != "closed" {
		t.Fatalf("run = %+v", got)
	}
	h.hooks.checkFinishedOnce(t, h.s)
}

func TestReopenWhileCancelling(t *testing.T) {
	// MR closed then reopened (same sha) before the runner has finished dying:
	// the dying run must not dedupe the new one.
	r := newRunner()
	r.holdAfterCancel = true
	h := newHarness(t, nil, r)
	h.start(t)
	a := h.enqueue(t, mr(1, shaA))
	h.runner.waitStarted(t)
	if err := h.q.CancelKey(context.Background(), "mr:1", "closed"); err != nil {
		t.Fatal(err)
	}
	b := h.enqueue(t, mr(1, shaA))
	h.runner.finish(t, model.StateCanceled) // the dying run returns
	if got := h.waitState(t, a.ID, model.StateCanceled); got.Description != "closed" {
		t.Fatalf("run a = %+v", got)
	}
	if s := h.runner.waitStarted(t); s.ID != b.ID {
		t.Fatalf("started %d, want %d", s.ID, b.ID)
	}
	h.runner.finish(t, model.StateSuccess)
	h.waitState(t, b.ID, model.StateSuccess)
	h.hooks.checkFinishedOnce(t, h.s)
}

func TestFIFOAndSingleWorker(t *testing.T) {
	h := newHarness(t, nil, nil)
	var want []int64
	want = append(want, h.enqueue(t, mainReq(shaA)).ID)
	want = append(want, h.enqueue(t, mr(2, shaA)).ID)
	want = append(want, h.enqueue(t, mr(1, shaA)).ID)
	want = append(want, h.enqueue(t, Request{Kind: model.KindNightly, SHA: shaA, Trigger: model.TriggerSchedule}).ID)
	h.start(t)

	var got []int64
	states := []model.State{model.StateSuccess, model.StateFailed, model.StateError, model.StateSuccess}
	for i := range want {
		s := h.runner.waitStarted(t)
		got = append(got, s.ID)
		h.runner.noneStarted(t) // nothing else starts while this one runs
		h.runner.finish(t, states[i])
		h.waitState(t, s.ID, states[i])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("run order %v, want %v", got, want)
	}
	h.runner.mu.Lock()
	maxActive := h.runner.maxActive
	h.runner.mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("max concurrent runs = %d", maxActive)
	}
	h.hooks.checkFinishedOnce(t, h.s)
}

func TestNonTerminalRunnerStateIsError(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.start(t)
	a := h.enqueue(t, mainReq(shaA))
	h.runner.waitStarted(t)
	h.runner.finish(t, model.StateRunning)
	h.waitState(t, a.ID, model.StateError)
}

func TestRestartRecovery(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	stale := &model.Run{Kind: model.KindMain, SHA: shaA, State: model.StateRunning, StartedAt: time.Now(), Description: "old"}
	if err := s.CreateRun(ctx, stale); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, s, nil)
	h.start(t)
	got := h.runner.waitStarted(t)
	if got.ID != stale.ID {
		t.Fatalf("started %d, want %d", got.ID, stale.ID)
	}
	ev := h.hooks.of(stale.ID)
	if len(ev) != 1 || ev[0].hook != "started" {
		t.Fatalf("hooks for recovered run = %+v (want only started)", ev)
	}
	h.runner.finish(t, model.StateSuccess)
	h.waitState(t, stale.ID, model.StateSuccess)
	h.hooks.checkFinishedOnce(t, h.s)
}

func TestShutdownRequeues(t *testing.T) {
	s := openStore(t)
	h := newHarness(t, s, nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	a := h.enqueue(t, mainReq(shaA))
	h.runner.waitStarted(t)
	cancel()
	done := make(chan struct{})
	go func() { h.q.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after shutdown")
	}

	got := h.get(t, a.ID)
	if got.State != model.StateQueued || !got.StartedAt.IsZero() || !got.FinishedAt.IsZero() || got.Description != "desc:queued" {
		t.Fatalf("after shutdown = %+v", got)
	}
	if ev := h.hooks.finished(a.ID); len(ev) != 0 {
		t.Fatalf("OnFinished on shutdown: %+v", ev)
	}

	// The next process runs it.
	h2 := newHarness(t, s, nil)
	h2.start(t)
	if r := h2.runner.waitStarted(t); r.ID != a.ID {
		t.Fatalf("started %d, want %d", r.ID, a.ID)
	}
	h2.runner.finish(t, model.StateSuccess)
	h2.waitState(t, a.ID, model.StateSuccess)
}

func TestShutdownIdleAndWaitWithoutStart(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.q.Wait() // never started: returns at once
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	h.q.Wait()
}

func TestQueueLength(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.enqueue(t, mainReq(shaA))
	h.enqueue(t, mr(1, shaA))
	h.enqueue(t, mr(1, shaB)) // supersedes mr:1 shaA
	h.start(t)
	h.runner.waitStarted(t)
	h.runner.finish(t, model.StateSuccess)
	h.runner.waitStarted(t)
	h.runner.finish(t, model.StateSuccess)

	h.lmu.Lock()
	defer h.lmu.Unlock()
	// enqueue 1, 2; supersede -> 1, enqueue -> 2; Start -> 2; start main -> 1; start mr -> 0
	want := []int{1, 2, 1, 2, 2, 1, 0}
	if !reflect.DeepEqual(h.lengths, want) {
		t.Fatalf("queue lengths %v, want %v", h.lengths, want)
	}
}

func TestConcurrentEnqueue(t *testing.T) {
	h := newHarness(t, nil, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sha := fmt.Sprintf("%040d", i)
			if _, _, err := h.q.Enqueue(ctx, mr(1, sha)); err != nil {
				t.Error(err)
			}
			if _, _, err := h.q.Enqueue(ctx, mr(1, sha)); err != nil { // may dedupe or supersede
				t.Error(err)
			}
			if i%5 == 0 {
				if err := h.q.CancelKey(ctx, "mr:2", "x"); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	active, err := h.s.ActiveByKey(ctx, "mr:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("%d active runs for mr:1, want 1", len(active))
	}
	h.hooks.checkFinishedOnce(t, h.s)
}

func TestCancelRun(t *testing.T) {
	h := newHarness(t, nil, nil)
	ctx := context.Background()
	h.start(t)
	running := h.enqueue(t, mainReq(shaA))
	h.runner.waitStarted(t)
	q1 := h.enqueue(t, mr(5, shaB))
	q2 := h.enqueue(t, mr(6, shaB))

	if err := h.q.CancelRun(ctx, q1.ID, "canceled by carol"); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, q1.ID); got.State != model.StateCanceled || got.Description != "canceled by carol" {
		t.Fatalf("queued after CancelRun = %+v", got)
	}
	if err := h.q.CancelRun(ctx, running.ID, "canceled by carol"); err != nil {
		t.Fatal(err)
	}
	h.waitState(t, running.ID, model.StateCanceled)
	if err := h.q.CancelRun(ctx, q1.ID, "again"); !errors.Is(err, ErrNotActive) {
		t.Errorf("finished run: %v", err)
	}
	if err := h.q.CancelRun(ctx, 999, "x"); !errors.Is(err, ErrNotActive) {
		t.Errorf("missing run: %v", err)
	}
	if s := h.runner.waitStarted(t); s.ID != q2.ID {
		t.Fatalf("started %d, want %d (CancelRun must not touch other runs)", s.ID, q2.ID)
	}
	h.runner.finish(t, model.StateSuccess)
	h.waitState(t, q2.ID, model.StateSuccess)
	h.hooks.checkFinishedOnce(t, h.s)
}

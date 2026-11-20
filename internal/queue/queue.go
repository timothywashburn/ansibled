// Package queue implements ansibled's run queue (DESIGN.md "Queue"): a single
// worker that executes runs one at a time in FIFO order, where queueing a run
// supersedes (cancels) every queued or running run with the same key, and
// queueing a (key, sha) that already has a run is a no-op unless forced.
//
// All state lives in the store, so the queue survives restarts: on Start,
// runs left `running` by a previous process go back to `queued`.
//
// Every state change is persisted first and then reported through Hooks. The
// hooks are called while the queue's mutex is held, so they observe changes in
// exactly the order they were persisted (for example, an MR comment is never
// overwritten by an older run's result after a newer run was queued).
package queue

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/timothywashburn/ansibled/internal/model"
	"github.com/timothywashburn/ansibled/internal/store"
)

// idleRecheck is how often an idle worker looks for queued runs even without a wake-up.
const idleRecheck = 30 * time.Second

// hookTimeout bounds hooks and store writes made after the queue's context is done.
const hookTimeout = 30 * time.Second

// Request asks for a run.
type Request struct {
	Kind        model.Kind
	SHA, Ref    string
	MRIID       int
	Trigger     model.Trigger
	TriggeredBy string
	Force       bool // skip dedupe (used for `/ansibled check` comments)
}

// Runner executes one run. It must honour ctx cancellation promptly (kill its
// processes) and then return model.StateCanceled. Timeouts are the runner's
// job (it returns StateError). context.Cause(ctx) carries the cancel reason.
type Runner interface {
	Run(ctx context.Context, r *model.Run) (model.State, *model.Summary)
}

// Hooks are called synchronously by the queue after it has persisted the
// change. Implementations report to the forge/metrics; errors are theirs to log.
// Each hook receives its own copy of the run.
type Hooks interface {
	OnQueued(ctx context.Context, r *model.Run)
	OnStarted(ctx context.Context, r *model.Run)
	OnFinished(ctx context.Context, r *model.Run) // any terminal state, including canceled
}

// Options configure a Queue. Store and Runner are required.
type Options struct {
	Store  *store.Store
	Runner Runner
	Hooks  Hooks // nil: no hooks
	// Describe fills r.Description for queued/running/finished runs (canceled
	// runs keep the reason the queue set). nil: the state name.
	Describe func(r *model.Run) string
	Logger   *slog.Logger // nil: slog.Default()
	// OnQueueLength is optional; it is called whenever the number of queued runs changes.
	OnQueueLength func(n int)
}

// Queue is the run queue. Create it with New, then call Start.
type Queue struct {
	o    Options
	log  *slog.Logger
	wake chan struct{}
	wg   sync.WaitGroup

	// mu serialises every state change (Enqueue, CancelKey, the worker's
	// transitions) together with the hooks that report it.
	mu        sync.Mutex
	cur       *model.Run              // the running run, nil when idle
	curCancel context.CancelCauseFunc // cancels cur's context
	curReason string                  // non-empty once cur was cancelled by CancelKey/supersede
}

// New returns a queue. It does nothing until Start.
func New(o Options) *Queue {
	if o.Hooks == nil {
		o.Hooks = noHooks{}
	}
	if o.Describe == nil {
		o.Describe = func(r *model.Run) string { return string(r.State) }
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Queue{o: o, log: log.With("component", "queue"), wake: make(chan struct{}, 1)}
}

// Start resets every `running` run in the store to `queued` (without hooks),
// then starts the worker. When ctx is done the worker cancels the in-flight
// run, puts it back to `queued` (no OnFinished) and exits; see Wait.
func (q *Queue) Start(ctx context.Context) error {
	q.mu.Lock()
	running, err := q.o.Store.ListByState(ctx, model.StateRunning)
	if err != nil {
		q.mu.Unlock()
		return err
	}
	for _, r := range running {
		q.log.Info("re-queueing interrupted run", "run", r.ID, "key", r.Key, "sha", r.SHA)
		requeue(q.o.Describe, r)
		if err := q.o.Store.UpdateRun(ctx, r); err != nil {
			q.mu.Unlock()
			return err
		}
	}
	q.notifyLength(ctx)
	q.mu.Unlock()

	q.wg.Add(1)
	go q.worker(ctx)
	return nil
}

// Wait blocks until the worker has exited after Start's ctx was cancelled.
// It returns at once if Start was never called.
func (q *Queue) Wait() { q.wg.Wait() }

// Enqueue queues a run. It returns (run, true, nil) when the run was queued
// and (nil, false, nil) when an equal (key, sha) run already exists and
// req.Force is false. Queueing cancels every active run with the same key.
func (q *Queue) Enqueue(ctx context.Context, req Request) (*model.Run, bool, error) {
	key := model.Key(req.Kind, req.MRIID)

	q.mu.Lock()
	defer q.mu.Unlock()

	if !req.Force {
		// A running run that is already being cancelled (e.g. MR closed, then
		// reopened before the runner returned) will end up canceled, so it
		// does not count.
		var except int64
		if q.cur != nil && q.curReason != "" {
			except = q.cur.ID
		}
		exists, err := q.o.Store.ExistsExcept(ctx, key, req.SHA, except)
		if err != nil {
			return nil, false, err
		}
		if exists {
			q.log.Debug("dedupe: run exists", "key", key, "sha", req.SHA)
			return nil, false, nil
		}
	}

	if err := q.cancelKeyLocked(ctx, key, "superseded by "+model.Short(req.SHA)); err != nil {
		return nil, false, err
	}

	r := &model.Run{
		Kind:        req.Kind,
		Key:         key,
		Mode:        model.ModeCheck,
		SHA:         req.SHA,
		Ref:         req.Ref,
		MRIID:       req.MRIID,
		Trigger:     req.Trigger,
		TriggeredBy: req.TriggeredBy,
		State:       model.StateQueued,
		QueuedAt:    time.Now(),
	}
	if req.Kind != model.KindMR {
		r.MRIID = 0
	}
	r.Description = q.o.Describe(r)
	if err := q.o.Store.CreateRun(ctx, r); err != nil {
		return nil, false, err
	}
	// Describe again now that the run has an ID, in case it uses it.
	if d := q.o.Describe(r); d != r.Description {
		r.Description = d
		if err := q.o.Store.UpdateRun(ctx, r); err != nil {
			return nil, false, err
		}
	}
	q.log.Info("run queued", "run", r.ID, "key", key, "sha", r.SHA, "trigger", r.Trigger, "by", r.TriggeredBy, "force", req.Force)
	q.o.Hooks.OnQueued(ctx, clone(r))
	q.notifyLength(ctx)

	select {
	case q.wake <- struct{}{}:
	default:
	}
	return clone(r), true, nil
}

// CancelKey cancels every queued or running run with key, setting their
// description to reason.
func (q *Queue) CancelKey(ctx context.Context, key, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cancelKeyLocked(ctx, key, reason)
}

// cancelReason is the context cause used when a running run is cancelled.
type cancelReason string

func (c cancelReason) Error() string { return "canceled: " + string(c) }

// cancelKeyLocked does CancelKey; q.mu must be held.
func (q *Queue) cancelKeyLocked(ctx context.Context, key, reason string) error {
	active, err := q.o.Store.ActiveByKey(ctx, key)
	if err != nil {
		return err
	}
	queuedChanged := false
	var errs []error
	for _, r := range active {
		if q.cur != nil && r.ID == q.cur.ID {
			// The worker records the final state when the runner returns.
			if q.curReason == "" {
				q.log.Info("canceling running run", "run", r.ID, "key", key, "reason", reason)
				q.curReason = reason
				q.curCancel(cancelReason(reason))
			}
			continue
		}
		// Queued, or `running` left over from a previous process before Start.
		if r.State == model.StateQueued {
			queuedChanged = true
		}
		q.log.Info("canceling run", "run", r.ID, "key", key, "state", r.State, "reason", reason)
		r.State = model.StateCanceled
		r.Description = reason
		r.FinishedAt = time.Now()
		if err := q.o.Store.UpdateRun(ctx, r); err != nil {
			errs = append(errs, err)
			continue
		}
		q.o.Hooks.OnFinished(ctx, clone(r))
	}
	if queuedChanged {
		q.notifyLength(ctx)
	}
	return errors.Join(errs...)
}

func (q *Queue) worker(ctx context.Context) {
	defer q.wg.Done()
	q.log.Info("worker started")
	defer q.log.Info("worker stopped")
	t := time.NewTimer(idleRecheck)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		ran, err := q.runNext(ctx)
		if err != nil && ctx.Err() == nil {
			q.log.Error("worker", "err", err)
		}
		if ran && err == nil {
			continue
		}
		t.Reset(idleRecheck)
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-t.C:
		}
	}
}

// runNext runs the oldest queued run, if any, and reports whether it found one.
func (q *Queue) runNext(ctx context.Context) (bool, error) {
	q.mu.Lock()
	r, err := q.o.Store.NextQueued(ctx)
	if err != nil || r == nil {
		q.mu.Unlock()
		return false, err
	}
	r.State = model.StateRunning
	r.StartedAt = time.Now()
	r.FinishedAt = time.Time{}
	r.Summary = nil
	r.Description = q.o.Describe(r)
	if err := q.o.Store.UpdateRun(ctx, r); err != nil {
		q.mu.Unlock()
		return false, err
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	q.cur, q.curCancel, q.curReason = r, cancel, ""
	q.log.Info("run started", "run", r.ID, "key", r.Key, "sha", r.SHA)
	q.o.Hooks.OnStarted(ctx, clone(r))
	q.notifyLength(ctx)
	q.mu.Unlock()

	state, sum := q.o.Runner.Run(runCtx, clone(r))

	q.mu.Lock()
	defer q.mu.Unlock()
	reason := q.curReason
	q.cur, q.curCancel, q.curReason = nil, nil, ""

	// After shutdown ctx is done, but the final writes and hooks must still happen.
	wctx := ctx
	if ctx.Err() != nil {
		var c context.CancelFunc
		wctx, c = context.WithTimeout(context.WithoutCancel(ctx), hookTimeout)
		defer c()
	}

	if reason == "" && ctx.Err() != nil {
		// Shutdown: run it again on the next start.
		q.log.Info("shutdown: re-queueing run", "run", r.ID, "runner_state", state)
		requeue(q.o.Describe, r)
		return true, q.o.Store.UpdateRun(wctx, r)
	}

	r.Summary = sum
	r.FinishedAt = time.Now()
	switch {
	case reason != "":
		r.State = model.StateCanceled
		r.Description = reason
	case state.Finished():
		r.State = state
		r.Description = q.o.Describe(r)
	default:
		q.log.Error("runner returned a non-terminal state", "run", r.ID, "state", state)
		r.State = model.StateError
		r.Description = q.o.Describe(r)
	}
	if err := q.o.Store.UpdateRun(wctx, r); err != nil {
		return true, err
	}
	q.log.Info("run finished", "run", r.ID, "key", r.Key, "sha", r.SHA, "state", r.State,
		"duration", r.Duration().Round(time.Second).String(), "description", r.Description)
	q.o.Hooks.OnFinished(wctx, clone(r))
	return true, nil
}

// requeue turns a running run back into a queued one.
func requeue(describe func(*model.Run) string, r *model.Run) {
	r.State = model.StateQueued
	r.StartedAt = time.Time{}
	r.FinishedAt = time.Time{}
	r.Summary = nil
	r.Description = describe(r)
}

// notifyLength reports the number of queued runs; q.mu must be held.
func (q *Queue) notifyLength(ctx context.Context) {
	if q.o.OnQueueLength == nil {
		return
	}
	n, err := q.o.Store.CountByState(ctx, model.StateQueued)
	if err != nil {
		q.log.Warn("count queued runs", "err", err)
		return
	}
	q.o.OnQueueLength(n)
}

func clone(r *model.Run) *model.Run {
	c := *r
	return &c
}

type noHooks struct{}

func (noHooks) OnQueued(context.Context, *model.Run)   {}
func (noHooks) OnStarted(context.Context, *model.Run)  {}
func (noHooks) OnFinished(context.Context, *model.Run) {}

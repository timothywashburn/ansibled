// Package daemon wires ansibled together: it turns forge events into queued runs
// (applying the trust rules), reports run progress back to the forge and metrics,
// and runs the startup catch-up, the nightly schedule and housekeeping.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/timothywashburn/ansibled/internal/config"
	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/metrics"
	"github.com/timothywashburn/ansibled/internal/model"
	"github.com/timothywashburn/ansibled/internal/queue"
	"github.com/timothywashburn/ansibled/internal/report"
	"github.com/timothywashburn/ansibled/internal/store"
)

// CommentCommand is the MR comment that asks for a dry run.
const CommentCommand = "/ansibled check"

// forgeTimeout bounds every forge call made from a hook or handler.
const forgeTimeout = 20 * time.Second

// Enqueuer is the part of the queue the daemon uses (queue.Queue satisfies it).
type Enqueuer interface {
	Enqueue(ctx context.Context, req queue.Request) (*model.Run, bool, error)
	CancelKey(ctx context.Context, key, reason string) error
}

// Files is the part of the engine the daemon uses for housekeeping.
type Files interface {
	DeleteRunFiles(id int64) error
	Housekeep(unusedFor time.Duration) error
}

// Daemon holds the wiring. Create it with New, then set Queue before Start.
type Daemon struct {
	cfg     *config.Config
	forge   forge.Forge
	store   *store.Store
	metrics *metrics.Metrics
	files   Files
	log     *slog.Logger

	// Queue must be set before any event is handled (it needs the Daemon as its Hooks).
	Queue Enqueuer

	now func() time.Time
}

// New creates a Daemon.
func New(cfg *config.Config, f forge.Forge, st *store.Store, m *metrics.Metrics, files Files, log *slog.Logger) *Daemon {
	if log == nil {
		log = slog.Default()
	}
	return &Daemon{cfg: cfg, forge: f, store: st, metrics: m, files: files, log: log, now: time.Now}
}

// ---------------------------------------------------------------------------
// Hooks (called by the queue)

// OnQueued reports a newly queued run.
func (d *Daemon) OnQueued(ctx context.Context, r *model.Run) { d.report(ctx, r) }

// OnStarted reports a run that started.
func (d *Daemon) OnStarted(ctx context.Context, r *model.Run) { d.report(ctx, r) }

// OnFinished reports a finished run and updates metrics.
func (d *Daemon) OnFinished(ctx context.Context, r *model.Run) {
	if d.metrics != nil {
		d.metrics.RunFinished(r)
	}
	d.report(ctx, r)
}

// report pushes a run's state to the forge: commit status for mr/main runs,
// the MR comment for mr runs. Nightly runs are only reported through metrics.
func (d *Daemon) report(ctx context.Context, r *model.Run) {
	if r.Kind != model.KindMR && r.Kind != model.KindMain {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forgeTimeout)
	defer cancel()

	st := forge.Status{
		Name:        d.cfg.StatusName(),
		State:       report.StatusState(r.State),
		Description: r.Description,
		TargetURL:   d.cfg.RunURL(r.ID),
		Ref:         r.Ref,
	}
	if err := d.forge.SetStatus(ctx, r.SHA, st); err != nil {
		d.log.Warn("set commit status", "run", r.ID, "sha", r.ShortSHA(), "err", err)
	}

	if r.Kind != model.KindMR {
		return
	}
	// A run superseded while running finishes after its replacement was queued;
	// don't let its "canceled" overwrite the newer run's comment.
	if r.State == model.StateCanceled && d.newerActive(ctx, r) {
		return
	}
	marker := d.cfg.CommentMarker()
	body := report.MRComment(r, d.cfg.Site, marker, d.cfg.RunURL(r.ID))
	if err := d.forge.UpsertMRComment(ctx, r.MRIID, marker, body); err != nil {
		d.log.Warn("update MR comment", "run", r.ID, "mr", r.MRIID, "err", err)
	}
}

func (d *Daemon) newerActive(ctx context.Context, r *model.Run) bool {
	active, err := d.store.ActiveByKey(ctx, r.Key)
	if err != nil {
		return false
	}
	for _, a := range active {
		if a.ID > r.ID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Webhook events

// HandleEvents applies the trust rules to webhook events and queues runs.
// It is called by the web server in its own goroutine.
func (d *Daemon) HandleEvents(ctx context.Context, events []forge.Event) {
	for _, ev := range events {
		accepted, err := d.handle(ctx, ev)
		if err != nil {
			d.log.Error("handle webhook event", "event", ev.Name, "err", err)
		}
		if d.metrics != nil {
			d.metrics.Webhook(eventLabel(ev), accepted)
		}
	}
}

func eventLabel(ev forge.Event) string {
	if ev.Type == forge.EventIgnored {
		return "ignored"
	}
	return string(ev.Type)
}

// handle returns whether the event led to something (a run queued or cancelled).
func (d *Daemon) handle(ctx context.Context, ev forge.Event) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*forgeTimeout)
	defer cancel()

	switch ev.Type {
	case forge.EventPush:
		return d.onPush(ctx, ev)
	case forge.EventMR:
		return d.onMR(ctx, ev)
	case forge.EventComment:
		return d.onComment(ctx, ev)
	default:
		return false, nil
	}
}

func (d *Daemon) onPush(ctx context.Context, ev forge.Event) (bool, error) {
	trusted, err := d.isTrusted(ctx, ev.Pusher)
	if err != nil {
		return false, fmt.Errorf("trust check for %s: %w", ev.Pusher.Username, err)
	}
	log := d.log.With("branch", ev.Branch, "sha", model.Short(ev.SHA), "pusher", ev.Pusher.Username)
	if !trusted {
		log.Info("ignoring push from untrusted user")
		return false, nil
	}

	def, err := d.forge.DefaultBranch(ctx)
	if err != nil {
		return false, fmt.Errorf("default branch: %w", err)
	}
	if ev.Branch == def {
		return d.enqueue(ctx, queue.Request{
			Kind: model.KindMain, SHA: ev.SHA, Ref: ev.Branch,
			Trigger: model.TriggerPush, TriggeredBy: ev.Pusher.Username,
		})
	}

	mrs, err := d.forge.MRsForBranch(ctx, ev.Branch)
	if err != nil {
		return false, fmt.Errorf("MRs for %s: %w", ev.Branch, err)
	}
	if len(mrs) == 0 {
		log.Debug("push to a branch without an open MR")
		return false, nil
	}
	queuedAny := false
	for _, mr := range mrs {
		ok, err := d.enqueue(ctx, queue.Request{
			Kind: model.KindMR, SHA: ev.SHA, Ref: ev.Branch, MRIID: mr.IID,
			Trigger: model.TriggerPush, TriggeredBy: ev.Pusher.Username,
		})
		if err != nil {
			return queuedAny, err
		}
		queuedAny = queuedAny || ok
	}
	return queuedAny, nil
}

func (d *Daemon) onMR(ctx context.Context, ev forge.Event) (bool, error) {
	mr := ev.MR
	switch ev.MRAction {
	case forge.MRClose, forge.MRMerge:
		reason := "MR closed"
		if ev.MRAction == forge.MRMerge {
			reason = "MR merged"
		}
		return true, d.Queue.CancelKey(ctx, model.Key(model.KindMR, mr.IID), reason)

	case forge.MROpen, forge.MRReopen:
		if mr.FromFork {
			d.log.Info("MR from a fork: waiting for a trusted /ansibled check comment", "mr", mr.IID)
			return false, nil
		}
		return d.enqueueMRIfTrustedPusher(ctx, mr, model.TriggerMROpen)
	}
	return false, nil
}

// enqueueMRIfTrustedPusher queues an MR head if the person who pushed it is trusted.
func (d *Daemon) enqueueMRIfTrustedPusher(ctx context.Context, mr forge.MR, trig model.Trigger) (bool, error) {
	log := d.log.With("mr", mr.IID, "sha", model.Short(mr.HeadSHA))
	pusher, err := d.forge.PusherOf(ctx, mr.SourceBranch, mr.HeadSHA)
	if err != nil {
		return false, fmt.Errorf("pusher of MR !%d: %w", mr.IID, err)
	}
	if pusher == nil {
		log.Info("can't tell who pushed the MR head; waiting for a push or an /ansibled check comment")
		return false, nil
	}
	trusted, err := d.isTrusted(ctx, *pusher)
	if err != nil {
		return false, fmt.Errorf("trust check for %s: %w", pusher.Username, err)
	}
	if !trusted {
		log.Info("MR head was pushed by an untrusted user", "pusher", pusher.Username)
		return false, nil
	}
	return d.enqueue(ctx, queue.Request{
		Kind: model.KindMR, SHA: mr.HeadSHA, Ref: mr.SourceBranch, MRIID: mr.IID,
		Trigger: trig, TriggeredBy: pusher.Username,
	})
}

// isTrusted applies the trust rule. ansibled's own bot user is never trusted,
// even if it is in the group: its token can push commits, so trusting it would
// let anyone holding the token run code as root.
func (d *Daemon) isTrusted(ctx context.Context, u forge.User) (bool, error) {
	bot, err := d.forge.BotUser(ctx)
	if err != nil {
		return false, fmt.Errorf("bot user: %w", err)
	}
	if u.ID == bot.ID {
		return false, nil
	}
	return d.forge.IsTrusted(ctx, u)
}

// IsCommand reports whether a comment body asks for a dry run.
func IsCommand(body string) bool {
	b := strings.TrimSpace(body)
	if !strings.HasPrefix(b, CommentCommand) {
		return false
	}
	rest := b[len(CommentCommand):]
	return rest == "" || rest[0] == ' ' || rest[0] == '\n' || rest[0] == '\r' || rest[0] == '\t'
}

func (d *Daemon) onComment(ctx context.Context, ev forge.Event) (bool, error) {
	if !IsCommand(ev.Body) {
		return false, nil
	}
	trusted, err := d.isTrusted(ctx, ev.Author)
	if err != nil {
		return false, fmt.Errorf("trust check for %s: %w", ev.Author.Username, err)
	}
	if !trusted {
		d.log.Info("ignoring /ansibled check from untrusted user", "mr", ev.MRIID, "author", ev.Author.Username)
		return false, nil
	}
	mr, err := d.forge.GetMR(ctx, ev.MRIID)
	if err != nil {
		return false, fmt.Errorf("get MR !%d: %w", ev.MRIID, err)
	}
	if mr.State != "opened" {
		return false, nil
	}
	return d.enqueue(ctx, queue.Request{
		Kind: model.KindMR, SHA: mr.HeadSHA, Ref: mr.SourceBranch, MRIID: mr.IID,
		Trigger: model.TriggerComment, TriggeredBy: ev.Author.Username, Force: true,
	})
}

func (d *Daemon) enqueue(ctx context.Context, req queue.Request) (bool, error) {
	if req.SHA == "" {
		return false, errors.New("empty sha")
	}
	r, queued, err := d.Queue.Enqueue(ctx, req)
	if err != nil {
		return false, err
	}
	if queued {
		d.log.Info("queued run", "run", r.ID, "kind", r.Kind, "mr", r.MRIID, "sha", r.ShortSHA(), "trigger", r.Trigger, "by", r.TriggeredBy)
	}
	return queued, nil
}

// ---------------------------------------------------------------------------
// Startup catch-up

// CatchUp queues what changed while ansibled was down (DESIGN.md "Startup catch-up").
// It retries until the forge answers or ctx ends, since at a cold start the
// network may come up after ansibled.
func (d *Daemon) CatchUp(ctx context.Context) {
	delay := 5 * time.Second
	for {
		err := d.catchUpOnce(ctx)
		if err == nil {
			d.log.Info("startup catch-up done")
			return
		}
		d.log.Warn("startup catch-up failed, retrying", "err", err, "in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Minute)
	}
}

func (d *Daemon) catchUpOnce(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := d.catchUpMain(ctx); err != nil {
		return err
	}
	open, err := d.forge.OpenMRs(ctx)
	if err != nil {
		return fmt.Errorf("open MRs: %w", err)
	}
	for _, mr := range open {
		if mr.FromFork || mr.HeadSHA == "" {
			continue
		}
		exists, err := d.store.Exists(ctx, model.Key(model.KindMR, mr.IID), mr.HeadSHA)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := d.enqueueMRIfTrustedPusher(ctx, mr, model.TriggerStartup); err != nil {
			d.log.Warn("catch-up for MR", "mr", mr.IID, "err", err)
		}
	}
	return nil
}

// catchUpMain queues the default branch head if it was never run. The default
// branch is protected and reviewed, so it is queued even when the pusher can't
// be found; a known untrusted pusher is skipped.
func (d *Daemon) catchUpMain(ctx context.Context) error {
	def, err := d.forge.DefaultBranch(ctx)
	if err != nil {
		return fmt.Errorf("default branch: %w", err)
	}
	head, err := d.forge.BranchHead(ctx, def)
	if err != nil {
		return fmt.Errorf("head of %s: %w", def, err)
	}
	exists, err := d.store.Exists(ctx, model.Key(model.KindMain, 0), head)
	if err != nil || exists {
		return err
	}
	by := ""
	if p, err := d.forge.PusherOf(ctx, def, head); err == nil && p != nil {
		trusted, err := d.isTrusted(ctx, *p)
		if err != nil {
			return err
		}
		if !trusted {
			d.log.Info("default branch head was pushed by an untrusted user; not running it", "sha", model.Short(head), "pusher", p.Username)
			return nil
		}
		by = p.Username
	}
	_, err = d.enqueue(ctx, queue.Request{Kind: model.KindMain, SHA: head, Ref: def, Trigger: model.TriggerStartup, TriggeredBy: by})
	return err
}

// ---------------------------------------------------------------------------
// Nightly schedule and housekeeping

// StartSchedule starts the nightly cron (if configured). Stop it by cancelling ctx.
func (d *Daemon) StartSchedule(ctx context.Context) error {
	if d.cfg.Schedule.Nightly == "" {
		return nil
	}
	c := cron.New(cron.WithLocation(d.cfg.Location()))
	if _, err := c.AddFunc(d.cfg.Schedule.Nightly, func() { d.Nightly(ctx) }); err != nil {
		return err
	}
	c.Start()
	go func() {
		<-ctx.Done()
		<-c.Stop().Done()
	}()
	return nil
}

// Nightly queues the drift run for the default branch head.
func (d *Daemon) Nightly(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*forgeTimeout)
	defer cancel()
	def, err := d.forge.DefaultBranch(ctx)
	if err == nil {
		var head string
		head, err = d.forge.BranchHead(ctx, def)
		if err == nil {
			err = d.queueNightly(ctx, def, head)
		}
	}
	if err != nil {
		d.log.Error("nightly run not queued", "err", err)
	}
}

func (d *Daemon) queueNightly(ctx context.Context, branch, head string) error {
	active, err := d.store.ActiveByKey(ctx, model.Key(model.KindMain, 0))
	if err != nil {
		return err
	}
	for _, a := range active {
		if a.SHA == head {
			d.log.Info("skipping nightly run: a main run for the same commit is pending", "run", a.ID)
			return nil
		}
	}
	// Force: the same commit must be re-checked every night (that's drift detection).
	_, err = d.enqueue(ctx, queue.Request{Kind: model.KindNightly, SHA: head, Ref: branch, Trigger: model.TriggerSchedule, Force: true})
	return err
}

// Housekeep deletes expired run logs and unused environments, now and every hour.
func (d *Daemon) Housekeep(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		d.housekeepOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *Daemon) housekeepOnce(ctx context.Context) {
	ids, err := d.store.LogsOlderThan(ctx, d.now().Add(-d.cfg.Retention.Duration))
	if err != nil {
		d.log.Error("housekeeping: list expired runs", "err", err)
		return
	}
	for _, id := range ids {
		if err := d.files.DeleteRunFiles(id); err != nil {
			d.log.Warn("housekeeping: delete run files", "run", id, "err", err)
			continue
		}
		if err := d.store.MarkLogsExpired(ctx, id); err != nil {
			d.log.Warn("housekeeping: mark expired", "run", id, "err", err)
		}
	}
	if err := d.files.Housekeep(14 * 24 * time.Hour); err != nil {
		d.log.Warn("housekeeping: prune environments", "err", err)
	}
}

// RestoreMetrics loads the drift gauges from the last finished main/nightly run.
func (d *Daemon) RestoreMetrics(ctx context.Context) {
	if d.metrics == nil {
		return
	}
	r, err := d.store.LatestFinished(ctx, model.KindMain, model.KindNightly)
	if err != nil {
		d.log.Warn("restore drift metrics", "err", err)
		return
	}
	d.metrics.RestoreDrift(r)
}

// Package engine runs one dry run of one commit: fetch, checkout, overlay,
// Python environment, collections, ansible-playbook --check --diff, and parsing
// of the callback plugin's events (DESIGN.md "Run engine" and "Result").
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/timothywashburn/ansibled/internal/config"
	"github.com/timothywashburn/ansibled/internal/model"
)

// DefaultCallbackDir is where the container image installs the callback plugin.
const DefaultCallbackDir = "/usr/share/ansibled/callback_plugins"

// CallbackDirEnv overrides DefaultCallbackDir when Options.CallbackDir is empty.
const CallbackDirEnv = "ANSIBLED_CALLBACK_DIR"

// markerName marks a fully installed venv, collections or roles directory. Its
// mtime is refreshed on every use so Housekeep can find unused ones.
const markerName = ".ansibled-ok"

// killGrace is how long a cancelled step gets between SIGTERM and SIGKILL
// (a variable so tests can shorten it).
var killGrace = 15 * time.Second

// Source is the part of forge.Forge the engine needs (forge.Forge satisfies it).
type Source interface {
	// GitAuth returns the HTTPS fetch URL and the Authorization header value.
	GitAuth() (fetchURL, authHeader string)
	// MRRef is the ref holding a merge request's head.
	MRRef(iid int) string
}

// Options configure an Engine.
type Options struct {
	Config *config.Config
	// Source supplies fetch URL, credentials and MR refs. It may be nil only
	// when Config.Git.CloneURL is set.
	Source Source
	// CallbackDir holds ansibled_events.py. Default: $ANSIBLED_CALLBACK_DIR,
	// else DefaultCallbackDir.
	CallbackDir string
	Logger      *slog.Logger
}

// Engine executes runs. It is safe for concurrent use, but runs are serialised
// by an exclusive flock on <data_dir>/engine.lock (also across processes).
type Engine struct {
	cfg         *config.Config
	src         Source
	callbackDir string
	log         *slog.Logger

	// daemonPath is the daemon's own PATH, appended to the child base PATH so
	// tools like uv, git and sh are found wherever they are installed.
	daemonPath string
	// passEnv are variables copied from the daemon's environment into every
	// child (proxies, CA bundles, uv settings).
	passEnv []string

	// onStart, if set, is called with the process group id of every child
	// process the engine starts (tests use it to check for leftovers).
	onStart func(step string, pgid int)
}

// New validates the options and returns an Engine.
func New(o Options) (*Engine, error) {
	if o.Config == nil {
		return nil, errors.New("engine: Config is required")
	}
	if o.Source == nil && o.Config.Git.CloneURL == "" {
		return nil, errors.New("engine: Source is required unless git.clone_url is set")
	}
	if !filepath.IsAbs(o.Config.DataDir) {
		return nil, fmt.Errorf("engine: data_dir %q must be absolute", o.Config.DataDir)
	}
	cb := o.CallbackDir
	if cb == "" {
		cb = os.Getenv(CallbackDirEnv)
	}
	if cb == "" {
		cb = DefaultCallbackDir
	}
	if abs, err := filepath.Abs(cb); err == nil {
		cb = abs
	}
	lg := o.Logger
	if lg == nil {
		lg = slog.Default()
	}
	if err := os.MkdirAll(o.Config.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	return &Engine{
		cfg:         o.Config,
		src:         o.Source,
		callbackDir: cb,
		log:         lg.With("component", "engine"),
		daemonPath:  os.Getenv("PATH"),
		passEnv:     passThroughEnv(os.Environ()),
	}, nil
}

// RunDir is <data_dir>/runs/<id>.
func (e *Engine) RunDir(id int64) string {
	return filepath.Join(e.cfg.DataDir, "runs", strconv.FormatInt(id, 10))
}

// LogPath is <data_dir>/runs/<id>/log.txt.
func (e *Engine) LogPath(id int64) string { return filepath.Join(e.RunDir(id), "log.txt") }

// EventsPath is <data_dir>/runs/<id>/events-<n>.jsonl for playbook index n.
func (e *Engine) EventsPath(id int64, n int) string {
	return filepath.Join(e.RunDir(id), fmt.Sprintf("events-%d.jsonl", n))
}

// Details reads the changed, failed and unreachable results of playbook n of
// run id from its events file. A missing file gives (nil, nil).
func (e *Engine) Details(id int64, n int) ([]model.TaskDetail, error) {
	f, err := os.Open(e.EventsPath(id, n))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseDetails(f)
}

// DeleteRunFiles removes a run's directory (log and events), for retention.
// A missing directory is not an error.
func (e *Engine) DeleteRunFiles(id int64) error { return os.RemoveAll(e.RunDir(id)) }

func (e *Engine) repoDir() string { return filepath.Join(e.cfg.DataDir, "repo.git") }
func (e *Engine) homeDir() string { return filepath.Join(e.cfg.DataDir, "home") }

// stepError is a failure of one engine step, reported as StateError.
type stepError struct {
	step string // model.Summary.FailedStep
	msg  string // short, no secrets
}

func (s *stepError) Error() string { return s.step + ": " + s.msg }

func stepErr(step, f string, a ...any) error {
	return &stepError{step: step, msg: fmt.Sprintf(f, a...)}
}

var errTimeout = errors.New("run timeout")

// Run executes the run and returns its final state and summary. It satisfies
// queue.Runner. Cancelling ctx yields StateCanceled; exceeding
// ansible.timeout yields StateError with FailedStep "timeout".
func (e *Engine) Run(ctx context.Context, r *model.Run) (model.State, *model.Summary) {
	lg := e.log.With("run", r.ID, "kind", r.Kind, "sha", model.Short(r.SHA))
	start := time.Now()
	sum := &model.Summary{Playbooks: []model.PlaybookResult{}}

	lock, err := acquireLock(ctx, filepath.Join(e.cfg.DataDir, "engine.lock"))
	if err != nil {
		if ctx.Err() != nil {
			return model.StateCanceled, sum
		}
		lg.Error("engine lock", "err", err)
		return errorResult(sum, "setup", "cannot lock engine: "+err.Error())
	}
	defer lock.release()

	runCtx, cancel := context.WithTimeoutCause(ctx, e.timeout(), errTimeout)
	defer cancel()

	rd := e.RunDir(r.ID)
	if err := os.MkdirAll(rd, 0o755); err != nil {
		return errorResult(sum, "setup", "cannot create run directory")
	}
	logf, err := os.OpenFile(e.LogPath(r.ID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o644)
	if err != nil {
		return errorResult(sum, "setup", "cannot create log file")
	}
	defer logf.Close()
	checkout := filepath.Join(rd, "checkout")
	defer func() {
		if err := os.RemoveAll(checkout); err != nil {
			lg.Warn("remove checkout", "err", err)
		}
	}()

	rc := &runCtxt{Engine: e, ctx: runCtx, run: r, log: logf, lg: lg, checkout: checkout}
	fmt.Fprintf(logf, "ansibled run %d: %s %s (%s) started %s\n", r.ID, r.Kind, r.SHA, r.Ref, stamp(start))
	lg.Info("run started")

	err = rc.execute(sum)

	var state model.State
	var se *stepError
	switch {
	case ctx.Err() != nil:
		state = model.StateCanceled
		fmt.Fprintf(logf, "\n*** run canceled\n")
	case runCtx.Err() != nil && errors.Is(context.Cause(runCtx), errTimeout):
		state = model.StateError
		sum.FailedStep = "timeout"
		sum.Error = "run exceeded timeout " + e.timeout().String()
		fmt.Fprintf(logf, "\n*** %s\n", sum.Error)
	case errors.As(err, &se):
		state = model.StateError
		sum.FailedStep, sum.Error = se.step, se.msg
		fmt.Fprintf(logf, "\n*** error in step %s: %s\n", se.step, se.msg)
	case err != nil:
		state = model.StateError
		sum.FailedStep, sum.Error = "ansible", err.Error()
	default:
		state = playbookState(sum)
		if state == model.StateError {
			fmt.Fprintf(logf, "\n*** error: %s\n", sum.Error)
		}
	}
	fmt.Fprintf(logf, "\nansibled run %d finished: %s in %s\n", r.ID, state, time.Since(start).Round(time.Millisecond))
	lg.Info("run finished", "state", state, "failed_step", sum.FailedStep, "duration", time.Since(start).Round(time.Millisecond))
	return state, sum
}

// timeout is ansible.timeout, or 45m if the config was not defaulted.
func (e *Engine) timeout() time.Duration {
	if d := e.cfg.Ansible.Timeout.Duration; d > 0 {
		return d
	}
	return 45 * time.Minute
}

func errorResult(sum *model.Summary, step, msg string) (model.State, *model.Summary) {
	sum.FailedStep, sum.Error = step, msg
	return model.StateError, sum
}

// runCtxt carries the state of one run through its steps.
type runCtxt struct {
	*Engine
	ctx      context.Context
	run      *model.Run
	log      io.Writer
	lg       *slog.Logger
	checkout string

	venv        string
	collections string // installed collections dir, "" if none
	roles       string // installed roles dir, "" if none
}

func (rc *runCtxt) workdir() string {
	return filepath.Join(rc.checkout, filepath.FromSlash(rc.cfg.Repo.Workdir))
}

// execute runs steps 1-7. A returned error is a *stepError or a context error;
// playbook outcomes are recorded in sum.
func (rc *runCtxt) execute(sum *model.Summary) error {
	if err := os.MkdirAll(rc.homeDir(), 0o700); err != nil {
		return stepErr("setup", "cannot create home directory")
	}
	steps := []func() error{rc.fetch, rc.checkoutTree, rc.overlay, rc.setupVenv, rc.setupCollections}
	for _, s := range steps {
		if err := s(); err != nil {
			return err
		}
		if err := rc.ctx.Err(); err != nil {
			return err
		}
	}
	// Every playbook runs even if an earlier one failed or errored.
	var firstErr error
	for n, pb := range rc.cfg.Ansible.Playbooks {
		res, err := rc.playbook(n, pb)
		sum.Playbooks = append(sum.Playbooks, res)
		if cerr := rc.ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// playbookState applies DESIGN.md "Result" to the collected playbook results.
// For StateError it also fills sum.FailedStep and sum.Error (first error wins).
func playbookState(sum *model.Summary) model.State {
	state := model.StateSuccess
	for _, p := range sum.Playbooks {
		switch p.ExitCode {
		case 0, 2, 4:
		default:
			if sum.FailedStep == "" {
				sum.FailedStep = "ansible"
				sum.Error = fmt.Sprintf("%s exited %d", p.Playbook, p.ExitCode)
			}
			state = model.StateError
			continue
		}
		if state == model.StateError {
			continue
		}
		if p.ExitCode != 0 {
			state = model.StateFailed
		}
		for _, h := range p.Hosts {
			if h.Failed > 0 || h.Unreachable {
				state = model.StateFailed
			}
		}
	}
	return state
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

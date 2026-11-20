// Package model holds the types shared between ansibled's packages.
package model

import (
	"fmt"
	"time"
)

// Kind is what started a run and decides where it is reported (DESIGN.md "Triggers and run kinds").
type Kind string

const (
	KindMR      Kind = "mr"
	KindMain    Kind = "main"
	KindNightly Kind = "nightly"
)

// State of a run.
type State string

const (
	StateQueued   State = "queued"
	StateRunning  State = "running"
	StateSuccess  State = "success"
	StateFailed   State = "failed"
	StateError    State = "error"
	StateCanceled State = "canceled"
)

// Finished reports whether the state is terminal.
func (s State) Finished() bool {
	return s == StateSuccess || s == StateFailed || s == StateError || s == StateCanceled
}

// Mode of a run. Only ModeCheck exists today.
type Mode string

const ModeCheck Mode = "check"

// Trigger says why a run was queued.
type Trigger string

const (
	TriggerPush     Trigger = "push"
	TriggerMROpen   Trigger = "mr_open"
	TriggerComment  Trigger = "comment"
	TriggerStartup  Trigger = "startup"
	TriggerSchedule Trigger = "schedule"
	TriggerManual   Trigger = "manual" // run-once CLI
	TriggerWeb      Trigger = "web"    // re-run button in the web UI
)

// Key returns the queue key for a kind: runs with the same key supersede each other.
func Key(kind Kind, mrIID int) string {
	if kind == KindMR {
		return fmt.Sprintf("mr:%d", mrIID)
	}
	return string(kind)
}

// Run is one dry run of one commit.
type Run struct {
	ID          int64
	Kind        Kind
	Key         string
	Mode        Mode
	SHA         string
	Ref         string // branch name, e.g. "main" or the MR source branch
	MRIID       int    // 0 unless Kind == KindMR
	Trigger     Trigger
	TriggeredBy string // forge username, empty for schedule/startup
	State       State
	Description string // short human text, also used for the commit status
	Summary     *Summary
	QueuedAt    time.Time
	StartedAt   time.Time // zero until started
	FinishedAt  time.Time // zero until finished
	LogsExpired bool
}

// ShortSHA returns the first 8 characters of the commit.
func (r *Run) ShortSHA() string { return Short(r.SHA) }

// Duration of the run, zero if it has not finished.
func (r *Run) Duration() time.Duration {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// Short abbreviates a commit sha to 8 characters.
func Short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// Summary is the parsed outcome of a run, per playbook.
type Summary struct {
	Playbooks []PlaybookResult `json:"playbooks"`
	// FailedStep is set when State == StateError: "fetch", "checkout", "overlay",
	// "setup", "collections", "ansible", "timeout".
	FailedStep string `json:"failed_step,omitempty"`
	Error      string `json:"error,omitempty"`
}

// PlaybookResult is the outcome of one ansible-playbook invocation.
type PlaybookResult struct {
	Playbook string       `json:"playbook"`
	ExitCode int          `json:"exit_code"`
	Hosts    []HostResult `json:"hosts"` // sorted by host name
}

// HostResult is one host's counts and task names in one playbook.
type HostResult struct {
	Host         string   `json:"host"`
	OK           int      `json:"ok"`
	Changed      int      `json:"changed"`
	Failed       int      `json:"failed"` // failures not covered by ignore_errors
	Unreachable  bool     `json:"unreachable"`
	Skipped      int      `json:"skipped"`
	Ignored      int      `json:"ignored"`
	Rescued      int      `json:"rescued"`
	ChangedTasks []string `json:"changed_tasks,omitempty"` // task names, in run order
	FailedTasks  []string `json:"failed_tasks,omitempty"`
}

// Totals adds up the summary across playbooks and hosts.
func (s *Summary) Totals() (changed, failed, unreachable, changedHosts, failedHosts int) {
	if s == nil {
		return
	}
	ch := map[string]bool{}
	fh := map[string]bool{}
	for _, p := range s.Playbooks {
		for _, h := range p.Hosts {
			changed += h.Changed
			failed += h.Failed
			if h.Unreachable {
				unreachable++
				fh[h.Host] = true
			}
			if h.Changed > 0 {
				ch[h.Host] = true
			}
			if h.Failed > 0 {
				fh[h.Host] = true
			}
		}
	}
	return changed, failed, unreachable, len(ch), len(fh)
}

// TaskDetail is one task result on one host that would change something or
// failed, read back from a run's events file for the web UI.
type TaskDetail struct {
	Host   string
	Play   string
	Task   string
	Action string
	Status string // "changed", "failed" or "unreachable"
	Diff   string // unified diff (changed results run with --diff), may be empty
	Msg    string // message of failed and unreachable results
}

// RunFilter narrows a run listing. Empty fields match everything.
type RunFilter struct {
	Kinds  []Kind
	States []State
	MRIID  int
}

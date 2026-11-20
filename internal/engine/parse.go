package engine

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"sort"

	"github.com/timothywashburn/ansibled/internal/model"
)

// event is one line written by callback/ansibled_events.py.
type event struct {
	Event        string                `json:"event"`
	Playbook     string                `json:"playbook"`
	Play         string                `json:"play"`
	Task         string                `json:"task"`
	Path         string                `json:"path"`
	Status       string                `json:"status"`
	Host         string                `json:"host"`
	Action       string                `json:"action"`
	IgnoreErrors bool                  `json:"ignore_errors"`
	Msg          string                `json:"msg"`
	Diff         string                `json:"diff"`
	Hosts        map[string]statsEntry `json:"hosts"`
}

// statsEntry is one host in the "stats" event (ansible's AggregateStats.summarize).
type statsEntry struct {
	OK          int `json:"ok"`
	Changed     int `json:"changed"`
	Failures    int `json:"failures"`
	Unreachable int `json:"unreachable"`
	Skipped     int `json:"skipped"`
	Rescued     int `json:"rescued"`
	Ignored     int `json:"ignored"`
}

// ParseEvents builds the result of one playbook from its events file and exit
// code. Counts come from the "stats" event; if it is missing (ansible was
// killed or crashed) they are counted from the result events instead. Task
// names come from the result events: ChangedTasks are results with status
// "changed", FailedTasks are "failed" results without ignore_errors (both in
// run order). Hosts are sorted by name. Malformed lines are skipped.
func ParseEvents(r io.Reader, playbook string, exitCode int) (model.PlaybookResult, error) {
	type acc struct{ h model.HostResult }
	hosts := map[string]*acc{}
	get := func(name string) *acc {
		a, ok := hosts[name]
		if !ok {
			a = &acc{h: model.HostResult{Host: name}}
			hosts[name] = a
		}
		return a
	}
	var stats map[string]statsEntry

	br := bufio.NewReader(r)
	for {
		line, rerr := br.ReadBytes('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return model.PlaybookResult{}, rerr
		}
		var ev event
		if len(line) == 0 || json.Unmarshal(line, &ev) != nil {
			ev = event{}
		}
		switch {
		case ev.Event == "result" && ev.Host != "":
			a := get(ev.Host)
			switch ev.Status {
			case "ok":
				a.h.OK++
			case "changed":
				a.h.OK++
				a.h.Changed++
				a.h.ChangedTasks = append(a.h.ChangedTasks, ev.Task)
			case "failed":
				if ev.IgnoreErrors {
					a.h.Ignored++
					a.h.OK++
				} else {
					a.h.Failed++
					a.h.FailedTasks = append(a.h.FailedTasks, ev.Task)
				}
			case "unreachable":
				a.h.Unreachable = true
			case "skipped":
				a.h.Skipped++
			}
		case ev.Event == "stats":
			stats = ev.Hosts
		}
		if rerr != nil {
			break
		}
	}

	for name, s := range stats {
		a := get(name)
		a.h.OK = s.OK
		a.h.Changed = s.Changed
		a.h.Failed = s.Failures
		a.h.Unreachable = s.Unreachable > 0
		a.h.Skipped = s.Skipped
		a.h.Rescued = s.Rescued
		a.h.Ignored = s.Ignored
		// Failures inside a rescued block are reported to callbacks but not
		// counted by ansible; don't list them when the host has none.
		if s.Failures == 0 {
			a.h.FailedTasks = nil
		}
	}

	res := model.PlaybookResult{Playbook: playbook, ExitCode: exitCode, Hosts: make([]model.HostResult, 0, len(hosts))}
	for _, a := range hosts {
		res.Hosts = append(res.Hosts, a.h)
	}
	sort.Slice(res.Hosts, func(i, j int) bool { return res.Hosts[i].Host < res.Hosts[j].Host })
	return res, nil
}

// ParseDetails returns the results of an events file that would change
// something (with their diffs), failed (without ignore_errors) or were
// unreachable, in run order. Malformed lines are skipped.
func ParseDetails(r io.Reader) ([]model.TaskDetail, error) {
	var out []model.TaskDetail
	br := bufio.NewReader(r)
	for {
		line, rerr := br.ReadBytes('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return out, rerr
		}
		var ev event
		if len(line) > 0 && json.Unmarshal(line, &ev) == nil && ev.Event == "result" && ev.Host != "" {
			keep := ev.Status == "changed" || ev.Status == "unreachable" || (ev.Status == "failed" && !ev.IgnoreErrors)
			if keep {
				d := model.TaskDetail{Host: ev.Host, Play: ev.Play, Task: ev.Task, Action: ev.Action, Status: ev.Status, Diff: ev.Diff}
				if ev.Status != "changed" {
					d.Msg = ev.Msg
				}
				out = append(out, d)
			}
		}
		if rerr != nil {
			return out, nil
		}
	}
}

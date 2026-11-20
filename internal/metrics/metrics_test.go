package metrics

import (
	"io"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/timothywashburn/ansibled/internal/model"
)

// values gathers one metric family as "label=value,..." -> value.
func values(t *testing.T, m *Metrics, name string) map[string]float64 {
	t.Helper()
	fams, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, mt := range f.GetMetric() {
			var ls []string
			for _, l := range mt.GetLabel() {
				ls = append(ls, l.GetName()+"="+l.GetValue())
			}
			sort.Strings(ls)
			out[strings.Join(ls, ",")] = value(mt)
		}
	}
	return out
}

func value(m *dto.Metric) float64 {
	switch {
	case m.Gauge != nil:
		return m.Gauge.GetValue()
	case m.Counter != nil:
		return m.Counter.GetValue()
	case m.Histogram != nil:
		return float64(m.Histogram.GetSampleCount())
	}
	return 0
}

func finished(kind model.Kind, state model.State, s *model.Summary) *model.Run {
	start := time.Unix(1000, 0)
	return &model.Run{Kind: kind, State: state, Summary: s, StartedAt: start, FinishedAt: start.Add(90 * time.Second)}
}

func TestRunFinished(t *testing.T) {
	m := New()
	m.RunFinished(finished(model.KindMR, model.StateSuccess, nil))
	m.RunFinished(finished(model.KindMR, model.StateFailed, nil))
	m.RunFinished(&model.Run{Kind: model.KindMR, State: model.StateCanceled, FinishedAt: time.Unix(2000, 0)}) // never started

	runs := values(t, m, "ansibled_runs_total")
	if runs["kind=mr,state=success"] != 1 || runs["kind=mr,state=failed"] != 1 || runs["kind=mr,state=canceled"] != 1 {
		t.Errorf("runs_total = %v", runs)
	}
	if _, ok := runs["kind=nightly,state=error"]; !ok {
		t.Errorf("counters not pre-created: %v", runs)
	}
	if d := values(t, m, "ansibled_run_duration_seconds"); d["kind=mr"] != 2 {
		t.Errorf("duration samples = %v, want 2", d)
	}
	if ts := values(t, m, "ansibled_last_run_timestamp_seconds"); ts["kind=mr"] != 2000 {
		t.Errorf("last_run_timestamp = %v", ts)
	}
	if s := values(t, m, "ansibled_last_run_success"); s["kind=mr"] != 0 {
		t.Errorf("last_run_success = %v", s)
	}
	// mr runs never touch drift.
	if d := values(t, m, "ansibled_drift_changed_tasks"); len(d) != 0 {
		t.Errorf("drift set by mr run: %v", d)
	}
}

func TestDrift(t *testing.T) {
	m := New()
	first := finished(model.KindMain, model.StateFailed, &model.Summary{Playbooks: []model.PlaybookResult{
		{Playbook: "site.yml", Hosts: []model.HostResult{
			{Host: "a", Changed: 3}, {Host: "b", Failed: 2}, {Host: "c", Unreachable: true},
		}},
		{Playbook: "other.yml", Hosts: []model.HostResult{{Host: "a", Changed: 1}}},
	}})
	m.RunFinished(first)
	if got := values(t, m, "ansibled_drift_changed_tasks"); got["host=a,playbook=site.yml"] != 3 || got["host=a,playbook=other.yml"] != 1 || len(got) != 4 {
		t.Errorf("changed = %v", got)
	}
	if got := values(t, m, "ansibled_drift_failed_tasks"); got["host=b,playbook=site.yml"] != 2 {
		t.Errorf("failed = %v", got)
	}
	if got := values(t, m, "ansibled_drift_unreachable"); got["host=c,playbook=site.yml"] != 1 || got["host=a,playbook=site.yml"] != 0 {
		t.Errorf("unreachable = %v", got)
	}
	if got := values(t, m, "ansibled_drift_run_timestamp_seconds"); got[""] != 1090 {
		t.Errorf("drift ts = %v", got)
	}

	// Host c removed from site.yml: it must disappear; other.yml stays.
	second := finished(model.KindNightly, model.StateSuccess, &model.Summary{Playbooks: []model.PlaybookResult{
		{Playbook: "site.yml", Hosts: []model.HostResult{{Host: "a"}, {Host: "b"}}},
	}})
	second.FinishedAt = time.Unix(5000, 0)
	m.RunFinished(second)
	got := values(t, m, "ansibled_drift_unreachable")
	if _, ok := got["host=c,playbook=site.yml"]; ok {
		t.Errorf("removed host still present: %v", got)
	}
	if _, ok := got["host=a,playbook=other.yml"]; !ok {
		t.Errorf("other playbook reset: %v", got)
	}
	if got := values(t, m, "ansibled_drift_run_timestamp_seconds"); got[""] != 5000 {
		t.Errorf("drift ts = %v", got)
	}

	// Errors don't touch drift.
	m.RunFinished(finished(model.KindMain, model.StateError, &model.Summary{FailedStep: "fetch"}))
	if got := values(t, m, "ansibled_drift_run_timestamp_seconds"); got[""] != 5000 {
		t.Errorf("error run updated drift: %v", got)
	}
}

func TestRestoreDrift(t *testing.T) {
	m := New()
	m.RestoreDrift(nil)
	m.RestoreDrift(finished(model.KindMain, model.StateSuccess, &model.Summary{Playbooks: []model.PlaybookResult{
		{Playbook: "site.yml", Hosts: []model.HostResult{{Host: "a", Changed: 2}}},
	}}))
	if got := values(t, m, "ansibled_drift_changed_tasks"); got["host=a,playbook=site.yml"] != 2 {
		t.Errorf("changed = %v", got)
	}
	if got := values(t, m, "ansibled_runs_total"); got["kind=main,state=success"] != 0 {
		t.Errorf("restore counted a run: %v", got)
	}
}

func TestQueueWebhookHandler(t *testing.T) {
	m := New()
	m.SetQueueLength(3)
	m.Webhook("Push Hook", true)
	m.Webhook("unauthorized", false)
	if got := values(t, m, "ansibled_queue_length"); got[""] != 3 {
		t.Errorf("queue = %v", got)
	}
	if got := values(t, m, "ansibled_webhooks_total"); got["accepted=true,event=Push Hook"] != 1 || got["accepted=false,event=unauthorized"] != 1 {
		t.Errorf("webhooks = %v", got)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, s := range []string{"ansibled_queue_length 3", "go_goroutines"} {
		if !strings.Contains(string(body), s) {
			t.Errorf("exposition missing %q", s)
		}
	}
}

func TestNilSafe(t *testing.T) {
	var m *Metrics
	m.RunFinished(&model.Run{})
	m.RestoreDrift(&model.Run{})
	m.SetQueueLength(1)
	m.Webhook("x", true)
	_ = m.Handler()
}

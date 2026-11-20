// Package metrics holds ansibled's Prometheus collectors (DESIGN.md "Metrics").
//
// All methods are safe for concurrent use and on a nil *Metrics (they do
// nothing), so callers and tests can run without metrics.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/timothywashburn/ansibled/internal/model"
)

// Metrics is ansibled's own registry and collectors.
type Metrics struct {
	reg *prometheus.Registry

	driftChanged     *prometheus.GaugeVec
	driftFailed      *prometheus.GaugeVec
	driftUnreachable *prometheus.GaugeVec
	driftTimestamp   prometheus.Gauge

	lastRunTimestamp *prometheus.GaugeVec
	lastRunSuccess   *prometheus.GaugeVec
	runsTotal        *prometheus.CounterVec
	runDuration      *prometheus.HistogramVec
	queueLength      prometheus.Gauge
	webhooks         *prometheus.CounterVec

	// driftMu serializes drift updates so two concurrent reset-then-set
	// sequences cannot interleave.
	driftMu sync.Mutex
}

// New creates the collectors on a fresh registry that also carries the Go and
// process collectors.
func New() *Metrics {
	hostLabels := []string{"playbook", "host"}
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		driftChanged: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ansibled_drift_changed_tasks",
			Help: "Tasks that would change, from the latest finished main/nightly run.",
		}, hostLabels),
		driftFailed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ansibled_drift_failed_tasks",
			Help: "Failed tasks, from the latest finished main/nightly run.",
		}, hostLabels),
		driftUnreachable: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ansibled_drift_unreachable",
			Help: "1 if the host was unreachable in the latest finished main/nightly run.",
		}, hostLabels),
		driftTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ansibled_drift_run_timestamp_seconds",
			Help: "When the drift gauges were last updated (finish time of that run).",
		}),
		lastRunTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ansibled_last_run_timestamp_seconds",
			Help: "When the last run of this kind finished.",
		}, []string{"kind"}),
		lastRunSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ansibled_last_run_success",
			Help: "1 if the last finished run of this kind succeeded.",
		}, []string{"kind"}),
		runsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ansibled_runs_total",
			Help: "Finished runs by kind and state.",
		}, []string{"kind", "state"}),
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ansibled_run_duration_seconds",
			Help:    "Duration of finished runs that started.",
			Buckets: []float64{10, 30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600},
		}, []string{"kind"}),
		queueLength: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ansibled_queue_length",
			Help: "Runs waiting in the queue.",
		}),
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ansibled_webhooks_total",
			Help: "Webhook events received; accepted=false for bad token, untrusted or ignored.",
		}, []string{"event", "accepted"}),
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.driftChanged, m.driftFailed, m.driftUnreachable, m.driftTimestamp,
		m.lastRunTimestamp, m.lastRunSuccess, m.runsTotal, m.runDuration,
		m.queueLength, m.webhooks,
	)
	// Pre-create the run counters so rate() and absent() work from the start.
	for _, k := range []model.Kind{model.KindMR, model.KindMain, model.KindNightly} {
		for _, s := range []model.State{model.StateSuccess, model.StateFailed, model.StateError, model.StateCanceled} {
			m.runsTotal.WithLabelValues(string(k), string(s))
		}
	}
	return m
}

// Registry returns the registry, for callers that add their own collectors.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

// Handler serves the registry in the Prometheus exposition format. It does
// no authentication; the web package does that.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// RunFinished records a finished run: counters, duration (if it started),
// last-run gauges and, for main/nightly runs that produced a result, the drift gauges.
func (m *Metrics) RunFinished(r *model.Run) {
	if m == nil || r == nil {
		return
	}
	kind := string(r.Kind)
	m.runsTotal.WithLabelValues(kind, string(r.State)).Inc()
	if !r.StartedAt.IsZero() {
		end := r.FinishedAt
		if end.IsZero() {
			end = time.Now()
		}
		if d := end.Sub(r.StartedAt); d >= 0 {
			m.runDuration.WithLabelValues(kind).Observe(d.Seconds())
		}
	}
	m.lastRunTimestamp.WithLabelValues(kind).Set(unix(finishTime(r)))
	success := 0.0
	if r.State == model.StateSuccess {
		success = 1
	}
	m.lastRunSuccess.WithLabelValues(kind).Set(success)
	m.RestoreDrift(r)
}

// RestoreDrift updates the drift gauges from r without touching the counters.
// It is used on startup with the last finished main/nightly run, and does
// nothing for nil runs or runs that don't carry drift (mr runs, errors,
// cancellations, missing summary).
func (m *Metrics) RestoreDrift(r *model.Run) {
	if m == nil || r == nil || r.Summary == nil {
		return
	}
	if r.Kind != model.KindMain && r.Kind != model.KindNightly {
		return
	}
	if r.State != model.StateSuccess && r.State != model.StateFailed {
		return
	}
	m.driftMu.Lock()
	defer m.driftMu.Unlock()
	for _, p := range r.Summary.Playbooks {
		sel := prometheus.Labels{"playbook": p.Playbook}
		m.driftChanged.DeletePartialMatch(sel)
		m.driftFailed.DeletePartialMatch(sel)
		m.driftUnreachable.DeletePartialMatch(sel)
		for _, h := range p.Hosts {
			m.driftChanged.WithLabelValues(p.Playbook, h.Host).Set(float64(h.Changed))
			m.driftFailed.WithLabelValues(p.Playbook, h.Host).Set(float64(h.Failed))
			unreachable := 0.0
			if h.Unreachable {
				unreachable = 1
			}
			m.driftUnreachable.WithLabelValues(p.Playbook, h.Host).Set(unreachable)
		}
	}
	m.driftTimestamp.Set(unix(finishTime(r)))
}

// SetQueueLength sets the number of queued runs.
func (m *Metrics) SetQueueLength(n int) {
	if m == nil {
		return
	}
	m.queueLength.Set(float64(n))
}

// Webhook counts one webhook event. event is the forge's event name (or
// "unauthorized"/"invalid" for requests that could not be parsed).
func (m *Metrics) Webhook(event string, accepted bool) {
	if m == nil {
		return
	}
	m.webhooks.WithLabelValues(event, strconv.FormatBool(accepted)).Inc()
}

func finishTime(r *model.Run) time.Time {
	if r.FinishedAt.IsZero() {
		return time.Now()
	}
	return r.FinishedAt
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

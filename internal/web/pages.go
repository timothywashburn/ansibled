package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/timothywashburn/ansibled/internal/model"
	"github.com/timothywashburn/ansibled/internal/report"
)

//go:embed templates/*.html
var templateFS embed.FS

// pages holds one template set per page, each with the shared layout.
type pages struct {
	overview, runs, run, message *template.Template
}

func loadPages(a *assets) (*pages, error) {
	funcs := template.FuncMap{
		"asset":     a.url,
		"icon":      icon,
		"diffLines": diffLines,
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
	}
	parse := func(name string) (*template.Template, error) {
		return template.New("layout.html").Funcs(funcs).ParseFS(templateFS,
			"templates/layout.html", "templates/parts.html", "templates/"+name)
	}
	var p pages
	var err error
	for _, x := range []struct {
		t    **template.Template
		name string
	}{{&p.overview, "overview.html"}, {&p.runs, "runs.html"}, {&p.run, "run.html"}, {&p.message, "message.html"}} {
		if *x.t, err = parse(x.name); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// base is what the layout needs.
type base struct {
	Title string
	Site  string
	User  string // empty when not signed in
	CSRF  string // form token, empty when not signed in
	Nav   string // "overview", "runs" or ""
}

type messagePage struct {
	base
	Message  string
	Link     string
	LinkText string
}

// runRow is a run formatted for the templates.
type runRow struct {
	ID          int64
	Kind        model.Kind
	MR          string
	MRURL       string
	Ref         string
	SHA         string
	FullSHA     string
	CommitURL   string
	Trigger     model.Trigger
	By          string
	State       model.State
	Active      bool
	Description string
	Queued      string // absolute, in the site's time zone
	Started     string
	Finished    string
	QueuedISO   string // RFC 3339, for relative times in the browser
	StartedISO  string
	FinishedISO string
	Duration    string
	// Counts across playbooks (finished runs with a summary).
	HasSummary   bool
	ChangedHosts int
	FailedHosts  int
	Unreachable  int
}

type runsPage struct {
	base
	Runs    []runRow
	Kind    string
	State   string
	MR      string
	Kinds   []string
	States  []string
	Page    int
	PrevURL string
	NextURL string
	Filters bool // any filter set
}

type overviewPage struct {
	base
	Active        []runRow
	LatestMain    *runRow
	LatestNightly *runRow
	DriftFrom     *runRow // the run the drift table comes from
	Drift         []driftRow
	HostsTotal    int
	HostsDrifted  int
	HostsFailed   int
	HostsDown     int
	Recent        []runRow
}

type driftRow struct {
	Host        string
	Changed     int
	Failed      int
	Unreachable bool
}

type runPage struct {
	base
	Run          runRow
	Summary      *model.Summary
	Playbooks    []playbookView
	ChangedTasks int
	FailedTasks  int
	DetailsError bool
	LogExpired   bool
	LogMissing   bool
	CanAct       bool
	States       []model.State // for the live badge
}

type playbookView struct {
	Playbook string
	ExitCode int
	Hosts    []model.HostResult
	Details  []hostDetails // hosts with changed or failed results
}

type hostDetails struct {
	Host    string
	Changed int
	Failed  int
	Items   []model.TaskDetail
}

// render executes t into a buffer first so template errors give a clean 500.
func (s *Server) render(w http.ResponseWriter, status int, t *template.Template, data any) {
	s.renderNamed(w, status, t, "layout.html", data)
}

func (s *Server) renderNamed(w http.ResponseWriter, status int, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render page", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// message renders a simple page with a title, a sentence and an optional link.
func (s *Server) message(w http.ResponseWriter, _ *http.Request, sess *session, status int, title, msg, link, linkText string) {
	s.render(w, status, s.pages.message, messagePage{
		base: s.base(sess, title, ""), Message: msg, Link: link, LinkText: linkText,
	})
}

func (s *Server) base(sess *session, title, nav string) base {
	b := base{Title: title, Site: s.cfg.Site, Nav: nav}
	if sess != nil {
		b.User = sess.Username
		b.CSRF = s.csrfToken(sess)
	}
	return b
}

// partial reports whether the browser asked for just the live part of a
// page (it refreshes it in place).
func partial(r *http.Request) bool { return r.URL.Query().Get("partial") == "1" }

// ---------------------------------------------------------------------------
// Overview

func (s *Server) overview(w http.ResponseWriter, r *http.Request, sess *session) {
	ctx := r.Context()
	p := overviewPage{base: s.base(sess, "Overview", "overview")}
	fail := func(err error) {
		s.log.Error("overview", "err", err)
		s.message(w, r, sess, http.StatusInternalServerError, "Error", "Could not load the overview.", "", "")
	}
	active, err := s.o.Runs.ListByState(ctx, model.StateRunning, model.StateQueued)
	if err != nil {
		fail(err)
		return
	}
	sort.SliceStable(active, func(i, j int) bool { // running first, then queue order
		return active[i].State == model.StateRunning && active[j].State != model.StateRunning
	})
	for _, run := range active {
		p.Active = append(p.Active, s.row(run))
	}
	mainRun, err := s.o.Runs.LatestFinished(ctx, model.KindMain)
	if err != nil {
		fail(err)
		return
	}
	nightly, err := s.o.Runs.LatestFinished(ctx, model.KindNightly)
	if err != nil {
		fail(err)
		return
	}
	if mainRun != nil {
		row := s.row(mainRun)
		p.LatestMain = &row
	}
	if nightly != nil {
		row := s.row(nightly)
		p.LatestNightly = &row
	}
	// Drift comes from the latest nightly run, or main's until there is one.
	src, srcRow := nightly, p.LatestNightly
	if src == nil || src.Summary == nil {
		src, srcRow = mainRun, p.LatestMain
	}
	if src != nil && src.Summary != nil {
		p.DriftFrom = srcRow
		p.Drift = drift(src.Summary)
		for _, d := range p.Drift {
			p.HostsTotal++
			if d.Changed > 0 {
				p.HostsDrifted++
			}
			if d.Failed > 0 {
				p.HostsFailed++
			}
			if d.Unreachable {
				p.HostsDown++
			}
		}
	}
	recent, err := s.o.Runs.FilterRuns(ctx, model.RunFilter{}, 10, 0)
	if err != nil {
		fail(err)
		return
	}
	for _, run := range recent {
		p.Recent = append(p.Recent, s.row(run))
	}
	if partial(r) {
		s.renderNamed(w, http.StatusOK, s.pages.overview, "overview-live", p)
		return
	}
	s.render(w, http.StatusOK, s.pages.overview, p)
}

// drift sums a summary per host across playbooks: hosts that failed or were
// unreachable first, then hosts that would change, then by name.
func drift(sum *model.Summary) []driftRow {
	m := map[string]*driftRow{}
	for _, pb := range sum.Playbooks {
		for _, h := range pb.Hosts {
			d := m[h.Host]
			if d == nil {
				d = &driftRow{Host: h.Host}
				m[h.Host] = d
			}
			d.Changed += h.Changed
			d.Failed += h.Failed
			d.Unreachable = d.Unreachable || h.Unreachable
		}
	}
	out := make([]driftRow, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	rank := func(d driftRow) int {
		switch {
		case d.Failed > 0 || d.Unreachable:
			return 0
		case d.Changed > 0:
			return 1
		}
		return 2
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := rank(out[i]), rank(out[j]); a != b {
			return a < b
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// ---------------------------------------------------------------------------
// Run list

var (
	kindOptions  = []string{string(model.KindMR), string(model.KindMain), string(model.KindNightly)}
	stateOptions = []string{
		string(model.StateRunning), string(model.StateQueued), string(model.StateSuccess),
		string(model.StateFailed), string(model.StateError), string(model.StateCanceled),
	}
)

func oneOf(v string, opts []string) string {
	for _, o := range opts {
		if v == o {
			return v
		}
	}
	return ""
}

func (s *Server) runList(w http.ResponseWriter, r *http.Request, sess *session) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 1)
	p := runsPage{
		base: s.base(sess, "Runs", "runs"), Page: page, Kinds: kindOptions, States: stateOptions,
		Kind: oneOf(q.Get("kind"), kindOptions), State: oneOf(q.Get("state"), stateOptions),
	}
	var f model.RunFilter
	if p.Kind != "" {
		f.Kinds = []model.Kind{model.Kind(p.Kind)}
	}
	if p.State != "" {
		f.States = []model.State{model.State(p.State)}
	}
	if mr, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(q.Get("mr")), "!")); err == nil && mr > 0 {
		f.MRIID = mr
		p.MR = strconv.Itoa(mr)
	}
	p.Filters = p.Kind != "" || p.State != "" || p.MR != ""

	runs, err := s.o.Runs.FilterRuns(r.Context(), f, runsPerPage+1, (page-1)*runsPerPage)
	if err != nil {
		s.log.Error("list runs", "err", err)
		s.message(w, r, sess, http.StatusInternalServerError, "Error", "Could not load runs.", "", "")
		return
	}
	pageURL := func(n int) string {
		v := url.Values{}
		for k, val := range map[string]string{"kind": p.Kind, "state": p.State, "mr": p.MR} {
			if val != "" {
				v.Set(k, val)
			}
		}
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		if len(v) == 0 {
			return "/runs"
		}
		return "/runs?" + v.Encode()
	}
	if len(runs) > runsPerPage {
		runs = runs[:runsPerPage]
		p.NextURL = pageURL(page + 1)
	}
	if page > 1 {
		p.PrevURL = pageURL(page - 1)
	}
	for _, run := range runs {
		p.Runs = append(p.Runs, s.row(run))
	}
	if partial(r) {
		s.renderNamed(w, http.StatusOK, s.pages.runs, "runs-live", p)
		return
	}
	s.render(w, http.StatusOK, s.pages.runs, p)
}

// lookupRun parses {id} and loads the run, writing an error page if it can't.
func (s *Server) lookupRun(w http.ResponseWriter, r *http.Request, sess *session) *model.Run {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var run *model.Run
	if err == nil && id > 0 {
		run, err = s.o.Runs.GetRun(r.Context(), id)
		if err != nil {
			s.log.Error("get run", "id", id, "err", err)
			s.message(w, r, sess, http.StatusInternalServerError, "Error", "Could not load the run.", "/runs", "All runs")
			return nil
		}
	}
	if run == nil {
		s.message(w, r, sess, http.StatusNotFound, "Not found", "There is no such run.", "/runs", "All runs")
		return nil
	}
	return run
}

// ---------------------------------------------------------------------------
// Run page

func (s *Server) runPage(w http.ResponseWriter, r *http.Request, sess *session) {
	run := s.lookupRun(w, r, sess)
	if run == nil {
		return
	}
	p := runPage{
		base:       s.base(sess, fmt.Sprintf("Run %d", run.ID), "runs"),
		Run:        s.row(run),
		Summary:    run.Summary,
		LogExpired: run.LogsExpired,
		CanAct:     s.o.Actions != nil,
		States: []model.State{model.StateQueued, model.StateRunning, model.StateSuccess,
			model.StateFailed, model.StateError, model.StateCanceled},
	}
	if !run.LogsExpired {
		if _, err := os.Stat(s.o.LogPath(run.ID)); err != nil {
			p.LogMissing = true
		}
	}
	if run.Summary != nil {
		for n, pb := range run.Summary.Playbooks {
			v := playbookView{Playbook: pb.Playbook, ExitCode: pb.ExitCode, Hosts: pb.Hosts}
			if s.o.Details != nil && !run.LogsExpired {
				items, err := s.o.Details(run.ID, n)
				if err != nil {
					s.log.Warn("read run details", "run", run.ID, "playbook", n, "err", err)
					p.DetailsError = true
				}
				v.Details = groupDetails(items)
			}
			if len(v.Details) == 0 {
				v.Details = detailsFromSummary(pb.Hosts)
			}
			for _, h := range v.Details {
				p.ChangedTasks += h.Changed
				p.FailedTasks += h.Failed
			}
			p.Playbooks = append(p.Playbooks, v)
		}
	}
	s.render(w, http.StatusOK, s.pages.run, p)
}

// detailsFromSummary lists the task names of a summary when no events file
// is available (no diffs or messages then).
func detailsFromSummary(hosts []model.HostResult) []hostDetails {
	var items []model.TaskDetail
	for _, h := range hosts {
		for _, t := range h.ChangedTasks {
			items = append(items, model.TaskDetail{Host: h.Host, Task: t, Status: "changed"})
		}
		for _, t := range h.FailedTasks {
			items = append(items, model.TaskDetail{Host: h.Host, Task: t, Status: "failed"})
		}
	}
	return groupDetails(items)
}

// groupDetails groups results by host (sorted), keeping run order per host.
func groupDetails(items []model.TaskDetail) []hostDetails {
	idx := map[string]int{}
	var out []hostDetails
	for _, it := range items {
		i, ok := idx[it.Host]
		if !ok {
			i = len(out)
			idx[it.Host] = i
			out = append(out, hostDetails{Host: it.Host})
		}
		h := &out[i]
		h.Items = append(h.Items, it)
		if it.Status == "changed" {
			h.Changed++
		} else {
			h.Failed++
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// diffLine is one line of a unified diff with its kind for styling.
type diffLine struct {
	Class string // "add", "del", "hunk", "file" or ""
	Text  string
}

func diffLines(diff string) []diffLine {
	diff = strings.TrimRight(diff, "\n")
	if diff == "" {
		return nil
	}
	lines := strings.Split(diff, "\n")
	out := make([]diffLine, len(lines))
	for i, l := range lines {
		c := ""
		switch {
		case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---"):
			c = "file"
		case strings.HasPrefix(l, "+"):
			c = "add"
		case strings.HasPrefix(l, "-"):
			c = "del"
		case strings.HasPrefix(l, "@@"):
			c = "hunk"
		}
		out[i] = diffLine{Class: c, Text: l}
	}
	return out
}

// ansiEscape matches terminal escape sequences (ansible's colours).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// runLog serves the raw log: as plain text without colour codes, or with
// them when ?ansi=1. ?download=1 makes it an attachment.
func (s *Server) runLog(w http.ResponseWriter, r *http.Request, sess *session) {
	run := s.lookupRun(w, r, sess)
	if run == nil {
		return
	}
	if run.LogsExpired {
		s.message(w, r, sess, http.StatusGone, "Log expired", "This run's log was removed by retention.", fmt.Sprintf("/runs/%d", run.ID), "Back to the run")
		return
	}
	f, err := os.Open(s.o.LogPath(run.ID))
	if err != nil {
		s.message(w, r, sess, http.StatusNotFound, "No log", "This run has no log yet.", fmt.Sprintf("/runs/%d", run.ID), "Back to the run")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ansibled-%s-run-%d.log"`, s.cfg.Site, run.ID))
	}
	if r.URL.Query().Get("ansi") == "1" {
		http.ServeContent(w, r, "", st.ModTime(), f)
		return
	}
	b, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(ansiEscape.ReplaceAll(b, nil))
}

// row formats a run for the templates.
func (s *Server) row(r *model.Run) runRow {
	row := runRow{
		ID: r.ID, Kind: r.Kind, Ref: r.Ref, SHA: r.ShortSHA(), FullSHA: r.SHA,
		Trigger: r.Trigger, By: r.TriggeredBy, State: r.State, Active: !r.State.Finished(),
		Description: r.Description,
		Queued:      s.fmtTime(r.QueuedAt),
		Started:     s.fmtTime(r.StartedAt),
		Finished:    s.fmtTime(r.FinishedAt),
		QueuedISO:   isoTime(r.QueuedAt),
		StartedISO:  isoTime(r.StartedAt),
		FinishedISO: isoTime(r.FinishedAt),
	}
	if row.Description == "" {
		row.Description = report.Describe(r)
	}
	if r.MRIID > 0 {
		row.MR = "!" + strconv.Itoa(r.MRIID)
	}
	if s.links != nil {
		row.CommitURL = s.links.CommitURL(r.SHA)
		if r.MRIID > 0 {
			row.MRURL = s.links.MRURL(r.MRIID)
		}
	}
	switch {
	case r.Duration() > 0:
		row.Duration = report.FormatDuration(r.Duration())
	case r.State == model.StateRunning && !r.StartedAt.IsZero():
		row.Duration = report.FormatDuration(s.now().Sub(r.StartedAt).Truncate(time.Second))
	}
	if r.Summary != nil && len(r.Summary.Playbooks) > 0 {
		row.HasSummary = true
		_, _, row.Unreachable, row.ChangedHosts, row.FailedHosts = r.Summary.Totals()
	}
	return row
}

func isoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Server) fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(s.cfg.Location()).Format("2006-01-02 15:04:05 MST")
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.ToValidUTF8(string([]rune(s)[:n-1]), "") + "…"
}

// icon returns an inline SVG icon (16px, currentColor).
func icon(name string) template.HTML {
	if p, ok := iconPaths[name]; ok {
		return template.HTML(`<svg class="i i-` + name + `" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">` + p + `</svg>`)
	}
	return ""
}

var iconPaths = map[string]string{
	"success":  `<circle cx="8" cy="8" r="7" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M5 8.2l2 2 4-4.4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>`,
	"failed":   `<circle cx="8" cy="8" r="7" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M5.6 5.6l4.8 4.8M10.4 5.6l-4.8 4.8" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/>`,
	"error":    `<path d="M8 1.5l6.8 12H1.2z" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/><path d="M8 6v3.4" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/><circle cx="8" cy="11.6" r=".9" fill="currentColor"/>`,
	"running":  `<circle cx="8" cy="8" r="6.2" fill="none" stroke="currentColor" stroke-opacity=".25" stroke-width="2"/><path d="M8 1.8a6.2 6.2 0 0 1 6.2 6.2" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>`,
	"queued":   `<circle cx="8" cy="8" r="7" fill="none" stroke="currentColor" stroke-width="1.5" stroke-dasharray="2.2 2"/><path d="M8 4.6V8l2.2 1.4" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>`,
	"canceled": `<circle cx="8" cy="8" r="7" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M3.2 12.8l9.6-9.6" stroke="currentColor" stroke-width="1.5"/>`,
	"commit":   `<circle cx="8" cy="8" r="2.6" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M1 8h4.4M10.6 8H15" stroke="currentColor" stroke-width="1.5"/>`,
	"mr":       `<circle cx="4" cy="3.5" r="1.8" fill="none" stroke="currentColor" stroke-width="1.4"/><circle cx="4" cy="12.5" r="1.8" fill="none" stroke="currentColor" stroke-width="1.4"/><circle cx="12" cy="12.5" r="1.8" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M4 5.3v5.4M12 10.7V6.5a2 2 0 0 0-2-2H7.5" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M9 3l-1.6 1.5L9 6" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/>`,
	"branch":   `<circle cx="4.5" cy="3.5" r="1.8" fill="none" stroke="currentColor" stroke-width="1.4"/><circle cx="4.5" cy="12.5" r="1.8" fill="none" stroke="currentColor" stroke-width="1.4"/><circle cx="11.5" cy="5" r="1.8" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M4.5 5.3v5.4M11.5 6.8c0 3-7 1.8-7 3.9" fill="none" stroke="currentColor" stroke-width="1.4"/>`,
	"clock":    `<circle cx="8" cy="8" r="6.5" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M8 4.5V8l2.4 1.6" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/>`,
	"user":     `<circle cx="8" cy="5.5" r="2.8" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M2.5 14c.6-3 2.9-4.4 5.5-4.4s4.9 1.4 5.5 4.4" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/>`,
	"rerun":    `<path d="M13.2 8A5.2 5.2 0 1 1 11.6 4.3" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/><path d="M12.4 1.6v3.2H9.2" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>`,
	"stop":     `<rect x="3.5" y="3.5" width="9" height="9" rx="1.5" fill="none" stroke="currentColor" stroke-width="1.6"/>`,
	"external": `<path d="M9.5 2.5h4v4M13.5 2.5L7.5 8.5M12 9.5v3a1 1 0 0 1-1 1H3.5a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1h3" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/>`,
	"chevron":  `<path d="M6 4l4 4-4 4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>`,
	"host":     `<rect x="2" y="2.5" width="12" height="4.5" rx="1" fill="none" stroke="currentColor" stroke-width="1.4"/><rect x="2" y="9" width="12" height="4.5" rx="1" fill="none" stroke="currentColor" stroke-width="1.4"/><circle cx="4.6" cy="4.75" r=".8" fill="currentColor"/><circle cx="4.6" cy="11.25" r=".8" fill="currentColor"/>`,
}

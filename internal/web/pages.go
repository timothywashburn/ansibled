package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
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
	runs, run, message *template.Template
}

func loadPages() (*pages, error) {
	parse := func(name string) (*template.Template, error) {
		return template.New("layout.html").ParseFS(templateFS, "templates/layout.html", "templates/"+name)
	}
	var p pages
	var err error
	if p.runs, err = parse("runs.html"); err != nil {
		return nil, err
	}
	if p.run, err = parse("run.html"); err != nil {
		return nil, err
	}
	if p.message, err = parse("message.html"); err != nil {
		return nil, err
	}
	return &p, nil
}

// base is what the layout needs.
type base struct {
	Title   string
	Site    string
	User    string // empty when not signed in
	Refresh bool
}

type messagePage struct {
	base
	Message  string
	Link     string
	LinkText string
}

type runsPage struct {
	base
	Runs     []runRow
	Page     int
	PrevPage int // 0 if none
	NextPage int // 0 if none
}

type runRow struct {
	ID          int64
	Kind        model.Kind
	MR          string
	Ref         string
	SHA         string
	FullSHA     string
	Trigger     model.Trigger
	By          string
	State       model.State
	Description string
	Queued      string
	Started     string
	Finished    string
	Duration    string
}

type runPage struct {
	base
	Run     runRow
	Summary *model.Summary
	Log     logView
}

type logView struct {
	Expired   bool
	Missing   bool
	Truncated bool
	Size      string
	Shown     string
	Text      string
}

// render executes t into a buffer first so template errors give a clean 500.
func (s *Server) render(w http.ResponseWriter, status int, t *template.Template, data any) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		s.log.Error("render page", "err", err)
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
		base: s.base(sess, title), Message: msg, Link: link, LinkText: linkText,
	})
}

func (s *Server) base(sess *session, title string) base {
	b := base{Title: title, Site: s.cfg.Site}
	if sess != nil {
		b.User = sess.Username
	}
	return b
}

func (s *Server) runList(w http.ResponseWriter, r *http.Request, sess *session) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	runs, err := s.o.Runs.ListRuns(r.Context(), runsPerPage+1, (page-1)*runsPerPage)
	if err != nil {
		s.log.Error("list runs", "err", err)
		s.message(w, r, sess, http.StatusInternalServerError, "Error", "Could not load runs.", "", "")
		return
	}
	p := runsPage{base: s.base(sess, "Runs"), Page: page}
	if len(runs) > runsPerPage {
		runs = runs[:runsPerPage]
		p.NextPage = page + 1
	}
	if page > 1 {
		p.PrevPage = page - 1
	}
	for _, run := range runs {
		p.Runs = append(p.Runs, s.row(run))
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

func (s *Server) runPage(w http.ResponseWriter, r *http.Request, sess *session) {
	run := s.lookupRun(w, r, sess)
	if run == nil {
		return
	}
	p := runPage{
		base:    s.base(sess, fmt.Sprintf("Run %d", run.ID)),
		Run:     s.row(run),
		Summary: run.Summary,
	}
	p.Refresh = !run.State.Finished()
	if run.LogsExpired {
		p.Log.Expired = true
	} else {
		lv, err := readLogTail(s.o.LogPath(run.ID), logInlineLimit)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Error("read log", "id", run.ID, "err", err)
		}
		p.Log = lv
	}
	s.render(w, http.StatusOK, s.pages.run, p)
}

// readLogTail reads the file, or only its last limit bytes (starting at a line
// boundary) if it is larger.
func readLogTail(path string, limit int64) (logView, error) {
	f, err := os.Open(path)
	if err != nil {
		return logView{Missing: true}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return logView{Missing: true}, err
	}
	v := logView{Size: byteSize(st.Size())}
	var r io.Reader = f
	if st.Size() > limit {
		if _, err := f.Seek(st.Size()-limit, io.SeekStart); err != nil {
			return logView{Missing: true}, err
		}
		r = io.LimitReader(f, limit)
		v.Truncated = true
		v.Shown = byteSize(limit)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return logView{Missing: true}, err
	}
	if v.Truncated {
		if i := bytes.IndexByte(b, '\n'); i >= 0 && i < len(b)-1 {
			b = b[i+1:]
		}
	}
	if !utf8.Valid(b) {
		b = bytes.ToValidUTF8(b, []byte("�"))
	}
	v.Text = string(b)
	return v, nil
}

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
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// row formats a run for the templates.
func (s *Server) row(r *model.Run) runRow {
	row := runRow{
		ID: r.ID, Kind: r.Kind, Ref: r.Ref, SHA: r.ShortSHA(), FullSHA: r.SHA,
		Trigger: r.Trigger, By: r.TriggeredBy, State: r.State,
		Description: r.Description,
		Queued:      s.fmtTime(r.QueuedAt),
		Started:     s.fmtTime(r.StartedAt),
		Finished:    s.fmtTime(r.FinishedAt),
	}
	if row.Description == "" {
		row.Description = report.Describe(r)
	}
	if r.MRIID > 0 {
		row.MR = "!" + strconv.Itoa(r.MRIID)
	}
	switch {
	case r.Duration() > 0:
		row.Duration = report.FormatDuration(r.Duration())
	case r.State == model.StateRunning && !r.StartedAt.IsZero():
		row.Duration = report.FormatDuration(s.now().Sub(r.StartedAt)) + " so far"
	}
	return row
}

func (s *Server) fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(s.cfg.Location()).Format("2006-01-02 15:04:05 MST")
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.ToValidUTF8(string([]rune(s)[:n-1]), "") + "…"
}

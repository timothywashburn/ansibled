package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/daemon"
	"github.com/timothywashburn/ansibled/internal/model"
)

func writeLog(t *testing.T, e *env, id int64, text string) string {
	t.Helper()
	p := filepath.Join(e.logDir, strconv.FormatInt(id, 10), "log.txt")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestChangesTab(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Details = func(id int64, n int) ([]model.TaskDetail, error) {
			return []model.TaskDetail{
				{Host: "web1", Play: "base", Task: "Write motd", Action: "ansible.builtin.copy", Status: "changed",
					Diff: "--- before: /etc/motd\n+++ after: /etc/motd\n@@ -1 +1 @@\n-old\n+<new>\n"},
				{Host: "db1", Task: "Start postgres", Action: "ansible.builtin.service", Status: "failed", Msg: "Unit <postgres> not found"},
			}, nil
		}
	})
	r := mkRun(5, model.StateFailed)
	r.Summary = &model.Summary{Playbooks: []model.PlaybookResult{{Playbook: "site.yml", ExitCode: 2, Hosts: []model.HostResult{
		{Host: "db1", Failed: 1, FailedTasks: []string{"Start postgres"}},
		{Host: "web1", OK: 3, Changed: 1, ChangedTasks: []string{"Write motd"}},
	}}}}
	e.runs.runs = []*model.Run{r}
	sc := e.login(t)
	body := readBody(t, e.do(t, "GET", "/runs/5", nil, nil, sc))
	for _, s := range []string{
		`<span class="dl del">-old</span>`,
		`<span class="dl add">&#43;&lt;new&gt;</span>`,
		`<span class="dl hunk">@@ -1 &#43;1 @@</span>`,
		`<pre class="msg">Unit &lt;postgres&gt; not found</pre>`,
		`<span class="count warn">1</span>`, `<span class="count bad">1</span>`,
		"ansible.builtin.copy", `id="h-db1"`, `id="h-web1"`,
	} {
		if !strings.Contains(body, s) {
			t.Errorf("missing %q", s)
		}
	}
}

func TestRawLogColours(t *testing.T) {
	e := newEnv(t, nil)
	e.runs.runs = []*model.Run{mkRun(5, model.StateSuccess)}
	writeLog(t, e, 5, "\x1b[0;32mok: [web1]\x1b[0m\n")
	sc := e.login(t)
	if body := readBody(t, e.do(t, "GET", "/runs/5/log.txt", nil, nil, sc)); body != "ok: [web1]\n" {
		t.Errorf("plain log = %q", body)
	}
	if body := readBody(t, e.do(t, "GET", "/runs/5/log.txt?ansi=1", nil, nil, sc)); body != "\x1b[0;32mok: [web1]\x1b[0m\n" {
		t.Errorf("ansi log = %q", body)
	}
	res := e.do(t, "GET", "/runs/5/log.txt?download=1", nil, nil, sc)
	if cd := res.Header.Get("Content-Disposition"); cd != `attachment; filename="ansibled-lab-run-5.log"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

// sse is one Server-Sent Event.
type sse struct{ event, id, data string }

// readEvents reads events from a stream until done or n events.
func readEvents(t *testing.T, sc *bufio.Scanner, ch chan<- sse) {
	var cur sse
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.data != "" || cur.event != "" {
				ch <- cur
			}
			cur = sse{}
		case strings.HasPrefix(line, "event: "):
			cur.event = line[7:]
		case strings.HasPrefix(line, "id: "):
			cur.id = line[4:]
		case strings.HasPrefix(line, "data: "):
			cur.data = line[6:]
		}
	}
	close(ch)
}

func openStream(t *testing.T, e *env, sc *http.Cookie, id string, lastID string) (chan sse, func()) {
	t.Helper()
	srv := httptest.NewServer(e.h)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/runs/"+id+"/stream", nil)
	req.AddCookie(sc)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	ch := make(chan sse, 100)
	scn := bufio.NewScanner(res.Body)
	scn.Buffer(make([]byte, 64<<10), 4<<20)
	go readEvents(t, scn, ch)
	return ch, func() { cancel(); res.Body.Close(); srv.Close() }
}

func next(t *testing.T, ch chan sse) sse {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return sse{}
}

func logText(t *testing.T, ev sse) string {
	t.Helper()
	if ev.event != "log" {
		t.Fatalf("event %q, want log (%s)", ev.event, ev.data)
	}
	var c logChunk
	if err := json.Unmarshal([]byte(ev.data), &c); err != nil {
		t.Fatal(err)
	}
	return c.Text
}

// lockedRuns lets a test change a run while the stream reads it.
type lockedRuns struct {
	fakeRuns
	mu sync.Mutex
}

func (l *lockedRuns) GetRun(ctx context.Context, id int64) (*model.Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.fakeRuns.GetRun(ctx, id)
	if r != nil {
		c := *r
		return &c, err
	}
	return r, err
}

func TestStreamLive(t *testing.T) {
	lr := &lockedRuns{}
	e := newEnv(t, func(o *Options) { o.Runs = lr })
	e.s.streamPoll = 10 * time.Millisecond
	run := mkRun(5, model.StateRunning)
	lr.runs = []*model.Run{run}
	p := writeLog(t, e, 5, "line one\npart")
	sc := e.login(t)

	ch, stop := openStream(t, e, sc, "5", "")
	defer stop()
	if ev := next(t, ch); ev.event != "run" || !strings.Contains(ev.data, `"state":"running"`) {
		t.Fatalf("first event %+v", ev)
	}
	if got := logText(t, next(t, ch)); got != "line one\n" {
		t.Fatalf("first chunk %q (a partial line must wait)", got)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("ial\n\x1b[0;33mchanged\x1b[0m\nlast")
	f.Close()
	if got := logText(t, next(t, ch)); got != "partial\n\x1b[0;33mchanged\x1b[0m\n" {
		t.Fatalf("second chunk %q", got)
	}
	lr.mu.Lock()
	run.State = model.StateSuccess
	run.FinishedAt = run.StartedAt.Add(time.Minute)
	lr.mu.Unlock()
	ev := next(t, ch)
	if ev.event != "run" || !strings.Contains(ev.data, `"finished":true`) {
		t.Fatalf("state event %+v", ev)
	}
	if got := logText(t, next(t, ch)); got != "last" {
		t.Fatalf("final chunk %q", got)
	}
	if ev := next(t, ch); ev.event != "done" {
		t.Fatalf("want done, got %+v", ev)
	}
}

func TestStreamResume(t *testing.T) {
	e := newEnv(t, nil)
	e.runs.runs = []*model.Run{mkRun(5, model.StateFailed)}
	writeLog(t, e, 5, "aaaa\nbbbb\n")
	sc := e.login(t)
	ch, stop := openStream(t, e, sc, "5", "5")
	defer stop()
	next(t, ch) // run
	ev := next(t, ch)
	if got := logText(t, ev); got != "bbbb\n" || ev.id != "10" {
		t.Fatalf("resumed chunk %q id %q", got, ev.id)
	}
	if ev := next(t, ch); ev.event != "done" {
		t.Fatalf("want done, got %+v", ev)
	}
}

func TestStreamStartsNearEndOfLargeLog(t *testing.T) {
	e := newEnv(t, nil)
	e.runs.runs = []*model.Run{mkRun(5, model.StateSuccess)}
	big := strings.Repeat("x", 99) + "\n"
	writeLog(t, e, 5, strings.Repeat(big, logInlineLimit/100+500)+"END\n")
	sc := e.login(t)
	ch, stop := openStream(t, e, sc, "5", "")
	defer stop()
	next(t, ch)
	ev := next(t, ch)
	var c logChunk
	_ = json.Unmarshal([]byte(ev.data), &c)
	if c.Skipped == 0 || !strings.HasPrefix(c.Text, "xxx") {
		t.Fatalf("first chunk skipped=%d starts %q", c.Skipped, c.Text[:10])
	}
	var all strings.Builder
	all.WriteString(c.Text)
	for {
		ev := next(t, ch)
		if ev.event == "done" {
			break
		}
		all.WriteString(logText(t, ev))
	}
	if n := int64(all.Len()); n > logInlineLimit || !strings.HasSuffix(all.String(), "END\n") {
		t.Fatalf("streamed %d bytes, suffix %q", n, all.String()[all.Len()-4:])
	}
}

func TestReadFrom(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	_ = os.WriteFile(p, []byte("ab\ncd"), 0o644)
	buf := make([]byte, 16)
	if n, _ := readFrom(p, 0, buf, false); string(buf[:n]) != "ab\n" {
		t.Errorf("complete lines: %q", buf[:n])
	}
	if n, _ := readFrom(p, 3, buf, false); n != 0 {
		t.Errorf("partial line sent: %q", buf[:n])
	}
	if n, _ := readFrom(p, 3, buf, true); string(buf[:n]) != "cd" {
		t.Errorf("finished: %q", buf[:n])
	}
	_ = os.WriteFile(p, []byte("abcdé"), 0o644) // é is 2 bytes; cut inside it
	small := make([]byte, 5)
	if n, _ := readFrom(p, 0, small, false); string(small[:n]) != "abcd" {
		t.Errorf("long line cut: %q", small[:n])
	}
}

// fakeActions records the buttons pressed.
type fakeActions struct {
	mu       sync.Mutex
	reruns   []int64
	cancels  []int64
	user     string
	rerunErr error
}

func (f *fakeActions) Rerun(_ context.Context, id int64, user string) (*model.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reruns, f.user = append(f.reruns, id), user
	if f.rerunErr != nil {
		return nil, f.rerunErr
	}
	return &model.Run{ID: 42}, nil
}

func (f *fakeActions) Cancel(_ context.Context, id int64, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels, f.user = append(f.cancels, id), user
	return nil
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func TestActions(t *testing.T) {
	fa := &fakeActions{}
	e := newEnv(t, func(o *Options) { o.Actions = fa })
	e.runs.runs = []*model.Run{mkRun(6, model.StateRunning), mkRun(5, model.StateSuccess)}
	sc := e.login(t)

	body := readBody(t, e.do(t, "GET", "/runs/6", nil, nil, sc))
	m := csrfRe.FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, `action="/runs/6/cancel"`) || !strings.Contains(body, `action="/runs/6/rerun"`) {
		t.Fatal("buttons missing on an active run")
	}
	if body := readBody(t, e.do(t, "GET", "/runs/5", nil, nil, sc)); strings.Contains(body, "/cancel") {
		t.Error("cancel shown on a finished run")
	}
	token := m[1]
	form := func(tok string) *strings.Reader { return strings.NewReader(url.Values{"csrf": {tok}}.Encode()) }
	hdr := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}

	res := e.do(t, "POST", "/runs/5/rerun", form(token), hdr, sc)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/runs/42#log" {
		t.Errorf("rerun: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	res = e.do(t, "POST", "/runs/6/cancel", form(token), hdr, sc)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/runs/6" {
		t.Errorf("cancel: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if len(fa.reruns) != 1 || fa.reruns[0] != 5 || len(fa.cancels) != 1 || fa.cancels[0] != 6 || fa.user != "alice" {
		t.Errorf("actions = %+v", fa)
	}

	// Rejected: no token, wrong token, cross-site, no session.
	for name, tc := range map[string]struct {
		tok  string
		site string
		sess *http.Cookie
		want int
	}{
		"no token":   {"", "", sc, http.StatusForbidden},
		"bad token":  {"AAAA", "", sc, http.StatusForbidden},
		"cross-site": {token, "cross-site", sc, http.StatusForbidden},
		"no session": {token, "", nil, http.StatusFound},
	} {
		h := hdr.Clone()
		if tc.site != "" {
			h.Set("Sec-Fetch-Site", tc.site)
		}
		if res := e.do(t, "POST", "/runs/5/rerun", form(tc.tok), h, tc.sess); res.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", name, res.StatusCode, tc.want)
		}
	}
	if len(fa.reruns) != 1 {
		t.Errorf("a rejected request ran: %v", fa.reruns)
	}

	fa.rerunErr = daemon.ActionError("merge request !3 is merged")
	res = e.do(t, "POST", "/runs/5/rerun", form(token), hdr, sc)
	if b := readBody(t, res); res.StatusCode != http.StatusConflict || !strings.Contains(b, "merge request !3 is merged") {
		t.Errorf("action error: %d", res.StatusCode)
	}
}

func TestActionsDisabled(t *testing.T) {
	e := newEnv(t, nil)
	e.runs.runs = []*model.Run{mkRun(5, model.StateSuccess)}
	sc := e.login(t)
	if res := e.do(t, "POST", "/runs/5/rerun", strings.NewReader("csrf=x"), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, sc); res.StatusCode != 404 {
		t.Errorf("status %d", res.StatusCode)
	}
}

func TestRunFilters(t *testing.T) {
	e := newEnv(t, nil)
	a := mkRun(3, model.StateFailed)
	b := mkRun(2, model.StateSuccess)
	b.Kind, b.MRIID, b.Key = model.KindMain, 0, "main"
	c := mkRun(1, model.StateSuccess)
	c.MRIID = 9
	e.runs.runs = []*model.Run{a, b, c}
	sc := e.login(t)
	check := func(q string, want ...string) {
		t.Helper()
		body := readBody(t, e.do(t, "GET", "/runs"+q, nil, nil, sc))
		for _, id := range []string{"1", "2", "3"} {
			has := strings.Contains(body, `href="/runs/`+id+`"`)
			if has != slices.Contains(want, id) {
				t.Errorf("%s: run %s shown=%v", q, id, has)
			}
		}
	}
	check("", "1", "2", "3")
	check("?kind=main", "2")
	check("?state=success", "1", "2")
	check("?mr=!9", "1")
	check("?kind=bogus", "1", "2", "3")

	body := readBody(t, e.do(t, "GET", "/runs?partial=1&kind=main", nil, nil, sc))
	if strings.Contains(body, "<html") || !strings.Contains(body, `href="/runs/2"`) {
		t.Errorf("partial: %s", body)
	}
}

func TestOverview(t *testing.T) {
	e := newEnv(t, nil)
	nightly := mkRun(4, model.StateFailed)
	nightly.Kind, nightly.MRIID = model.KindNightly, 0
	nightly.Summary = &model.Summary{Playbooks: []model.PlaybookResult{{Playbook: "site.yml", Hosts: []model.HostResult{
		{Host: "alpha", OK: 5},
		{Host: "beta", Changed: 2},
		{Host: "gamma", Unreachable: true},
	}}}}
	mainRun := mkRun(3, model.StateSuccess)
	mainRun.Kind, mainRun.MRIID = model.KindMain, 0
	e.runs.runs = []*model.Run{mkRun(6, model.StateRunning), mkRun(5, model.StateQueued), nightly, mainRun}
	sc := e.login(t)
	body := readBody(t, e.do(t, "GET", "/", nil, nil, sc))
	for _, s := range []string{`/runs/4#h-beta`, `/runs/4#h-gamma`, `>1</span><span class="muted"> / 3 hosts`, "1 unreachable",
		`href="/runs/3"`, `href="/runs/6"`, `>2</span><span class="muted"> runs`} {
		if !strings.Contains(body, s) {
			t.Errorf("overview missing %q", s)
		}
	}
	// gamma (unreachable) sorts before beta (would change) before alpha.
	if g, b, a := strings.Index(body, "#h-gamma"), strings.Index(body, "#h-beta"), strings.Index(body, "#h-alpha"); !(g < b && b < a) {
		t.Errorf("drift order gamma=%d beta=%d alpha=%d", g, b, a)
	}
	if p := readBody(t, e.do(t, "GET", "/?partial=1", nil, nil, sc)); strings.Contains(p, "<html") || !strings.Contains(p, "#h-beta") {
		t.Error("overview partial wrong")
	}
}

func TestStaticAssets(t *testing.T) {
	e := newEnv(t, nil)
	res := e.do(t, "GET", e.s.assets.url("app.js"), nil, nil)
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "javascript") ||
		!strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("app.js: %d %v", res.StatusCode, res.Header)
	}
	if res := e.do(t, "GET", "/static/app.css", nil, nil); res.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("unversioned asset cached: %q", res.Header.Get("Cache-Control"))
	}
	if res := e.do(t, "GET", "/static/nope.js", nil, nil); res.StatusCode != 404 {
		t.Errorf("missing asset: %d", res.StatusCode)
	}
}

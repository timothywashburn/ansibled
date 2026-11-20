package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/config"
	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/metrics"
	"github.com/timothywashburn/ansibled/internal/model"
)

// fakeForge implements forge.Forge with just enough behaviour for the web tests.
type fakeForge struct {
	mu          sync.Mutex
	webhookErr  error
	events      []forge.Event
	user        forge.User
	exchangeErr error
	trusted     bool
	trustErr    error
	// recorded Exchange arguments
	code, verifier, redirect string
}

func (f *fakeForge) Name() string { return "fake" }
func (f *fakeForge) HandleWebhook(r *http.Request) ([]forge.Event, error) {
	_, _ = io.ReadAll(r.Body)
	return f.events, f.webhookErr
}
func (f *fakeForge) DefaultBranch(context.Context) (string, error)            { return "main", nil }
func (f *fakeForge) BranchHead(context.Context, string) (string, error)       { return "", nil }
func (f *fakeForge) OpenMRs(context.Context) ([]forge.MR, error)              { return nil, nil }
func (f *fakeForge) MRsForBranch(context.Context, string) ([]forge.MR, error) { return nil, nil }
func (f *fakeForge) GetMR(context.Context, int) (forge.MR, error)             { return forge.MR{}, nil }
func (f *fakeForge) PusherOf(context.Context, string, string) (*forge.User, error) {
	return nil, nil
}
func (f *fakeForge) IsTrusted(_ context.Context, u forge.User) (bool, error) {
	return f.trusted && u.ID == f.user.ID, f.trustErr
}
func (f *fakeForge) BotUser(context.Context) (forge.User, error)                { return forge.User{}, nil }
func (f *fakeForge) SetStatus(context.Context, string, forge.Status) error      { return nil }
func (f *fakeForge) UpsertMRComment(context.Context, int, string, string) error { return nil }
func (f *fakeForge) GitAuth() (string, string)                                  { return "", "" }
func (f *fakeForge) MRRef(iid int) string                                       { return fmt.Sprintf("refs/merge-requests/%d/head", iid) }
func (f *fakeForge) OAuth() forge.OAuthProvider                                 { return (*fakeOAuth)(f) }

type fakeOAuth fakeForge

func (o *fakeOAuth) AuthCodeURL(state, verifier, redirectURL string) string {
	v := url.Values{"state": {state}, "code_challenge_src": {verifier}, "redirect_uri": {redirectURL}}
	return "https://forge.example/oauth/authorize?" + v.Encode()
}

func (o *fakeOAuth) Exchange(_ context.Context, code, verifier, redirectURL string) (forge.User, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.code, o.verifier, o.redirect = code, verifier, redirectURL
	return o.user, o.exchangeErr
}

// fakeRuns is a RunSource over a slice, newest first.
type fakeRuns struct {
	runs []*model.Run
	err  error
}

func (f *fakeRuns) GetRun(_ context.Context, id int64) (*model.Run, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return r, f.err
		}
	}
	return nil, f.err
}

func (f *fakeRuns) ListRuns(_ context.Context, limit, offset int) ([]*model.Run, error) {
	if offset >= len(f.runs) {
		return nil, f.err
	}
	return f.runs[offset:min(len(f.runs), offset+limit)], f.err
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	s      *Server
	h      http.Handler
	forge  *fakeForge
	runs   *fakeRuns
	logDir string
	logs   *syncBuf
	events chan []forge.Event
}

const metricsToken = "test-metrics-token"

func newEnv(t *testing.T, mod func(*Options)) *env {
	t.Helper()
	e := &env{
		forge:  &fakeForge{user: forge.User{ID: 7, Username: "alice"}, trusted: true},
		runs:   &fakeRuns{},
		logDir: t.TempDir(),
		logs:   &syncBuf{},
		events: make(chan []forge.Event, 1),
	}
	o := Options{
		Config: &config.Config{
			Site: "lab", PublicURL: "http://ansibled.example:8471",
			Listen:   config.Listen{Webhook: "127.0.0.1:0", Web: "127.0.0.1:0"},
			Forge:    config.Forge{TrustedGroup: "example/admins"},
			Schedule: config.Schedule{Timezone: "UTC"},
		},
		Forge:        e.forge,
		Runs:         e.runs,
		LogPath:      func(id int64) string { return filepath.Join(e.logDir, fmt.Sprint(id), "log.txt") },
		Metrics:      metrics.New(),
		MetricsToken: metricsToken,
		SessionKey:   []byte("0123456789abcdef0123456789abcdef"),
		OnEvents:     func(_ context.Context, ev []forge.Event) { e.events <- ev },
		Logger:       slog.New(slog.NewTextHandler(e.logs, nil)),
	}
	if mod != nil {
		mod(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	e.s, e.h = s, s.Handler()
	return e
}

func (e *env) do(t *testing.T, method, target string, body io.Reader, hdr http.Header, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		req.Header[k] = v
	}
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Result()
}

func readBody(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func cookie(r *http.Response, name string) *http.Cookie {
	for _, c := range r.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// startLogin runs /login and returns the oauth cookie and the state sent to the forge.
func (e *env) startLogin(t *testing.T, next string) (*http.Cookie, url.Values) {
	t.Helper()
	res := e.do(t, "GET", "/login?next="+url.QueryEscape(next), nil, nil)
	if res.StatusCode != http.StatusFound {
		t.Fatalf("/login status %d", res.StatusCode)
	}
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil || loc.Host != "forge.example" {
		t.Fatalf("/login Location = %q", res.Header.Get("Location"))
	}
	c := cookie(res, "ansibled_oauth_lab")
	if c == nil || !c.HttpOnly || c.MaxAge != 600 {
		t.Fatalf("oauth cookie = %+v", c)
	}
	return c, loc.Query()
}

// login signs in and returns the session cookie.
func (e *env) login(t *testing.T) *http.Cookie {
	t.Helper()
	oc, q := e.startLogin(t, "/runs")
	res := e.do(t, "GET", "/oauth/callback?code=c0de&state="+url.QueryEscape(q.Get("state")), nil, nil, oc)
	if res.StatusCode != http.StatusFound {
		t.Fatalf("callback status %d: %s", res.StatusCode, readBody(t, res))
	}
	c := cookie(res, "ansibled_session_lab")
	if c == nil {
		t.Fatal("no session cookie")
	}
	return c
}

func TestHealthz(t *testing.T) {
	e := newEnv(t, nil)
	res := e.do(t, "GET", "/healthz", nil, nil)
	if res.StatusCode != 200 || strings.TrimSpace(readBody(t, res)) != "ok" {
		t.Fatalf("healthz %d", res.StatusCode)
	}
}

func TestWebhook(t *testing.T) {
	e := newEnv(t, nil)
	webhooks := func() string {
		rec := httptest.NewRecorder()
		e.s.o.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}

	e.forge.webhookErr = fmt.Errorf("wrapped: %w", forge.ErrUnauthorized)
	if res := e.do(t, "POST", "/webhook/fake", strings.NewReader("{}"), nil); res.StatusCode != 401 {
		t.Errorf("unauthorized: status %d", res.StatusCode)
	}
	if !strings.Contains(webhooks(), `ansibled_webhooks_total{accepted="false",event="unauthorized"} 1`) {
		t.Errorf("unauthorized not counted")
	}

	e.forge.webhookErr = errors.New("bad json")
	if res := e.do(t, "POST", "/webhook/fake", strings.NewReader("{"), nil); res.StatusCode != 400 {
		t.Errorf("invalid: status %d", res.StatusCode)
	}
	if !strings.Contains(webhooks(), `ansibled_webhooks_total{accepted="false",event="invalid"} 1`) {
		t.Errorf("invalid not counted")
	}

	e.forge.webhookErr = nil
	e.forge.events = []forge.Event{{Type: forge.EventPush, Branch: "main", SHA: "abc"}}
	if res := e.do(t, "POST", "/webhook/fake", strings.NewReader("{}"), nil); res.StatusCode != 202 {
		t.Errorf("ok: status %d", res.StatusCode)
	}
	select {
	case ev := <-e.events:
		if len(ev) != 1 || ev[0].SHA != "abc" {
			t.Errorf("events = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnEvents not called")
	}
	if strings.Contains(webhooks(), `accepted="true"`) {
		t.Errorf("web counted an accepted webhook; the dispatcher does that")
	}

	if res := e.do(t, "GET", "/webhook/fake", nil, nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET webhook: status %d", res.StatusCode)
	}
}

func TestMetricsAuth(t *testing.T) {
	e := newEnv(t, nil)
	for name, h := range map[string]string{"none": "", "wrong": "Bearer nope", "basic": "Basic " + metricsToken, "prefix": "Bearer " + metricsToken[:5]} {
		hdr := http.Header{}
		if h != "" {
			hdr.Set("Authorization", h)
		}
		res := e.do(t, "GET", "/metrics", nil, hdr)
		if res.StatusCode != 401 || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: status %d, WWW-Authenticate %q", name, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
	}
	res := e.do(t, "GET", "/metrics", nil, http.Header{"Authorization": {"Bearer " + metricsToken}})
	if body := readBody(t, res); res.StatusCode != 200 || !strings.Contains(body, "ansibled_runs_total") {
		t.Errorf("authorized: status %d", res.StatusCode)
	}

	// No token configured: always refuse.
	e2 := newEnv(t, func(o *Options) { o.MetricsToken = "" })
	if res := e2.do(t, "GET", "/metrics", nil, http.Header{"Authorization": {"Bearer "}}); res.StatusCode != 401 {
		t.Errorf("empty token: status %d", res.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, nil)
	res := e.do(t, "GET", "/logout", nil, nil)
	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "same-origin",
	}
	for k, v := range want {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if body := readBody(t, res); !strings.Contains(body, "signed out") && !strings.Contains(body, "Signed out") {
		t.Errorf("logout body: %s", body)
	}
	if c := cookie(res, "ansibled_session_lab"); c == nil || c.MaxAge >= 0 {
		t.Errorf("logout did not clear session: %+v", c)
	}
}

func TestLoginSuccess(t *testing.T) {
	e := newEnv(t, nil)
	oc, q := e.startLogin(t, "/runs/5")
	if len(q.Get("state")) != 43 || len(q.Get("code_challenge_src")) != 43 {
		t.Errorf("state/verifier not 32 random bytes base64url: %v", q)
	}
	if q.Get("redirect_uri") != "http://ansibled.example:8471/oauth/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	res := e.do(t, "GET", "/oauth/callback?code=c0de&state="+url.QueryEscape(q.Get("state")), nil, nil, oc)
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/runs/5" {
		t.Fatalf("callback: %d -> %q", res.StatusCode, res.Header.Get("Location"))
	}
	if e.forge.code != "c0de" || e.forge.verifier != q.Get("code_challenge_src") || e.forge.redirect != q.Get("redirect_uri") {
		t.Errorf("Exchange got code=%q verifier=%q redirect=%q", e.forge.code, e.forge.verifier, e.forge.redirect)
	}
	sc := cookie(res, "ansibled_session_lab")
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || sc.Path != "/" || sc.Secure || sc.MaxAge != 12*3600 {
		t.Errorf("session cookie = %+v", sc)
	}
	if c := cookie(res, "ansibled_oauth_lab"); c == nil || c.MaxAge >= 0 {
		t.Errorf("oauth cookie not cleared: %+v", c)
	}

	// The session works and shows the user.
	res = e.do(t, "GET", "/runs", nil, nil, sc)
	if body := readBody(t, res); res.StatusCode != 200 || !strings.Contains(body, "alice") || !strings.Contains(body, "lab") {
		t.Errorf("/runs with session: %d", res.StatusCode)
	}
}

func TestSecureCookieOnHTTPS(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Config.PublicURL = "https://ansibled.example" })
	oc, q := e.startLogin(t, "")
	if !oc.Secure {
		t.Error("oauth cookie not Secure")
	}
	res := e.do(t, "GET", "/oauth/callback?code=x&state="+url.QueryEscape(q.Get("state")), nil, nil, oc)
	if c := cookie(res, "ansibled_session_lab"); c == nil || !c.Secure {
		t.Errorf("session cookie not Secure: %+v", c)
	}
	if res.Header.Get("Location") != "/runs" {
		t.Errorf("default next = %q", res.Header.Get("Location"))
	}
}

func TestLoginFailures(t *testing.T) {
	t.Run("state mismatch", func(t *testing.T) {
		e := newEnv(t, nil)
		oc, _ := e.startLogin(t, "/runs")
		res := e.do(t, "GET", "/oauth/callback?code=x&state=forged", nil, nil, oc)
		if res.StatusCode != 400 || cookie(res, "ansibled_session_lab") != nil {
			t.Errorf("status %d", res.StatusCode)
		}
		if e.forge.code != "" {
			t.Error("Exchange called despite state mismatch")
		}
	})
	t.Run("no oauth cookie", func(t *testing.T) {
		e := newEnv(t, nil)
		_, q := e.startLogin(t, "/runs")
		res := e.do(t, "GET", "/oauth/callback?code=x&state="+q.Get("state"), nil, nil)
		if res.StatusCode != 400 || cookie(res, "ansibled_session_lab") != nil {
			t.Errorf("status %d", res.StatusCode)
		}
	})
	t.Run("expired oauth cookie", func(t *testing.T) {
		e := newEnv(t, nil)
		oc, q := e.startLogin(t, "/runs")
		e.s.now = func() time.Time { return time.Now().Add(11 * time.Minute) }
		res := e.do(t, "GET", "/oauth/callback?code=x&state="+q.Get("state"), nil, nil, oc)
		if res.StatusCode != 400 {
			t.Errorf("status %d", res.StatusCode)
		}
	})
	t.Run("untrusted", func(t *testing.T) {
		e := newEnv(t, nil)
		e.forge.trusted = false
		oc, q := e.startLogin(t, "/runs")
		res := e.do(t, "GET", "/oauth/callback?code=x&state="+q.Get("state"), nil, nil, oc)
		body := readBody(t, res)
		if res.StatusCode != 403 || cookie(res, "ansibled_session_lab") != nil || !strings.Contains(body, "example/admins") {
			t.Errorf("status %d body %s", res.StatusCode, body)
		}
	})
	t.Run("exchange error", func(t *testing.T) {
		e := newEnv(t, nil)
		e.forge.exchangeErr = errors.New("boom")
		oc, q := e.startLogin(t, "/runs")
		res := e.do(t, "GET", "/oauth/callback?code=x&state="+q.Get("state"), nil, nil, oc)
		if res.StatusCode != 502 || cookie(res, "ansibled_session_lab") != nil {
			t.Errorf("status %d", res.StatusCode)
		}
	})
}

func TestSessionRejected(t *testing.T) {
	e := newEnv(t, nil)
	good := e.login(t)

	redirected := func(name string, c *http.Cookie) {
		t.Helper()
		res := e.do(t, "GET", "/runs/1?x=y", nil, nil, c)
		if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/login?next="+url.QueryEscape("/runs/1?x=y") {
			t.Errorf("%s: %d -> %q", name, res.StatusCode, res.Header.Get("Location"))
		}
	}
	redirected("no cookie", nil)

	// Tampered payload: change the username, keep the MAC.
	p, mac, _ := strings.Cut(good.Value, ".")
	forgedPayload, _ := e.s.sign("x", session{UserID: 1, Username: "mallory", Expires: time.Now().Add(time.Hour).Unix()})
	fp, _, _ := strings.Cut(forgedPayload, ".")
	redirected("tampered payload", &http.Cookie{Name: good.Name, Value: fp + "." + mac})
	redirected("tampered mac", &http.Cookie{Name: good.Name, Value: p + ".AAAA"})
	redirected("garbage", &http.Cookie{Name: good.Name, Value: "garbage"})

	// Signed with another key.
	other := newEnv(t, func(o *Options) { o.SessionKey = []byte("another-key-another-key-another!!") })
	redirected("other key", other.login(t))

	// An oauth cookie cannot be used as a session cookie.
	oc, _ := e.startLogin(t, "/runs")
	redirected("oauth cookie as session", &http.Cookie{Name: good.Name, Value: oc.Value})

	// Expired.
	e.s.now = func() time.Time { return time.Now().Add(12*time.Hour + time.Minute) }
	redirected("expired", good)
	e.s.now = time.Now

	if res := e.do(t, "GET", "/runs", nil, nil, good); res.StatusCode != 200 {
		t.Errorf("good cookie: %d", res.StatusCode)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "/runs",
		"/runs/4":               "/runs/4",
		"/runs?page=2":          "/runs?page=2",
		"runs":                  "/runs",
		"//evil.example/":       "/runs",
		"/\\evil.example":       "/runs",
		"https://evil.example/": "/runs",
		"/runs\r\nSet-Cookie:x": "/runs",
		"/login":                "/runs",
		"/oauth/callback?x":     "/runs",
		"/%2F/evil":             "/%2F/evil",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}

	// next is validated when /login stores it too.
	e := newEnv(t, nil)
	oc, q := e.startLogin(t, "//evil.example/x")
	res := e.do(t, "GET", "/oauth/callback?code=x&state="+q.Get("state"), nil, nil, oc)
	if loc := res.Header.Get("Location"); loc != "/runs" {
		t.Errorf("redirect to %q", loc)
	}
}

func TestRootRedirect(t *testing.T) {
	e := newEnv(t, nil)
	if res := e.do(t, "GET", "/", nil, nil); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/runs" {
		t.Errorf("/ -> %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func mkRun(id int64, state model.State) *model.Run {
	q := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	r := &model.Run{
		ID: id, Kind: model.KindMR, Key: "mr:3", Mode: model.ModeCheck, SHA: fmt.Sprintf("%040x", id),
		Ref: "feature", MRIID: 3, Trigger: model.TriggerPush, TriggeredBy: "bob", State: state, QueuedAt: q,
	}
	if state != model.StateQueued {
		r.StartedAt = q.Add(time.Second)
	}
	if state.Finished() {
		r.FinishedAt = q.Add(192 * time.Second)
	}
	return r
}

func TestRunList(t *testing.T) {
	e := newEnv(t, nil)
	for i := int64(60); i >= 1; i-- {
		e.runs.runs = append(e.runs.runs, mkRun(i, model.StateSuccess))
	}
	sc := e.login(t)
	body := readBody(t, e.do(t, "GET", "/runs", nil, nil, sc))
	for _, s := range []string{`href="/runs/60"`, `href="/runs/11"`, "!3", "feature", "bob", "3m11s", `href="/runs?page=2"`, "no changes"} {
		if !strings.Contains(body, s) {
			t.Errorf("page 1 missing %q", s)
		}
	}
	if strings.Contains(body, `href="/runs/10"`) {
		t.Error("page 1 has more than 50 runs")
	}
	body = readBody(t, e.do(t, "GET", "/runs?page=2", nil, nil, sc))
	if !strings.Contains(body, `href="/runs/10"`) || !strings.Contains(body, `href="/runs?page=1"`) || strings.Contains(body, "page=3") {
		t.Errorf("page 2 wrong")
	}
}

func TestRunPage(t *testing.T) {
	e := newEnv(t, nil)
	evil := "<script>alert(1)</script>"
	r := mkRun(5, model.StateFailed)
	r.Summary = &model.Summary{Playbooks: []model.PlaybookResult{{Playbook: "site.yml", ExitCode: 2, Hosts: []model.HostResult{
		{Host: "fiona", OK: 4, Changed: 1, ChangedTasks: []string{evil}},
		{Host: "khai", Failed: 1, FailedTasks: []string{"broken task"}},
	}}}}
	q := mkRun(6, model.StateQueued)
	x := mkRun(7, model.StateSuccess)
	x.LogsExpired = true
	e.runs.runs = []*model.Run{x, q, r}
	if err := os.MkdirAll(filepath.Join(e.logDir, "5"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.logDir, "5", "log.txt"), []byte("TASK [x] <b>bold</b>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := e.login(t)

	res := e.do(t, "GET", "/runs/5", nil, nil, sc)
	body := readBody(t, res)
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if strings.Contains(body, evil) || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("task name not escaped:\n%s", body)
	}
	if strings.Contains(body, "<b>bold</b>") || !strings.Contains(body, "&lt;b&gt;bold&lt;/b&gt;") {
		t.Errorf("log not escaped")
	}
	for _, s := range []string{"site.yml", "fiona", "khai", "broken task", `/runs/5/log.txt`} {
		if !strings.Contains(body, s) {
			t.Errorf("missing %q", s)
		}
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("finished run refreshes")
	}

	body = readBody(t, e.do(t, "GET", "/runs/6", nil, nil, sc))
	if !strings.Contains(body, `<meta http-equiv="refresh" content="5">`) {
		t.Error("queued run does not refresh")
	}

	body = readBody(t, e.do(t, "GET", "/runs/7", nil, nil, sc))
	if !strings.Contains(body, "log expired") {
		t.Error("expired log not reported")
	}

	for _, p := range []string{"/runs/999", "/runs/abc", "/runs/-1"} {
		if res := e.do(t, "GET", p, nil, nil, sc); res.StatusCode != 404 {
			t.Errorf("%s: status %d", p, res.StatusCode)
		}
	}
}

func TestRawLog(t *testing.T) {
	e := newEnv(t, nil)
	x := mkRun(7, model.StateSuccess)
	x.LogsExpired = true
	e.runs.runs = []*model.Run{x, mkRun(5, model.StateSuccess), mkRun(4, model.StateSuccess)}
	_ = os.MkdirAll(filepath.Join(e.logDir, "5"), 0o755)
	_ = os.WriteFile(filepath.Join(e.logDir, "5", "log.txt"), []byte("<html>hello</html>\n"), 0o644)
	sc := e.login(t)

	res := e.do(t, "GET", "/runs/5/log.txt", nil, nil, sc)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/plain; charset=utf-8" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("raw log: %d %v", res.StatusCode, res.Header)
	}
	if body := readBody(t, res); body != "<html>hello</html>\n" {
		t.Errorf("body %q", body)
	}
	if res := e.do(t, "GET", "/runs/7/log.txt", nil, nil, sc); res.StatusCode != http.StatusGone {
		t.Errorf("expired: %d", res.StatusCode)
	}
	if res := e.do(t, "GET", "/runs/4/log.txt", nil, nil, sc); res.StatusCode != 404 {
		t.Errorf("missing: %d", res.StatusCode)
	}
	if res := e.do(t, "GET", "/runs/5/log.txt", nil, nil); res.StatusCode != http.StatusFound {
		t.Errorf("unauthenticated raw log: %d", res.StatusCode)
	}
}

func TestLogTailTruncation(t *testing.T) {
	e := newEnv(t, nil)
	e.runs.runs = []*model.Run{mkRun(5, model.StateSuccess)}
	_ = os.MkdirAll(filepath.Join(e.logDir, "5"), 0o755)
	var b strings.Builder
	b.WriteString("FIRST-LINE-MARKER\n")
	line := strings.Repeat("x", 99) + "\n"
	for b.Len() < 3<<20 {
		b.WriteString(line)
	}
	b.WriteString("LAST-LINE-MARKER\n")
	_ = os.WriteFile(filepath.Join(e.logDir, "5", "log.txt"), []byte(b.String()), 0o644)
	sc := e.login(t)

	body := readBody(t, e.do(t, "GET", "/runs/5", nil, nil, sc))
	if strings.Contains(body, "FIRST-LINE-MARKER") || !strings.Contains(body, "LAST-LINE-MARKER") {
		t.Error("log not tail-truncated")
	}
	if !strings.Contains(body, "larger than 2.0 MiB") {
		t.Error("no truncation note")
	}
	if len(body) > 2<<20+64<<10 {
		t.Errorf("page is %d bytes", len(body))
	}

	// Unit: the tail starts at a line boundary.
	p := filepath.Join(t.TempDir(), "log")
	_ = os.WriteFile(p, []byte("aaaa\nbbbb\ncccc\n"), 0o644)
	v, err := readLogTail(p, 8)
	if err != nil || !v.Truncated || v.Text != "cccc\n" {
		t.Errorf("readLogTail = %+v, %v", v, err)
	}
	v, err = readLogTail(p, 100)
	if err != nil || v.Truncated || v.Text != "aaaa\nbbbb\ncccc\n" {
		t.Errorf("readLogTail small = %+v, %v", v, err)
	}
}

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never became healthy", addr)
}

func startRun(t *testing.T, s *Server) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}
}

func TestBindRetry(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := busy.Addr().String()
	e := newEnv(t, func(o *Options) { o.Config.Listen = config.Listen{Webhook: addr, Web: addr} })
	e.s.retry = 10 * time.Millisecond
	stop := startRun(t, e.s)
	defer stop()

	// Wait for a few failed attempts, then free the address.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(e.logs.String(), "cannot bind") {
		if time.Now().After(deadline) {
			busy.Close()
			t.Fatal("no bind warning")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	busy.Close()
	waitHealthy(t, addr)
	if n := strings.Count(e.logs.String(), "cannot bind"); n != 1 {
		t.Errorf("%d bind warnings, want 1 (rate limited)", n)
	}
}

func TestRunCancelWhileBinding(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	addr := busy.Addr().String()
	e := newEnv(t, func(o *Options) { o.Config.Listen = config.Listen{Webhook: addr, Web: addr} })
	e.s.retry = 10 * time.Millisecond
	stop := startRun(t, e.s)
	time.Sleep(50 * time.Millisecond)
	stop()
}

func TestSingleListener(t *testing.T) {
	addr := freeAddr(t)
	e := newEnv(t, func(o *Options) { o.Config.Listen = config.Listen{Webhook: addr, Web: addr} })
	stop := startRun(t, e.s)
	defer stop()
	waitHealthy(t, addr)

	res, err := http.Post("http://"+addr+"/webhook/fake", "application/json", strings.NewReader("{}"))
	if err != nil || res.StatusCode != 202 {
		t.Fatalf("webhook: %v %v", res, err)
	}
	res.Body.Close()
	req, _ := http.NewRequest("GET", "http://"+addr+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+metricsToken)
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("metrics: %v %v", res, err)
	}
	res.Body.Close()
	if n := strings.Count(e.logs.String(), "msg=listening"); n != 1 {
		t.Errorf("%d listeners, want 1", n)
	}
}

func TestSeparateListeners(t *testing.T) {
	wh, web := freeAddr(t), freeAddr(t)
	e := newEnv(t, func(o *Options) { o.Config.Listen = config.Listen{Webhook: wh, Web: web} })
	stop := startRun(t, e.s)
	defer stop()
	waitHealthy(t, wh)
	waitHealthy(t, web)

	status := func(method, url string) int {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+metricsToken)
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if got := status("POST", "http://"+wh+"/webhook/fake"); got != 202 {
		t.Errorf("webhook on webhook listener: %d", got)
	}
	for _, p := range []string{"/metrics", "/runs", "/login"} {
		if got := status("GET", "http://"+wh+p); got != 404 {
			t.Errorf("%s on webhook listener: %d", p, got)
		}
	}
	if got := status("GET", "http://"+web+"/metrics"); got != 200 {
		t.Errorf("metrics on web listener: %d", got)
	}
	if got := status("GET", "http://"+web+"/runs"); got != http.StatusFound {
		t.Errorf("runs on web listener: %d", got)
	}
	if got := status("POST", "http://"+web+"/webhook/fake"); got == 202 {
		t.Errorf("webhook served on web listener")
	}
}

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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/config"
	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/metrics"
	"github.com/timothywashburn/ansibled/internal/model"
	"github.com/timothywashburn/ansibled/internal/oidc"
	"github.com/timothywashburn/ansibled/internal/oidc/oidctest"
)

// fakeForge implements forge.Forge with just enough behaviour for the web tests.
type fakeForge struct {
	webhookErr error
	events     []forge.Event
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
func (f *fakeForge) IsTrusted(context.Context, forge.User) (bool, error) {
	panic("the web UI must not use the forge's trusted group")
}
func (f *fakeForge) BotUser(context.Context) (forge.User, error)                { return forge.User{}, nil }
func (f *fakeForge) SetStatus(context.Context, string, forge.Status) error      { return nil }
func (f *fakeForge) UpsertMRComment(context.Context, int, string, string) error { return nil }
func (f *fakeForge) GitAuth() (string, string)                                  { return "", "" }
func (f *fakeForge) MRRef(iid int) string                                       { return fmt.Sprintf("refs/merge-requests/%d/head", iid) }

// fakeSignIn implements SignIn.
type fakeSignIn struct {
	mu       sync.Mutex
	id       oidc.Identity
	urlErr   error
	loginErr error
	// recorded Login arguments
	code, verifier, redirect string
}

func (f *fakeSignIn) AuthCodeURL(state, verifier, redirectURL string) (string, error) {
	v := url.Values{"state": {state}, "code_challenge_src": {verifier}, "redirect_uri": {redirectURL}}
	return "https://idp.example/authorize?" + v.Encode(), f.urlErr
}

func (f *fakeSignIn) Login(_ context.Context, code, verifier, redirectURL string) (oidc.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code, f.verifier, f.redirect = code, verifier, redirectURL
	return f.id, f.loginErr
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

func (f *fakeRuns) FilterRuns(_ context.Context, flt model.RunFilter, limit, offset int) ([]*model.Run, error) {
	var match []*model.Run
	for _, r := range f.runs {
		if (len(flt.Kinds) == 0 || slices.Contains(flt.Kinds, r.Kind)) &&
			(len(flt.States) == 0 || slices.Contains(flt.States, r.State)) &&
			(flt.MRIID == 0 || flt.MRIID == r.MRIID) {
			match = append(match, r)
		}
	}
	if offset >= len(match) {
		return nil, f.err
	}
	return match[offset:min(len(match), offset+limit)], f.err
}

func (f *fakeRuns) ListByState(_ context.Context, states ...model.State) ([]*model.Run, error) {
	var out []*model.Run
	for i := len(f.runs) - 1; i >= 0; i-- {
		if slices.Contains(states, f.runs[i].State) {
			out = append(out, f.runs[i])
		}
	}
	return out, f.err
}

func (f *fakeRuns) LatestFinished(_ context.Context, kinds ...model.Kind) (*model.Run, error) {
	for _, r := range f.runs {
		if (r.State == model.StateSuccess || r.State == model.StateFailed || r.State == model.StateError) &&
			(len(kinds) == 0 || slices.Contains(kinds, r.Kind)) {
			return r, f.err
		}
	}
	return nil, f.err
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
	signIn *fakeSignIn
	runs   *fakeRuns
	logDir string
	logs   *syncBuf
	events chan []forge.Event
}

const metricsToken = "test-metrics-token"

func newEnv(t *testing.T, mod func(*Options)) *env {
	t.Helper()
	e := &env{
		forge:  &fakeForge{},
		signIn: &fakeSignIn{id: oidc.Identity{Subject: "sub-7", Username: "alice", Groups: []string{"sysadmins"}, Allowed: true}},
		runs:   &fakeRuns{},
		logDir: t.TempDir(),
		logs:   &syncBuf{},
		events: make(chan []forge.Event, 1),
	}
	o := Options{
		Config: &config.Config{
			Site: "lab", PublicURL: "http://ansibled.example:9251",
			Listen:   config.Listen{Webhook: "127.0.0.1:0", Web: "127.0.0.1:0"},
			Git:      config.Git{TrustedGroup: "example/admins"},
			Schedule: config.Schedule{Timezone: "UTC"},
			Web:      config.Web{OIDC: config.OIDC{AllowedGroups: []string{"sysadmins", "ops"}}},
		},
		Forge:        e.forge,
		SignIn:       e.signIn,
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

// startLogin runs /login and returns the oauth cookie and the state sent to the provider.
func (e *env) startLogin(t *testing.T, next string) (*http.Cookie, url.Values) {
	t.Helper()
	res := e.do(t, "GET", "/login?next="+url.QueryEscape(next), nil, nil)
	if res.StatusCode != http.StatusFound {
		t.Fatalf("/login status %d", res.StatusCode)
	}
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil || loc.Host != "idp.example" {
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
		"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
			"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
		"X-Frame-Options": "DENY",
		"Referrer-Policy": "same-origin",
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
	if q.Get("redirect_uri") != "http://ansibled.example:9251/oauth/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	res := e.do(t, "GET", "/oauth/callback?code=c0de&state="+url.QueryEscape(q.Get("state")), nil, nil, oc)
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/runs/5" {
		t.Fatalf("callback: %d -> %q", res.StatusCode, res.Header.Get("Location"))
	}
	if e.signIn.code != "c0de" || e.signIn.verifier != q.Get("code_challenge_src") || e.signIn.redirect != q.Get("redirect_uri") {
		t.Errorf("Login got code=%q verifier=%q redirect=%q", e.signIn.code, e.signIn.verifier, e.signIn.redirect)
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

func TestNewRequiresSignIn(t *testing.T) {
	_, err := New(Options{Config: &config.Config{}, Forge: &fakeForge{}, Runs: &fakeRuns{}, LogPath: func(int64) string { return "" }})
	if err == nil || !strings.Contains(err.Error(), "SignIn") {
		t.Errorf("New without SignIn: %v", err)
	}
}

// TestSignInWithOIDCProvider runs the real *oidc.Provider against oidctest.
func TestSignInWithOIDCProvider(t *testing.T) {
	idp := oidctest.New()
	defer idp.Close()
	idp.SetIssuer(idp.URL() + "/application/o/ansibled/")
	p, err := oidc.New(oidc.Options{
		Issuer: idp.Issuer(), ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret,
		AllowedGroups: []string{"sysadmins"}, HTTPClient: idp.Client(), Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(o *Options) { o.SignIn = p; o.Config.Web.OIDC.AllowedGroups = []string{"sysadmins"} })
	browser := *idp.Client()
	browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	signIn := func(code string, claims map[string]any) *http.Response {
		t.Helper()
		idp.AddUser(code, claims)
		res := e.do(t, "GET", "/login?next=/runs/3", nil, nil)
		if res.StatusCode != http.StatusFound || !strings.HasPrefix(res.Header.Get("Location"), idp.URL()+"/application/o/ansibled/authorize?") {
			t.Fatalf("/login: %d %q", res.StatusCode, res.Header.Get("Location"))
		}
		oc := cookie(res, "ansibled_oauth_lab")
		ar, err := browser.Get(res.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		ar.Body.Close()
		cb, err := url.Parse(ar.Header.Get("Location"))
		if ar.StatusCode != http.StatusFound || err != nil || cb.Path != "/oauth/callback" {
			t.Fatalf("authorize: %d %q", ar.StatusCode, ar.Header.Get("Location"))
		}
		return e.do(t, "GET", cb.RequestURI(), nil, nil, oc)
	}

	res := signIn("code-alice", map[string]any{"sub": "a1", "preferred_username": "alice", "groups": []any{"sysadmins"}})
	sc := cookie(res, "ansibled_session_lab")
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/runs/3" || sc == nil {
		t.Fatalf("alice: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if body := readBody(t, e.do(t, "GET", "/runs", nil, nil, sc)); !strings.Contains(body, "alice") {
		t.Error("/runs does not show alice")
	}

	res = signIn("code-bob", map[string]any{"sub": "b1", "preferred_username": "bob", "groups": "users"})
	if body := readBody(t, res); res.StatusCode != 403 || !strings.Contains(body, "bob is not in an allowed group (sysadmins).") ||
		cookie(res, "ansibled_session_lab") != nil {
		t.Errorf("bob: %d %s", res.StatusCode, body)
	}

	// Discovery failure on a fresh provider -> 502 at /login.
	idp.FailNext(oidctest.Discovery, 1, 500)
	p2, _ := oidc.New(oidc.Options{Issuer: idp.Issuer(), ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret,
		AllowedGroups: []string{"sysadmins"}, HTTPClient: idp.Client(), Logger: slog.New(slog.DiscardHandler)})
	e2 := newEnv(t, func(o *Options) { o.SignIn = p2 })
	if res := e2.do(t, "GET", "/login", nil, nil); res.StatusCode != http.StatusBadGateway {
		t.Errorf("/login with failing discovery: %d", res.StatusCode)
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
	if res.Header.Get("Location") != "/" {
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
		if e.signIn.code != "" {
			t.Error("Login called despite state mismatch")
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
	t.Run("not in an allowed group", func(t *testing.T) {
		e := newEnv(t, nil)
		e.signIn.id = oidc.Identity{Subject: "sub-8", Username: "bob", Groups: []string{"users"}, Allowed: false}
		oc, q := e.startLogin(t, "/runs")
		res := e.do(t, "GET", "/oauth/callback?code=x&state="+q.Get("state"), nil, nil, oc)
		body := readBody(t, res)
		if res.StatusCode != 403 || cookie(res, "ansibled_session_lab") != nil ||
			!strings.Contains(body, "bob is not in an allowed group (sysadmins, ops).") {
			t.Errorf("status %d body %s", res.StatusCode, body)
		}
	})
	t.Run("login error", func(t *testing.T) {
		e := newEnv(t, nil)
		e.signIn.loginErr = errors.New("boom")
		oc, q := e.startLogin(t, "/runs")
		res := e.do(t, "GET", "/oauth/callback?code=x&state="+q.Get("state"), nil, nil, oc)
		body := readBody(t, res)
		if res.StatusCode != 502 || cookie(res, "ansibled_session_lab") != nil || strings.Contains(body, "boom") {
			t.Errorf("status %d body %s", res.StatusCode, body)
		}
	})
	t.Run("provider unreachable", func(t *testing.T) {
		e := newEnv(t, nil)
		e.signIn.urlErr = errors.New("oidc: discovery: connection refused")
		res := e.do(t, "GET", "/login?next=/runs", nil, nil)
		body := readBody(t, res)
		if res.StatusCode != 502 || !strings.Contains(body, "Could not reach the sign-in provider.") ||
			strings.Contains(body, "connection refused") || cookie(res, "ansibled_oauth_lab") != nil {
			t.Errorf("status %d body %s", res.StatusCode, body)
		}
	})
	t.Run("provider error param", func(t *testing.T) {
		e := newEnv(t, nil)
		oc, q := e.startLogin(t, "/runs")
		res := e.do(t, "GET", "/oauth/callback?error=access_denied&state="+q.Get("state"), nil, nil, oc)
		if res.StatusCode != 403 || cookie(res, "ansibled_session_lab") != nil || e.signIn.code != "" {
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
	forgedPayload, _ := e.s.sign("x", session{Version: sessionVersion, Subject: "1", Username: "mallory", Expires: time.Now().Add(time.Hour).Unix()})
	fp, _, _ := strings.Cut(forgedPayload, ".")
	redirected("tampered payload", &http.Cookie{Name: good.Name, Value: fp + "." + mac})
	redirected("tampered mac", &http.Cookie{Name: good.Name, Value: p + ".AAAA"})
	redirected("garbage", &http.Cookie{Name: good.Name, Value: "garbage"})

	// Signed with another key.
	other := newEnv(t, func(o *Options) { o.SessionKey = []byte("another-key-another-key-another!!") })
	redirected("other key", other.login(t))

	// Correctly signed cookies from before OIDC (GitLab user ids, no
	// version) or with another version are rejected.
	exp := time.Now().Add(time.Hour).Unix()
	gitlabEra, _ := e.s.sign(good.Name, map[string]any{"uid": 7, "user": "alice", "exp": exp})
	redirected("GitLab-era cookie", &http.Cookie{Name: good.Name, Value: gitlabEra})
	v1, _ := e.s.sign(good.Name, session{Version: 1, Subject: "sub-7", Username: "alice", Expires: exp})
	redirected("version 1 cookie", &http.Cookie{Name: good.Name, Value: v1})
	noSub, _ := e.s.sign(good.Name, session{Version: sessionVersion, Username: "alice", Expires: exp})
	redirected("cookie without subject", &http.Cookie{Name: good.Name, Value: noSub})
	current, _ := e.s.sign(good.Name, session{Version: sessionVersion, Subject: "sub-7", Username: "alice", Expires: exp})
	if res := e.do(t, "GET", "/runs", nil, nil, &http.Cookie{Name: good.Name, Value: current}); res.StatusCode != 200 {
		t.Errorf("hand-made current cookie: %d", res.StatusCode)
	}

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
		"":                      "/",
		"/runs/4":               "/runs/4",
		"/runs?page=2":          "/runs?page=2",
		"runs":                  "/",
		"//evil.example/":       "/",
		"/\\evil.example":       "/",
		"https://evil.example/": "/",
		"/runs\r\nSet-Cookie:x": "/",
		"/login":                "/",
		"/oauth/callback?x":     "/",
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
	if loc := res.Header.Get("Location"); loc != "/" {
		t.Errorf("redirect to %q", loc)
	}
}

func TestRootNeedsSession(t *testing.T) {
	e := newEnv(t, nil)
	if res := e.do(t, "GET", "/", nil, nil); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/login?next=%2F" {
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
	if !strings.Contains(body, `href="/runs/10"`) || !strings.Contains(body, `href="/runs">← Newer`) || strings.Contains(body, "page=3") {
		t.Errorf("page 2 wrong")
	}
}

func TestRunPage(t *testing.T) {
	e := newEnv(t, nil)
	evil := "<script>alert(1)</script>"
	r := mkRun(5, model.StateFailed)
	r.Summary = &model.Summary{Playbooks: []model.PlaybookResult{{Playbook: "site.yml", ExitCode: 2, Hosts: []model.HostResult{
		{Host: "web1", OK: 4, Changed: 1, ChangedTasks: []string{evil}},
		{Host: "db1", Failed: 1, FailedTasks: []string{"broken task"}},
	}}}}
	q := mkRun(6, model.StateQueued)
	x := mkRun(7, model.StateSuccess)
	x.LogsExpired = true
	e.runs.runs = []*model.Run{x, q, r}
	if err := os.MkdirAll(filepath.Join(e.logDir, "5"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.logDir, "5", "log.txt"), []byte("TASK [x]\n"), 0o644); err != nil {
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
	for _, s := range []string{"site.yml", "web1", "db1", "broken task", `/runs/5/log.txt`, `data-stream="/runs/5/stream"`,
		`id="h-web1"`, `data-active=""`} {
		if !strings.Contains(body, s) {
			t.Errorf("missing %q", s)
		}
	}
	if strings.Contains(body, "/rerun") {
		t.Error("buttons shown without Actions")
	}

	body = readBody(t, e.do(t, "GET", "/runs/6", nil, nil, sc))
	if !strings.Contains(body, `data-active="1"`) || !strings.Contains(body, "Waiting for output") {
		t.Error("queued run not marked active")
	}

	body = readBody(t, e.do(t, "GET", "/runs/7", nil, nil, sc))
	if !strings.Contains(body, "removed by retention") || strings.Contains(body, "data-stream") {
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

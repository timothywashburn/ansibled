package oidc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/timothywashburn/ansibled/internal/oidc/oidctest"
)

const (
	redirect = "http://ansibled.example:8471/oauth/callback"
	verifier = "verifier-0123456789-0123456789-0123456789-abc"
)

var ctx = context.Background()

func newProvider(t *testing.T, srv *oidctest.Server, mod ...func(*Options)) *Provider {
	t.Helper()
	o := Options{
		Issuer: srv.Issuer(), ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret,
		AllowedGroups: []string{"sysadmins"}, HTTPClient: srv.Client(),
		Logger: slog.New(slog.DiscardHandler),
	}
	for _, m := range mod {
		m(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newServer(t *testing.T) *oidctest.Server {
	t.Helper()
	srv := oidctest.New()
	t.Cleanup(srv.Close)
	return srv
}

// authorize plays the browser: GET the auth URL and return the code from
// the redirect to the callback.
func authorize(t *testing.T, srv *oidctest.Server, authURL, state string) string {
	t.Helper()
	hc := *srv.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), redirect+"?") || loc.Query().Get("state") != state {
		t.Fatalf("authorize redirect %q", resp.Header.Get("Location"))
	}
	return loc.Query().Get("code")
}

// signIn runs the whole flow for claims and returns the result of Login.
func signIn(t *testing.T, srv *oidctest.Server, p *Provider, claims map[string]any) (Identity, error) {
	t.Helper()
	srv.AddUser("code-"+t.Name(), claims)
	u, err := p.AuthCodeURL("st4te", verifier, redirect)
	if err != nil {
		t.Fatal(err)
	}
	code := authorize(t, srv, u, "st4te")
	return p.Login(ctx, code, verifier, redirect)
}

func TestNewValidates(t *testing.T) {
	good := Options{Issuer: "https://auth.example.org/", ClientID: "id", ClientSecret: "s", AllowedGroups: []string{"a"}}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Options){
		"relative issuer": func(o *Options) { o.Issuer = "auth.example.org" },
		"ftp issuer":      func(o *Options) { o.Issuer = "ftp://auth.example.org" },
		"issuer query":    func(o *Options) { o.Issuer = "https://auth.example.org/?a=b" },
		"no client id":    func(o *Options) { o.ClientID = "" },
		"no secret":       func(o *Options) { o.ClientSecret = "" },
		"no openid":       func(o *Options) { o.Scopes = []string{"profile"} },
		"no groups":       func(o *Options) { o.AllowedGroups = nil },
	} {
		o := good
		mod(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewNoNetworkAndHTTPWarning(t *testing.T) {
	srv := newServer(t)
	var logs bytes.Buffer
	newProvider(t, srv, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	if n := len(srv.Requests()); n != 0 {
		t.Errorf("New made %d requests", n)
	}
	if strings.Count(logs.String(), "plain http") != 1 {
		t.Errorf("want one plain-http warning, logs:\n%s", logs.String())
	}
	logs.Reset()
	if _, err := New(Options{Issuer: "https://auth.example.org", ClientID: "i", ClientSecret: "s", AllowedGroups: []string{"a"},
		Logger: slog.New(slog.NewTextHandler(&logs, nil))}); err != nil || logs.Len() != 0 {
		t.Errorf("https issuer: %v, logs %q", err, logs.String())
	}
}

func TestDiscoveryCachedAndRetried(t *testing.T) {
	srv := newServer(t)
	p := newProvider(t, srv)
	srv.FailNext(oidctest.Discovery, 1, http.StatusServiceUnavailable)
	if err := p.Check(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("Check with failing discovery: %v", err)
	}
	if _, err := p.AuthCodeURL("s", verifier, redirect); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	for range 3 {
		if _, err := p.AuthCodeURL("s", verifier, redirect); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if n := srv.CountRequests(oidctest.Discovery); n != 2 {
		t.Errorf("%d discovery requests, want 2 (one failure, then cached)", n)
	}
	if got := srv.Requests()[0]; got != "GET /.well-known/openid-configuration" {
		t.Errorf("discovery request %q", got)
	}
}

func TestDiscoveryConcurrent(t *testing.T) {
	srv := newServer(t)
	p := newProvider(t, srv)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := p.AuthCodeURL("s", verifier, redirect); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := srv.CountRequests(oidctest.Discovery); n != 1 {
		t.Errorf("%d discovery requests", n)
	}
}

func TestDiscoveryIssuerTrailingSlash(t *testing.T) {
	for _, tc := range []struct{ served, configured string }{
		{"/application/o/ansibled/", "/application/o/ansibled/"},
		{"/application/o/ansibled/", "/application/o/ansibled"},
		{"/realms/lab", "/realms/lab/"},
		{"/realms/lab", "/realms/lab"},
	} {
		srv := newServer(t)
		srv.SetIssuer(srv.URL() + tc.served)
		p := newProvider(t, srv, func(o *Options) { o.Issuer = srv.URL() + tc.configured })
		if err := p.Check(ctx); err != nil {
			t.Errorf("served %q configured %q: %v", tc.served, tc.configured, err)
			continue
		}
		want := "GET " + strings.TrimSuffix(tc.configured, "/") + "/.well-known/openid-configuration"
		if got := srv.Requests()[0]; got != want {
			t.Errorf("request %q, want %q", got, want)
		}
		if _, err := signIn(t, srv, p, map[string]any{"sub": "1", "groups": []any{"sysadmins"}}); err != nil {
			t.Errorf("sign-in with issuer path %q: %v", tc.served, err)
		}
	}
}

func TestDiscoveryRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(doc map[string]any)
		want string
	}{
		"issuer mismatch":      {func(d map[string]any) { d["issuer"] = "https://evil.example.org" }, "does not match the configured issuer"},
		"issuer extra slashes": {func(d map[string]any) { d["issuer"] = d["issuer"].(string) + "//" }, "does not match"},
		"no issuer":            {func(d map[string]any) { delete(d, "issuer") }, "does not match"},
		"no authorization":     {func(d map[string]any) { delete(d, "authorization_endpoint") }, "no authorization_endpoint"},
		"no token":             {func(d map[string]any) { delete(d, "token_endpoint") }, "no token_endpoint"},
		"no userinfo":          {func(d map[string]any) { delete(d, "userinfo_endpoint") }, "no userinfo_endpoint"},
		"relative endpoint":    {func(d map[string]any) { d["token_endpoint"] = "/token" }, "token_endpoint"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t)
			srv.EditDiscovery(tc.edit)
			p := newProvider(t, srv)
			if err := p.Check(ctx); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check: %v, want %q", err, tc.want)
			}
			if _, err := p.AuthCodeURL("s", verifier, redirect); err == nil {
				t.Error("AuthCodeURL works with a bad discovery document")
			}
			if _, err := p.Login(ctx, "c", verifier, redirect); err == nil {
				t.Error("Login works with a bad discovery document")
			}
			// Not cached: a fixed document works on the next attempt.
			srv.EditDiscovery(nil)
			if err := p.Check(ctx); err != nil {
				t.Errorf("after fix: %v", err)
			}
		})
	}
}

func TestDiscoveryNotJSON(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>login</html>"))
	}))
	defer hs.Close()
	p, _ := New(Options{Issuer: hs.URL, ClientID: "i", ClientSecret: "s", AllowedGroups: []string{"a"}, Logger: slog.New(slog.DiscardHandler)})
	if err := p.Check(ctx); err == nil || !strings.Contains(err.Error(), "not a JSON") {
		t.Errorf("Check: %v", err)
	}
}

func TestAuthCodeURL(t *testing.T) {
	srv := newServer(t)
	srv.SetIssuer(srv.URL() + "/application/o/ansibled/")
	p := newProvider(t, srv)
	got, err := p.AuthCodeURL("st4te", verifier, redirect)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got)
	q := u.Query()
	sum := sha256.Sum256([]byte(verifier))
	want := map[string]string{
		"client_id": oidctest.ClientID, "redirect_uri": redirect, "response_type": "code",
		"scope": "openid profile email", "state": "st4te",
		"code_challenge_method": "S256", "code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]),
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if u.Path != "/application/o/ansibled/authorize" {
		t.Errorf("path %q", u.Path)
	}
	if strings.Contains(got, verifier) || strings.Contains(got, oidctest.ClientSecret) {
		t.Errorf("auth URL leaks the verifier or secret: %s", got)
	}

	p2 := newProvider(t, srv, func(o *Options) { o.Scopes = []string{"openid", "groups"} })
	got, _ = p2.AuthCodeURL("s", verifier, redirect)
	if u, _ := url.Parse(got); u.Query().Get("scope") != "openid groups" {
		t.Errorf("custom scopes: %s", got)
	}
	if _, err := p.AuthCodeURL("", verifier, redirect); err == nil {
		t.Error("empty state accepted")
	}
	if _, err := p.AuthCodeURL("s", "", redirect); err == nil {
		t.Error("empty verifier accepted")
	}
}

func TestLoginAllowedAndDenied(t *testing.T) {
	srv := newServer(t)
	p := newProvider(t, srv, func(o *Options) { o.AllowedGroups = []string{"sysadmins", "ops"} })

	id, err := signIn(t, srv, p, map[string]any{
		"sub": "u-1", "preferred_username": "alice", "email": "alice@example.org",
		"groups": []any{"users", "ops"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "u-1" || id.Username != "alice" || !slices.Equal(id.Groups, []string{"users", "ops"}) || !id.Allowed {
		t.Errorf("alice: %+v", id)
	}

	id, err = signIn(t, srv, p, map[string]any{"sub": "u-2", "preferred_username": "bob", "groups": []any{"users", "Sysadmins", "sysadmins "}})
	if err != nil {
		t.Fatal(err)
	}
	if id.Allowed || id.Username != "bob" {
		t.Errorf("bob (case/space variants must not match): %+v", id)
	}
	// Tokens are single use at the fake; Login never reuses them.
	if n := srv.CountRequests(oidctest.Userinfo); n != 2 {
		t.Errorf("%d userinfo requests", n)
	}
}

func TestGroupsClaim(t *testing.T) {
	for name, tc := range map[string]struct {
		claim   string
		claims  map[string]any
		groups  []string
		allowed bool
	}{
		"array":             {"", map[string]any{"groups": []any{"a", "sysadmins"}}, []string{"a", "sysadmins"}, true},
		"array non-strings": {"", map[string]any{"groups": []any{1, "sysadmins", nil, map[string]any{"x": 1}}}, []string{"sysadmins"}, true},
		"single string":     {"", map[string]any{"groups": "sysadmins"}, []string{"sysadmins"}, true},
		"missing":           {"", map[string]any{}, nil, false},
		"wrong type":        {"", map[string]any{"groups": 7}, nil, false},
		"empty":             {"", map[string]any{"groups": []any{}}, nil, false},
		"custom claim":      {"roles", map[string]any{"roles": []any{"sysadmins"}, "groups": []any{"x"}}, []string{"sysadmins"}, true},
		"custom missing":    {"roles", map[string]any{"groups": []any{"sysadmins"}}, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t)
			p := newProvider(t, srv, func(o *Options) { o.GroupsClaim = tc.claim })
			tc.claims["sub"] = "s1"
			id, err := signIn(t, srv, p, tc.claims)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(id.Groups, tc.groups) || id.Allowed != tc.allowed {
				t.Errorf("got %+v, want groups %v allowed %v", id, tc.groups, tc.allowed)
			}
		})
	}
}

func TestUsernameFallback(t *testing.T) {
	for name, tc := range map[string]struct {
		claims map[string]any
		want   string
	}{
		"preferred_username": {map[string]any{"sub": "s", "preferred_username": "alice", "email": "a@example.org"}, "alice"},
		"email":              {map[string]any{"sub": "s", "preferred_username": "", "email": "a@example.org"}, "a@example.org"},
		"sub":                {map[string]any{"sub": "s-123"}, "s-123"},
		"non-string":         {map[string]any{"sub": "s-9", "preferred_username": 5}, "s-9"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t)
			id, err := signIn(t, srv, newProvider(t, srv), tc.claims)
			if err != nil || id.Username != tc.want {
				t.Errorf("Username = %q (%v), want %q", id.Username, err, tc.want)
			}
		})
	}
	srv := newServer(t)
	if _, err := signIn(t, srv, newProvider(t, srv), map[string]any{"preferred_username": "nosub"}); err == nil || !strings.Contains(err.Error(), "sub") {
		t.Errorf("missing sub: %v", err)
	}
	srv = newServer(t)
	if _, err := signIn(t, srv, newProvider(t, srv), map[string]any{"sub": 42}); err == nil {
		t.Error("numeric sub accepted")
	}
}

func TestTokenEndpointErrors(t *testing.T) {
	t.Run("wrong verifier", func(t *testing.T) {
		srv := newServer(t)
		p := newProvider(t, srv)
		srv.AddUser("c1", map[string]any{"sub": "1"})
		u, _ := p.AuthCodeURL("s", verifier, redirect)
		code := authorize(t, srv, u, "s")
		_, err := p.Login(ctx, code, verifier+"x", redirect)
		if err == nil || !strings.Contains(err.Error(), "token endpoint: HTTP 400: invalid_grant") {
			t.Fatalf("wrong verifier: %v", err)
		}
		// Correct verifier still works, once.
		if _, err := p.Login(ctx, code, verifier, redirect); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Login(ctx, code, verifier, redirect); err == nil {
			t.Error("code reused")
		}
	})
	t.Run("redirect mismatch", func(t *testing.T) {
		srv := newServer(t)
		p := newProvider(t, srv)
		srv.AddUser("c1", map[string]any{"sub": "1"})
		u, _ := p.AuthCodeURL("s", verifier, redirect)
		code := authorize(t, srv, u, "s")
		if _, err := p.Login(ctx, code, verifier, redirect+"x"); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
			t.Fatalf("redirect mismatch: %v", err)
		}
	})
	t.Run("wrong secret", func(t *testing.T) {
		srv := newServer(t)
		p := newProvider(t, srv, func(o *Options) { o.ClientSecret = "wrong-secret-value" })
		_, err := signIn(t, srv, p, map[string]any{"sub": "1"})
		if err == nil || !strings.Contains(err.Error(), "HTTP 401: invalid_client") {
			t.Fatalf("wrong secret: %v", err)
		}
		if strings.Contains(err.Error(), "wrong-secret-value") {
			t.Errorf("error leaks the secret: %v", err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		srv := newServer(t)
		p := newProvider(t, srv)
		srv.FailNext(oidctest.Token, 1, http.StatusBadGateway)
		if _, err := signIn(t, srv, p, map[string]any{"sub": "1"}); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
			t.Fatalf("token 502: %v", err)
		}
	})
	t.Run("unknown code", func(t *testing.T) {
		srv := newServer(t)
		p := newProvider(t, srv)
		if _, err := p.Login(ctx, "never-issued", verifier, redirect); err == nil {
			t.Fatal("unknown code accepted")
		}
	})
}

func TestUserinfoErrors(t *testing.T) {
	srv := newServer(t)
	p := newProvider(t, srv)
	srv.FailNext(oidctest.Userinfo, 1, http.StatusUnauthorized)
	_, err := signIn(t, srv, p, map[string]any{"sub": "1"})
	if err == nil || !strings.Contains(err.Error(), "userinfo: HTTP 401") {
		t.Fatalf("userinfo 401: %v", err)
	}
	if strings.Contains(err.Error(), "fake-oidc-access-token") {
		t.Errorf("error leaks the access token: %v", err)
	}
}

// TestErrorsHideSecrets uses a provider that echoes the credentials it gets
// into every error body.
func TestErrorsHideSecrets(t *testing.T) {
	const secret = "s3cr3t-client-secret"
	const access = "at-very-secret-access-token"
	var hs *httptest.Server
	tokenMode := "error"
	hs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"issuer":"` + hs.URL + `","authorization_endpoint":"` + hs.URL + `/a","token_endpoint":"` +
				hs.URL + `/t","userinfo_endpoint":"` + hs.URL + `/u"}`))
		case "/t":
			r.ParseForm()
			echo := r.Header.Get("Authorization") + " " + r.PostForm.Encode()
			if tokenMode == "error" {
				http.Error(w, "boom: "+echo, http.StatusInternalServerError)
				return
			}
			if tokenMode == "garbage" {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte("{not json " + echo))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"` + access + `","token_type":"Bearer"}`))
		case "/u":
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", token="`+r.Header.Get("Authorization")+`"`)
			http.Error(w, "bad token "+r.Header.Get("Authorization"), http.StatusForbidden)
		}
	}))
	defer hs.Close()
	p, err := New(Options{Issuer: hs.URL, ClientID: "cid", ClientSecret: secret, AllowedGroups: []string{"a"}, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("cid:" + secret))
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error", what)
		}
		for _, bad := range []string{secret, basic, access, verifier, "the-code"} {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("%s: error contains %q: %v", what, bad, err)
			}
		}
	}
	for _, mode := range []string{"error", "garbage", "ok"} {
		tokenMode = mode
		_, err := p.Login(ctx, "the-code", verifier, redirect)
		check("token mode "+mode, err)
		t.Logf("%s: %v", mode, err)
	}
}

func TestUserinfoJWTAndNonObject(t *testing.T) {
	var hs *httptest.Server
	ct, body := "", ""
	hs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Write([]byte(`{"issuer":"` + hs.URL + `/","authorization_endpoint":"` + hs.URL + `/a","token_endpoint":"` +
				hs.URL + `/t","userinfo_endpoint":"` + hs.URL + `/u"}`))
		case "/t":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"x","token_type":"Bearer"}`))
		case "/u":
			w.Header().Set("Content-Type", ct)
			w.Write([]byte(body))
		}
	}))
	defer hs.Close()
	p, _ := New(Options{Issuer: hs.URL, ClientID: "c", ClientSecret: "s", AllowedGroups: []string{"a"}, Logger: slog.New(slog.DiscardHandler)})
	ct, body = "application/jwt; charset=utf-8", "eyJ.eyJ.sig"
	if _, err := p.Login(ctx, "c", verifier, redirect); err == nil || !strings.Contains(err.Error(), "application/jwt") {
		t.Errorf("jwt userinfo: %v", err)
	}
	ct, body = "application/json", `["sub"]`
	if _, err := p.Login(ctx, "c", verifier, redirect); err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Errorf("array userinfo: %v", err)
	}
	ct, body = "application/json", `{"sub":"x","groups":["a"]}`
	if id, err := p.Login(ctx, "c", verifier, redirect); err != nil || !id.Allowed {
		t.Errorf("good userinfo: %+v %v", id, err)
	}
}

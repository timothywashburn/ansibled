// Package oidctest is an in-memory fake OpenID Connect provider for tests.
// It serves the discovery document, the authorization, token and userinfo
// endpoints (all under the issuer's path), checks the client credentials,
// redirect URI and PKCE like a strict provider, and records every request.
//
// Sign-ins are queued with AddUser: each GET of the authorization endpoint
// redirects to redirect_uri?code=<next queued code>&state=<state>; the token
// endpoint trades that code (once) for an access token, and the userinfo
// endpoint returns the code's claims for that token.
package oidctest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// Client credentials the fake accepts.
const (
	ClientID     = "fake-oidc-client-id"
	ClientSecret = "fake-oidc-client-secret"
)

// Endpoint paths, relative to the issuer's path (for FailNext).
const (
	Discovery = "/.well-known/openid-configuration"
	Authorize = "/authorize"
	Token     = "/token"
	Userinfo  = "/userinfo"
)

type grant struct {
	code       string
	claims     map[string]any
	authorized bool
	challenge  string
	redirect   string
	used       bool
}

type failure struct {
	n      int
	status int
}

// Server is a fake OpenID Connect provider. All methods are safe for
// concurrent use.
type Server struct {
	srv *httptest.Server

	mu        sync.Mutex
	issuer    string // "" = URL()
	grants    []*grant
	tokens    map[string]*grant
	fail      map[string]failure
	discovery func(doc map[string]any)
	requests  []string
	nextID    int64
}

// New starts a fake provider on a random loopback port. Its issuer is URL().
func New() *Server {
	s := newServer()
	s.srv = httptest.NewServer(s)
	return s
}

// NewWithListener is New, but serves on l instead of a random loopback port,
// e.g. on an address a container can reach (end-to-end tests). The server
// takes ownership of l; Close closes it.
func NewWithListener(l net.Listener) *Server {
	s := newServer()
	s.srv = httptest.NewUnstartedServer(s)
	_ = s.srv.Listener.Close()
	s.srv.Listener = l
	s.srv.Start()
	return s
}

func newServer() *Server {
	return &Server{tokens: map[string]*grant{}, fail: map[string]failure{}, nextID: 1}
}

// URL is the server's base URL, e.g. http://127.0.0.1:1234.
func (s *Server) URL() string { return s.srv.URL }

// Issuer is the issuer the discovery document announces (URL() unless
// SetIssuer was called). The endpoints are served under its path.
func (s *Server) Issuer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issuerLocked()
}

func (s *Server) issuerLocked() string {
	if s.issuer != "" {
		return s.issuer
	}
	return s.srv.URL
}

// SetIssuer changes the issuer, e.g. to a URL with a path
// ("http://host:port/application/o/ansibled/") or with the host name a
// container uses. The endpoints in the discovery document are built from it,
// and requests are routed by its path (the host is not checked).
func (s *Server) SetIssuer(issuer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issuer = issuer
}

// Client returns an HTTP client for the server.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Close shuts the server down.
func (s *Server) Close() { s.srv.Close() }

// AddUser queues a sign-in: the next authorization request redirects with
// code, and the userinfo endpoint answers that code's token with claims
// (e.g. {"sub": "1", "preferred_username": "alice", "groups": []any{"sysadmins"}}).
func (s *Server) AddUser(code string, claims map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, &grant{code: code, claims: maps.Clone(claims)})
}

// FailNext makes the next n requests to endpoint (Discovery, Authorize,
// Token or Userinfo) answer with status and a JSON error body.
func (s *Server) FailNext(endpoint string, n, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[endpoint] = failure{n: n, status: status}
}

// EditDiscovery sets a function that modifies every discovery document
// before it is sent (e.g. delete an endpoint, change the issuer). nil resets.
func (s *Server) EditDiscovery(f func(doc map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discovery = f
}

// Requests returns every request received, as "METHOD /escaped/path?query".
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// CountRequests counts received requests whose path (no query) ends with
// suffix, e.g. CountRequests(oidctest.Discovery).
func (s *Server) CountRequests(suffix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		r, _, _ = strings.Cut(r, "?")
		if strings.HasSuffix(r, suffix) {
			n++
		}
	}
	return n
}

// ---- HTTP ----

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	esc := r.URL.EscapedPath()
	rec := r.Method + " " + esc
	if r.URL.RawQuery != "" {
		rec += "?" + r.URL.RawQuery
	}
	s.requests = append(s.requests, rec)

	base := ""
	if u, err := url.Parse(s.issuerLocked()); err == nil {
		base = strings.TrimSuffix(u.EscapedPath(), "/")
	}
	endpoint, ok := strings.CutPrefix(esc, base)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch endpoint {
	case Discovery, Authorize, Token, Userinfo:
	default:
		http.NotFound(w, r)
		return
	}
	if f := s.fail[endpoint]; f.n > 0 {
		f.n--
		s.fail[endpoint] = f
		writeJSON(w, f.status, map[string]any{"error": "server_error", "error_description": "injected failure"})
		return
	}
	switch endpoint {
	case Discovery:
		s.serveDiscovery(w, r)
	case Authorize:
		s.serveAuthorize(w, r)
	case Token:
		s.serveToken(w, r)
	case Userinfo:
		s.serveUserinfo(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	iss := s.issuerLocked()
	base := strings.TrimSuffix(iss, "/")
	doc := map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                base + Authorize,
		"token_endpoint":                        base + Token,
		"userinfo_endpoint":                     base + Userinfo,
		"jwks_uri":                              base + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
		"code_challenge_methods_supported":      []string{"S256"},
		"grant_types_supported":                 []string{"authorization_code"},
	}
	if s.discovery != nil {
		s.discovery(doc)
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bad := func(msg string) { http.Error(w, msg, http.StatusBadRequest) }
	switch {
	case r.Method != http.MethodGet:
		bad("GET required")
	case q.Get("client_id") != ClientID:
		bad("unknown client_id")
	case q.Get("response_type") != "code":
		bad("response_type must be code")
	case q.Get("redirect_uri") == "":
		bad("redirect_uri missing")
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		bad("PKCE S256 code_challenge required")
	case !slices.Contains(strings.Fields(q.Get("scope")), "openid"):
		bad("scope openid required")
	default:
		u, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || !u.IsAbs() {
			bad("bad redirect_uri")
			return
		}
		for _, g := range s.grants {
			if g.authorized {
				continue
			}
			g.authorized, g.challenge, g.redirect = true, q.Get("code_challenge"), q.Get("redirect_uri")
			rq := u.Query()
			rq.Set("code", g.code)
			if st := q.Get("state"); st != "" {
				rq.Set("state", st)
			}
			u.RawQuery = rq.Encode()
			http.Redirect(w, r, u.String(), http.StatusFound)
			return
		}
		bad("no pending sign-in (call AddUser)")
	}
}

func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	oauthErr := func(status int, code, desc string) {
		writeJSON(w, status, map[string]any{"error": code, "error_description": desc})
	}
	if r.Method != http.MethodPost {
		oauthErr(http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthErr(http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	// client_secret_basic: id and secret are form-urlencoded, then basic auth.
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	}
	if !ok || id != ClientID || secret != ClientSecret {
		w.Header().Set("WWW-Authenticate", `Basic realm="oidctest"`)
		oauthErr(http.StatusUnauthorized, "invalid_client", "Client authentication failed")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		oauthErr(http.StatusBadRequest, "unsupported_grant_type", "only authorization_code")
		return
	}
	var g *grant
	for _, x := range s.grants {
		if x.code == r.PostForm.Get("code") && x.authorized && !x.used {
			g = x
		}
	}
	if g == nil {
		oauthErr(http.StatusBadRequest, "invalid_grant", "unknown, unauthorized or used code")
		return
	}
	if r.PostForm.Get("redirect_uri") != g.redirect {
		oauthErr(http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		oauthErr(http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code_challenge")
		return
	}
	g.used = true
	n := s.nextID
	s.nextID++
	tok := fmt.Sprintf("fake-oidc-access-token-%d", n)
	s.tokens[tok] = g
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok, "token_type": "Bearer", "expires_in": 300,
		"id_token": fmt.Sprintf("fake-oidc-id-token-%d", n), "scope": "openid profile email",
	})
}

func (s *Server) serveUserinfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g := s.tokens[tok]
	if !ok || g == nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_token"})
		return
	}
	writeJSON(w, http.StatusOK, g.claims)
}

package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// sessionVersion is bumped whenever the session payload changes meaning, so
// older cookies (version 0/1: GitLab user ids) are rejected.
const sessionVersion = 2

// session is the signed-in user, stored in a signed cookie.
type session struct {
	Version  int    `json:"v"`
	Subject  string `json:"sub"`  // OIDC subject
	Username string `json:"user"` // for display and logs
	Expires  int64  `json:"exp"`  // unix seconds
}

// oauthState is what /login remembers for /oauth/callback.
type oauthState struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
	Expires  int64  `json:"exp"`
}

// sign returns "<payload>.<mac>" (both base64url) for v. The MAC covers the
// cookie name so a cookie of one kind cannot be replayed as another.
func (s *Server) sign(name string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + base64.RawURLEncoding.EncodeToString(s.mac(name, p)), nil
}

func (s *Server) mac(name, payload string) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// verify checks a cookie value made by sign and decodes it into v.
func (s *Server) verify(name, value string, v any) bool {
	p, m, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(m)
	if err != nil || !hmac.Equal(got, s.mac(name, p)) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// readCookie verifies and decodes the named cookie.
func (s *Server) readCookie(r *http.Request, name string, v any) bool {
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	return s.verify(name, c.Value, v)
}

func (s *Server) setCookie(w http.ResponseWriter, name string, v any, ttl time.Duration) error {
	val, err := s.sign(name, v)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: val, Path: "/", MaxAge: int(ttl / time.Second),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// currentSession returns the valid, unexpired session of the request, if any.
func (s *Server) currentSession(r *http.Request) (*session, bool) {
	var sess session
	if !s.readCookie(r, s.sessionName, &sess) || sess.Version != sessionVersion ||
		s.now().Unix() >= sess.Expires || sess.Subject == "" || sess.Username == "" {
		return nil, false
	}
	return &sess, true
}

// requireSession redirects to /login unless the request has a valid session.
func (s *Server) requireSession(h func(http.ResponseWriter, *http.Request, *session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		sess, ok := s.currentSession(r)
		if !ok {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		h(w, r, sess)
	})
}

// safeNext returns next if it is a local path, otherwise "/runs". It rejects
// absolute and scheme-relative URLs ("//host", "/\host"), control characters
// and the auth endpoints themselves.
func safeNext(next string) string {
	const def = "/runs"
	if len(next) == 0 || next[0] != '/' {
		return def
	}
	if len(next) > 1 && (next[1] == '/' || next[1] == '\\') {
		return def
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return def
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return def
	}
	switch u.Path {
	case "/login", "/logout", "/oauth/callback":
		return def
	}
	return next
}

// randToken returns 32 random bytes, base64url without padding.
func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// login starts the OpenID Connect authorization code flow with PKCE.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	state, err1 := randToken()
	verifier, err2 := randToken()
	st := oauthState{
		State: state, Verifier: verifier,
		Next:    safeNext(r.URL.Query().Get("next")),
		Expires: s.now().Add(oauthTTL).Unix(),
	}
	if err1 != nil || err2 != nil {
		s.message(w, r, nil, http.StatusInternalServerError, "Error", "Could not start sign-in.", "", "")
		return
	}
	authURL, err := s.o.SignIn.AuthCodeURL(state, verifier, s.redirectURL)
	if err != nil {
		s.log.Warn("sign-in provider unavailable", "err", err)
		s.message(w, r, nil, http.StatusBadGateway, "Sign-in unavailable", "Could not reach the sign-in provider.", "/login", "Try again")
		return
	}
	if err := s.setCookie(w, s.oauthName, st, oauthTTL); err != nil {
		s.message(w, r, nil, http.StatusInternalServerError, "Error", "Could not start sign-in.", "", "")
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// callback finishes sign-in: checks state, has the provider exchange the
// code, checks the groups and sets the session cookie.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var st oauthState
	ok := s.readCookie(r, s.oauthName, &st)
	s.clearCookie(w, s.oauthName) // single use
	q := r.URL.Query()
	if !ok || st.State == "" || s.now().Unix() >= st.Expires {
		s.message(w, r, nil, http.StatusBadRequest, "Sign-in expired",
			"The sign-in attempt is missing or expired.", "/login", "Try again")
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		s.log.Warn("oauth callback: state mismatch", "remote", r.RemoteAddr)
		s.message(w, r, nil, http.StatusBadRequest, "Sign-in failed",
			"The sign-in response did not match this browser's request.", "/login", "Try again")
		return
	}
	if e := q.Get("error"); e != "" {
		s.message(w, r, nil, http.StatusForbidden, "Sign-in failed",
			"The sign-in was not completed: "+truncate(e, 200), "/login", "Try again")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.message(w, r, nil, http.StatusBadRequest, "Sign-in failed", "No authorization code was returned.", "/login", "Try again")
		return
	}
	id, err := s.o.SignIn.Login(r.Context(), code, st.Verifier, s.redirectURL)
	if err != nil {
		s.log.Warn("sign-in failed", "err", err)
		s.message(w, r, nil, http.StatusBadGateway, "Sign-in failed", "Could not complete sign-in with the provider.", "/login", "Try again")
		return
	}
	if !id.Allowed {
		s.log.Warn("sign-in refused: not in an allowed group", "user", id.Username, "sub", id.Subject, "groups", id.Groups)
		s.message(w, r, nil, http.StatusForbidden, "Access denied",
			truncate(id.Username, 200)+" is not in an allowed group ("+strings.Join(s.cfg.Web.OIDC.AllowedGroups, ", ")+").", "", "")
		return
	}
	sess := session{Version: sessionVersion, Subject: id.Subject, Username: id.Username, Expires: s.now().Add(sessionTTL).Unix()}
	if err := s.setCookie(w, s.sessionName, sess, sessionTTL); err != nil {
		s.message(w, r, nil, http.StatusInternalServerError, "Error", "Could not create a session.", "", "")
		return
	}
	s.log.Info("signed in", "user", id.Username, "sub", id.Subject)
	http.Redirect(w, r, safeNext(st.Next), http.StatusFound)
}

// logout clears the session.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.clearCookie(w, s.sessionName)
	s.message(w, r, nil, http.StatusOK, "Signed out", "You are signed out.", "/login", "Sign in again")
}

// metrics serves Prometheus metrics to requests with the bearer token.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if !s.metricsAuthorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ansibled"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.o.Metrics.Handler().ServeHTTP(w, r)
}

func (s *Server) metricsAuthorized(r *http.Request) bool {
	if s.o.MetricsToken == "" {
		return false // fail closed
	}
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	// Compare digests so the comparison does not leak the token length.
	a := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	b := sha256.Sum256([]byte(s.o.MetricsToken))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

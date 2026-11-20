package oidctest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const (
	redirect = "http://ansibled.invalid/oauth/callback"
	verifier = "verifier-0123456789-0123456789-0123456789-abc"
)

func challenge(v string) string {
	s := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

func noRedirect(s *Server) *http.Client {
	c := *s.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func authParams() url.Values {
	return url.Values{
		"client_id": {ClientID}, "response_type": {"code"}, "redirect_uri": {redirect},
		"scope": {"openid profile"}, "state": {"st"}, "code_challenge": {challenge(verifier)},
		"code_challenge_method": {"S256"},
	}
}

func get(t *testing.T, c *http.Client, u string) *http.Response {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func token(t *testing.T, s *Server, id, secret string, form url.Values) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.URL()+Token, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if id != "" {
		req.SetBasicAuth(url.QueryEscape(id), url.QueryEscape(secret))
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestDiscoveryAndIssuerPath(t *testing.T) {
	s := New()
	defer s.Close()
	if s.Issuer() != s.URL() {
		t.Fatalf("default issuer %q", s.Issuer())
	}
	s.SetIssuer("http://host.docker.internal:9999/application/o/ansibled/")
	resp, err := s.Client().Get(s.URL() + "/application/o/ansibled" + Discovery)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if doc["issuer"] != "http://host.docker.internal:9999/application/o/ansibled/" ||
		doc["token_endpoint"] != "http://host.docker.internal:9999/application/o/ansibled/token" {
		t.Errorf("doc %v", doc)
	}
	if r := get(t, s.Client(), s.URL()+Discovery); r.StatusCode != http.StatusNotFound {
		t.Errorf("discovery outside the issuer path: %d", r.StatusCode)
	}
	s.EditDiscovery(func(d map[string]any) { delete(d, "userinfo_endpoint") })
	resp, _ = s.Client().Get(s.URL() + "/application/o/ansibled" + Discovery)
	doc = nil
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if _, ok := doc["userinfo_endpoint"]; ok {
		t.Error("EditDiscovery not applied")
	}
	if n := s.CountRequests(Discovery); n != 3 {
		t.Errorf("CountRequests = %d", n)
	}
}

func TestAuthorizeValidates(t *testing.T) {
	s := New()
	defer s.Close()
	s.AddUser("code-1", map[string]any{"sub": "1"})
	c := noRedirect(s)
	for name, mod := range map[string]func(url.Values){
		"client_id":     func(v url.Values) { v.Set("client_id", "other") },
		"response_type": func(v url.Values) { v.Set("response_type", "token") },
		"redirect_uri":  func(v url.Values) { v.Del("redirect_uri") },
		"no challenge":  func(v url.Values) { v.Del("code_challenge") },
		"plain pkce":    func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"no openid":     func(v url.Values) { v.Set("scope", "profile") },
	} {
		v := authParams()
		mod(v)
		if r := get(t, c, s.URL()+Authorize+"?"+v.Encode()); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", name, r.StatusCode)
		}
	}
	r := get(t, c, s.URL()+Authorize+"?"+authParams().Encode())
	loc, _ := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != http.StatusFound || loc.Query().Get("code") != "code-1" || loc.Query().Get("state") != "st" ||
		!strings.HasPrefix(loc.String(), redirect+"?") {
		t.Fatalf("authorize: %d %s", r.StatusCode, loc)
	}
	if r := get(t, c, s.URL()+Authorize+"?"+authParams().Encode()); r.StatusCode != http.StatusBadRequest {
		t.Errorf("second authorize without a queued user: %d", r.StatusCode)
	}
}

func TestTokenAndUserinfo(t *testing.T) {
	s := New()
	defer s.Close()
	s.AddUser("code-1", map[string]any{"sub": "1", "groups": []any{"sysadmins"}})
	form := url.Values{"grant_type": {"authorization_code"}, "code": {"code-1"}, "redirect_uri": {redirect}, "code_verifier": {verifier}}

	// Not authorized yet.
	if st, m := token(t, s, ClientID, ClientSecret, form); st != 400 || m["error"] != "invalid_grant" {
		t.Errorf("before authorize: %d %v", st, m)
	}
	get(t, noRedirect(s), s.URL()+Authorize+"?"+authParams().Encode())

	if st, m := token(t, s, "", "", form); st != 401 || m["error"] != "invalid_client" {
		t.Errorf("no basic auth: %d %v", st, m)
	}
	if st, _ := token(t, s, ClientID, "wrong", form); st != 401 {
		t.Errorf("wrong secret: %d", st)
	}
	bad := url.Values{}
	for k, v := range form {
		bad[k] = v
	}
	bad.Set("redirect_uri", redirect+"/x")
	if st, _ := token(t, s, ClientID, ClientSecret, bad); st != 400 {
		t.Errorf("redirect mismatch: %d", st)
	}
	bad.Set("redirect_uri", redirect)
	bad.Set("code_verifier", "other")
	if st, _ := token(t, s, ClientID, ClientSecret, bad); st != 400 {
		t.Errorf("verifier mismatch: %d", st)
	}
	st, m := token(t, s, ClientID, ClientSecret, form)
	tok, _ := m["access_token"].(string)
	if st != 200 || tok == "" || m["token_type"] != "Bearer" {
		t.Fatalf("token: %d %v", st, m)
	}
	if st, _ := token(t, s, ClientID, ClientSecret, form); st != 400 {
		t.Errorf("code reused: %d", st)
	}

	req, _ := http.NewRequest(http.MethodGet, s.URL()+Userinfo, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	json.NewDecoder(resp.Body).Decode(&claims)
	resp.Body.Close()
	if resp.StatusCode != 200 || claims["sub"] != "1" {
		t.Errorf("userinfo: %d %v", resp.StatusCode, claims)
	}
	req.Header.Set("Authorization", "Bearer nope")
	resp, _ = s.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "invalid_token") {
		t.Errorf("bad token: %d", resp.StatusCode)
	}

	s.FailNext(Userinfo, 1, 503)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ = s.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("FailNext: %d", resp.StatusCode)
	}
	resp, _ = s.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("after FailNext: %d", resp.StatusCode)
	}
}

func TestNewWithListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewWithListener(l)
	defer s.Close()
	if want := "http://" + l.Addr().String(); s.URL() != want || s.Issuer() != want {
		t.Fatalf("URL() = %q, Issuer() = %q, want %q", s.URL(), s.Issuer(), want)
	}
	if r := get(t, s.Client(), s.URL()+Discovery); r.StatusCode != 200 {
		t.Fatalf("discovery: %d", r.StatusCode)
	}
	if got := s.Requests(); len(got) != 1 || got[0] != "GET "+Discovery {
		t.Fatalf("requests %v", got)
	}
}

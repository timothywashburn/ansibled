// Package oidc signs people in to the web UI with any OpenID Connect provider
// (authentik, Keycloak, Google, ...), see DESIGN.md "Web UI and auth".
//
// It uses the authorization code flow with PKCE (S256) and a confidential
// client. After the code exchange it reads the userinfo endpoint with the
// access token and decides access by the configured groups claim. ID tokens
// are not validated: the userinfo response is fetched server side, over the
// provider's own TLS, and every token is discarded after sign-in.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// Timeouts and limits.
const (
	discoveryTimeout = 30 * time.Second
	requestTimeout   = 30 * time.Second
	maxBodyBytes     = 1 << 20
	maxErrorText     = 200
	userAgent        = "ansibled"
	discoveryPath    = "/.well-known/openid-configuration"
)

// Defaults for Options.
const DefaultGroupsClaim = "groups"

// DefaultScopes returns the scopes used when Options.Scopes is empty.
func DefaultScopes() []string { return []string{"openid", "profile", "email"} }

// Options configure a Provider.
type Options struct {
	// Issuer is the provider's issuer URL, e.g.
	// https://auth.example.org/application/o/ansibled/.
	Issuer       string
	ClientID     string
	ClientSecret string
	// Scopes requested; default openid, profile, email. Must include openid.
	Scopes []string
	// GroupsClaim is the userinfo claim holding the user's groups; default "groups".
	GroupsClaim string
	// AllowedGroups: a user must be in at least one (exact, case-sensitive).
	AllowedGroups []string
	// HTTPClient is used for discovery, the token and the userinfo endpoint.
	// Default: a client with a 30 s timeout.
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Identity is a signed-in user as the provider describes them.
type Identity struct {
	Subject  string   // "sub", stable per provider
	Username string   // preferred_username, else email, else sub
	Groups   []string // from the groups claim
	Allowed  bool     // in at least one allowed group
}

// Provider is one OpenID Connect provider. It is safe for concurrent use.
type Provider struct {
	o            Options
	discoveryURL string
	http         *http.Client
	log          *slog.Logger

	mu   sync.Mutex
	meta *metadata // nil until discovery succeeded
}

// metadata is the part of the discovery document ansibled uses.
type metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// New validates the options. It does not touch the network: discovery runs
// on first use.
func New(o Options) (*Provider, error) {
	u, err := url.Parse(o.Issuer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("oidc: issuer must be an absolute http(s) URL without query or fragment")
	}
	if o.ClientID == "" {
		return nil, errors.New("oidc: client id is required")
	}
	if o.ClientSecret == "" {
		return nil, errors.New("oidc: client secret is required")
	}
	if len(o.Scopes) == 0 {
		o.Scopes = DefaultScopes()
	}
	if !slices.Contains(o.Scopes, "openid") {
		return nil, errors.New("oidc: scopes must include openid")
	}
	if o.GroupsClaim == "" {
		o.GroupsClaim = DefaultGroupsClaim
	}
	if len(o.AllowedGroups) == 0 {
		return nil, errors.New("oidc: at least one allowed group is required")
	}
	o.Scopes = slices.Clone(o.Scopes)
	o.AllowedGroups = slices.Clone(o.AllowedGroups)
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	p := &Provider{
		o:            o,
		discoveryURL: strings.TrimSuffix(o.Issuer, "/") + discoveryPath,
		http:         hc,
		log:          o.Logger,
	}
	if u.Scheme == "http" {
		p.log.Warn("OIDC issuer uses plain http; only use this for tests and labs", "issuer", o.Issuer)
	}
	return p, nil
}

// Check runs discovery (if it has not succeeded yet) and reports problems.
func (p *Provider) Check(ctx context.Context) error {
	_, err := p.discover(ctx)
	return err
}

// discover returns the cached metadata or fetches it. Failures are not
// cached, so the next call retries.
func (p *Provider) discover(ctx context.Context) (*metadata, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meta != nil {
		return p.meta, nil
	}
	m, err := p.fetchMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery %s: %w", p.discoveryURL, err)
	}
	p.meta = m
	return m, nil
}

func (p *Provider) fetchMetadata(ctx context.Context) (*metadata, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.discoveryURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, stripURL(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var m metadata
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("not a JSON discovery document: %w", err)
	}
	if strings.TrimSuffix(m.Issuer, "/") != strings.TrimSuffix(p.o.Issuer, "/") {
		return nil, fmt.Errorf("document issuer %q does not match the configured issuer %q", truncate(m.Issuer), p.o.Issuer)
	}
	for name, v := range map[string]string{
		"authorization_endpoint": m.AuthorizationEndpoint,
		"token_endpoint":         m.TokenEndpoint,
		"userinfo_endpoint":      m.UserinfoEndpoint,
	} {
		if v == "" {
			return nil, fmt.Errorf("document has no %s", name)
		}
		if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("%s %q is not an absolute http(s) URL", name, truncate(v))
		}
	}
	return &m, nil
}

func (p *Provider) config(m *metadata, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.o.ClientID,
		ClientSecret: p.o.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   m.AuthorizationEndpoint,
			TokenURL:  m.TokenEndpoint,
			AuthStyle: oauth2.AuthStyleInHeader,
		},
		RedirectURL: redirectURL,
		Scopes:      p.o.Scopes,
	}
}

// AuthCodeURL returns the provider's authorization URL for the authorization
// code flow with PKCE: verifier is the code verifier, of which only the S256
// challenge is sent. It fails if discovery fails.
func (p *Provider) AuthCodeURL(state, verifier, redirectURL string) (string, error) {
	if state == "" || verifier == "" {
		return "", errors.New("oidc: state and verifier are required")
	}
	m, err := p.discover(context.Background())
	if err != nil {
		return "", err
	}
	return p.config(m, redirectURL).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), nil
}

// Login exchanges the authorization code (with the PKCE verifier), reads the
// userinfo endpoint with the access token and returns who signed in. The
// tokens are discarded. Errors never contain tokens or the client secret.
func (p *Provider) Login(ctx context.Context, code, verifier, redirectURL string) (Identity, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return Identity{}, err
	}
	tok, err := p.exchange(ctx, m, code, verifier, redirectURL)
	if err != nil {
		return Identity{}, err
	}
	claims, err := p.userinfo(ctx, m, tok)
	if err != nil {
		return Identity{}, err
	}
	return p.identity(claims)
}

// exchange trades the code for an access token at the token endpoint.
func (p *Provider) exchange(ctx context.Context, m *metadata, code, verifier, redirectURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.http)
	tok, err := p.config(m, redirectURL).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			status := 0
			if re.Response != nil {
				status = re.Response.StatusCode
			}
			msg := re.ErrorCode
			if msg == "" {
				msg = "no OAuth error code"
			}
			if re.ErrorDescription != "" {
				msg += ": " + truncate(re.ErrorDescription)
			}
			return "", fmt.Errorf("oidc: token endpoint: HTTP %d: %s", status, msg)
		}
		// Not a server answer: transport or decoding problem. The oauth2
		// package never puts the request (credentials) in these errors.
		return "", fmt.Errorf("oidc: token endpoint: %w", stripURL(err))
	}
	if tok.AccessToken == "" {
		return "", errors.New("oidc: token endpoint returned no access token")
	}
	return tok.AccessToken, nil
}

// userinfo reads the claims of the access token's user.
func (p *Provider) userinfo(ctx context.Context, m *metadata, accessToken string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.UserinfoEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("oidc: userinfo: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: userinfo: %w", stripURL(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("oidc: userinfo: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body and WWW-Authenticate are left out: they are the
		// provider's text and could echo the token.
		return nil, fmt.Errorf("oidc: userinfo: HTTP %d", resp.StatusCode)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/jwt" {
		return nil, errors.New("oidc: userinfo: signed (application/jwt) responses are not supported; configure the provider to return JSON")
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, errors.New("oidc: userinfo: response is not a JSON object")
	}
	return claims, nil
}

// identity maps userinfo claims to an Identity.
func (p *Provider) identity(claims map[string]any) (Identity, error) {
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Identity{}, errors.New("oidc: userinfo: no sub claim")
	}
	id := Identity{Subject: sub, Username: sub}
	for _, k := range []string{"preferred_username", "email"} {
		if v, _ := claims[k].(string); v != "" {
			id.Username = v
			break
		}
	}
	switch v := claims[p.o.GroupsClaim].(type) {
	case []any:
		for _, g := range v {
			if s, ok := g.(string); ok {
				id.Groups = append(id.Groups, s)
			}
		}
	case string:
		id.Groups = []string{v}
	}
	for _, g := range id.Groups {
		if slices.Contains(p.o.AllowedGroups, g) {
			id.Allowed = true
			break
		}
	}
	return id, nil
}

// stripURL drops the *url.Error wrapper; its message repeats the URL, which
// the caller already names.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func truncate(s string) string {
	if len(s) > maxErrorText {
		return s[:maxErrorText] + "..."
	}
	return s
}

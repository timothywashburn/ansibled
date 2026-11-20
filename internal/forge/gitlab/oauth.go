package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/timothywashburn/ansibled/internal/forge"
)

// OAuth implements forge.Forge: "Sign in with GitLab" with the authorization
// code flow, PKCE (S256) and scope read_user.
func (c *Client) OAuth() forge.OAuthProvider { return oauthProvider{c} }

type oauthProvider struct{ c *Client }

func (p oauthProvider) config(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.c.opts.OAuthClientID,
		ClientSecret: p.c.opts.OAuthClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   p.c.baseURL + "/oauth/authorize",
			TokenURL:  p.c.baseURL + "/oauth/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		RedirectURL: redirectURL,
		Scopes:      []string{"read_user"},
	}
}

// AuthCodeURL implements forge.OAuthProvider.
func (p oauthProvider) AuthCodeURL(state, verifier, redirectURL string) string {
	return p.config(redirectURL).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

// Exchange implements forge.OAuthProvider. It trades the code for a token,
// reads GET /api/v4/user with it and drops the token.
func (p oauthProvider) Exchange(ctx context.Context, code, verifier, redirectURL string) (forge.User, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.c.http)
	tok, err := p.config(redirectURL).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			msg := re.ErrorCode
			if re.ErrorDescription != "" {
				msg += ": " + re.ErrorDescription
			}
			status := 0
			if re.Response != nil {
				status = re.Response.StatusCode
			}
			return forge.User{}, fmt.Errorf("gitlab oauth: POST /oauth/token: %d %s", status, msg)
		}
		return forge.User{}, fmt.Errorf("gitlab oauth: POST /oauth/token: %w", stripURL(err))
	}
	if tok.AccessToken == "" {
		return forge.User{}, errors.New("gitlab oauth: token endpoint returned no access token")
	}
	var u apiUser
	if _, err := p.c.do(ctx, request{method: http.MethodGet, path: "/user", bearer: tok.AccessToken}, &u); err != nil {
		return forge.User{}, fmt.Errorf("gitlab oauth: %w", err)
	}
	if u.ID <= 0 {
		return forge.User{}, errors.New("gitlab oauth: GET /user returned no user id")
	}
	return u.forge(), nil
}

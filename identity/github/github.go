// Package github is Sign in with GitHub for identity: OAuth 2.0 with
// PKCE, the user read from the REST API.
//
//	auth := identity.MustNew(cfg.Auth, d,
//		identity.Providers(github.New(cfg.Auth.Providers["github"])))
//
// It reads, from auth.providers.github: client_id and client_secret (an
// OAuth app of the GitHub settings), redirect_url (the app's callback:
// https://api.example.com/auth/github/callback), scopes (read:user and
// user:email by default) and endpoint (a GitHub Enterprise server,
// https://github.example.com; github.com unless set). It refuses
// hosted_domain, team_id and key_id.
//
// The account id is GitHub's numeric user id, stable across renames. The
// email is the one /user shows, or else the primary verified address of
// /user/emails; without either, the identity has no email and cannot
// create an account (it can still be linked to one).
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/oauth2"
	githubendpoint "golang.org/x/oauth2/github"

	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/identity/internal/oauth"
)

const publicAPI = "https://api.github.com"

type provider struct {
	cfg     *oauth2.Config
	apiBase string
}

// New returns the GitHub provider for pc. A configuration it refuses
// makes identity.New fail with the reason.
func New(pc identity.ProviderConfig) identity.Provider {
	if err := check(pc); err != nil {
		return oauth.Broken{ProviderName: "github", Err: err}
	}
	endpoint, apiBase := githubendpoint.Endpoint, publicAPI
	if base := strings.TrimRight(pc.Endpoint, "/"); base != "" {
		endpoint = oauth2.Endpoint{AuthURL: base + "/login/oauth/authorize", TokenURL: base + "/login/oauth/access_token"}
		apiBase = base + "/api/v3"
	}
	scopes := pc.Scopes
	if len(scopes) == 0 {
		scopes = []string{"read:user", "user:email"}
	}
	return &provider{
		cfg:     &oauth2.Config{ClientID: pc.ClientID, ClientSecret: pc.ClientSecret, RedirectURL: pc.RedirectURL, Scopes: scopes, Endpoint: endpoint},
		apiBase: apiBase,
	}
}

func check(pc identity.ProviderConfig) error {
	switch {
	case pc.ClientID == "":
		return errors.New("github: client_id is required")
	case pc.ClientSecret == "":
		return errors.New("github: client_secret is required")
	case pc.RedirectURL == "":
		return errors.New("github: redirect_url is required")
	}
	if err := oauth.AbsoluteURL("github: redirect_url", pc.RedirectURL); err != nil {
		return err
	}
	if pc.Endpoint != "" {
		if err := oauth.AbsoluteURL("github: endpoint", pc.Endpoint); err != nil {
			return err
		}
	}
	return oauth.Unused("github", map[string]bool{"hosted_domain": pc.HostedDomain != "", "team_id": pc.TeamID != "", "key_id": pc.KeyID != ""})
}

func (p *provider) Name() string { return "github" }

func (p *provider) Check() error { return nil }

// Capabilities: GitHub takes PKCE, has no nonce (it is not OpenID
// Connect) and calls back with GET.
func (p *provider) Capabilities() oauth.Capabilities {
	return oauth.Capabilities{CallbackMethod: http.MethodGet, SupportsPKCE: true, RedirectURL: p.cfg.RedirectURL}
}

// BeginAuth builds the authorization URL with the state and the PKCE
// challenge.
func (p *provider) BeginAuth(_ context.Context, req *oauth.BeginRequest) (string, error) {
	var opts []oauth2.AuthCodeOption
	if req.CodeChallenge != "" {
		opts = append(opts, oauth2.SetAuthURLParam("code_challenge", req.CodeChallenge), oauth2.SetAuthURLParam("code_challenge_method", "S256"))
	}
	return p.cfg.AuthCodeURL(req.State, opts...), nil
}

type user struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
	HTMLURL   string `json:"html_url"`
	Company   string `json:"company"`
	Location  string `json:"location"`
	Bio       string `json:"bio"`
}

type email struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

var apiHeader = http.Header{"Accept": {"application/vnd.github+json"}, "X-Github-Api-Version": {"2022-11-28"}}

// CompleteAuth trades the code for a token and reads the user, and the
// email addresses when the profile hides its email.
func (p *provider) CompleteAuth(ctx context.Context, req *oauth.CompleteRequest) (*oauth.Identity, error) {
	var opts []oauth2.AuthCodeOption
	if req.CodeVerifier != "" {
		opts = append(opts, oauth2.SetAuthURLParam("code_verifier", req.CodeVerifier))
	}
	tok, err := p.cfg.Exchange(ctx, req.Code, opts...)
	if err != nil {
		return nil, fmt.Errorf("github: token exchange: %w", err)
	}
	client := p.cfg.Client(ctx, tok)
	var u user
	if err := oauth.GetJSON(ctx, client, p.apiBase+"/user", apiHeader, &u); err != nil {
		return nil, fmt.Errorf("github: fetch /user: %w", err)
	}
	// /user shows an email only when the user made it public, and GitHub
	// shows only verified addresses there.
	primary, verified := u.Email, u.Email != ""
	if primary == "" {
		var emails []email
		if err := oauth.GetJSON(ctx, client, p.apiBase+"/user/emails", apiHeader, &emails); err != nil {
			return nil, fmt.Errorf("github: fetch /user/emails: %w", err)
		}
		for _, e := range emails {
			if e.Primary && e.Verified {
				primary, verified = e.Email, true
				break
			}
		}
	}
	return &oauth.Identity{
		Provider: "github", ProviderAccountID: strconv.FormatInt(u.ID, 10), Email: primary, EmailVerified: verified,
		Name: u.Name, Picture: u.AvatarURL,
		Raw: map[string]any{"login": u.Login, "html_url": u.HTMLURL, "company": u.Company, "location": u.Location, "bio": u.Bio},
	}, nil
}

// Package facebook is Facebook Login for identity: OAuth 2.0 with PKCE,
// the user read from the Graph API.
//
//	auth := identity.MustNew(cfg.Auth, d,
//		identity.Providers(facebook.New(cfg.Auth.Providers["facebook"])))
//
// It reads, from auth.providers.facebook: client_id and client_secret
// (the app's id and secret), redirect_url (a valid OAuth redirect URI of
// the app: https://api.example.com/auth/facebook/callback), scopes (email
// and public_profile by default) and endpoint (the Graph API base with
// its version, https://graph.facebook.com/v18.0 unless set — set it to
// move to a newer version). It refuses hosted_domain, team_id and key_id.
//
// Facebook returns an email only when the user granted it, and only a
// confirmed one: an email present is taken as verified.
package facebook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"

	"github.com/zynthara/chok/v3/identity"
	"github.com/zynthara/chok/v3/identity/internal/oauth"
)

const defaultGraph = "https://graph.facebook.com/v18.0"

type provider struct {
	cfg   *oauth2.Config
	graph string // the Graph API base with its version
}

// New returns the Facebook provider for pc. A configuration it refuses
// makes identity.New fail with the reason.
func New(pc identity.ProviderConfig) identity.Provider {
	if err := check(pc); err != nil {
		return oauth.Broken{ProviderName: "facebook", Err: err}
	}
	graph := strings.TrimRight(pc.Endpoint, "/")
	if graph == "" {
		graph = defaultGraph
	}
	scopes := pc.Scopes
	if len(scopes) == 0 {
		scopes = []string{"email", "public_profile"}
	}
	return &provider{
		cfg: &oauth2.Config{
			ClientID: pc.ClientID, ClientSecret: pc.ClientSecret, RedirectURL: pc.RedirectURL, Scopes: scopes,
			Endpoint: oauth2.Endpoint{AuthURL: dialogURL(graph), TokenURL: graph + "/oauth/access_token"},
		},
		graph: graph,
	}
}

// dialogURL is the login dialog of the Graph version: on www.facebook.com
// for the real Graph API, under the base itself for another server.
func dialogURL(graph string) string {
	u, err := url.Parse(graph)
	if err == nil && u.Host == "graph.facebook.com" {
		return "https://www.facebook.com" + u.Path + "/dialog/oauth"
	}
	return graph + "/dialog/oauth"
}

func check(pc identity.ProviderConfig) error {
	switch {
	case pc.ClientID == "":
		return errors.New("facebook: client_id is required")
	case pc.ClientSecret == "":
		return errors.New("facebook: client_secret is required")
	case pc.RedirectURL == "":
		return errors.New("facebook: redirect_url is required")
	}
	if err := oauth.AbsoluteURL("facebook: redirect_url", pc.RedirectURL); err != nil {
		return err
	}
	if pc.Endpoint != "" {
		if err := oauth.AbsoluteURL("facebook: endpoint", pc.Endpoint); err != nil {
			return err
		}
	}
	return oauth.Unused("facebook", map[string]bool{"hosted_domain": pc.HostedDomain != "", "team_id": pc.TeamID != "", "key_id": pc.KeyID != ""})
}

func (p *provider) Name() string { return "facebook" }

func (p *provider) Check() error { return nil }

// Capabilities: Facebook takes PKCE, has no nonce in this flow and calls
// back with GET.
func (p *provider) Capabilities() oauth.Capabilities {
	return oauth.Capabilities{CallbackMethod: http.MethodGet, SupportsPKCE: true, RedirectURL: p.cfg.RedirectURL}
}

// BeginAuth builds the login dialog URL with the state and the PKCE
// challenge.
func (p *provider) BeginAuth(_ context.Context, req *oauth.BeginRequest) (string, error) {
	var opts []oauth2.AuthCodeOption
	if req.CodeChallenge != "" {
		opts = append(opts, oauth2.SetAuthURLParam("code_challenge", req.CodeChallenge), oauth2.SetAuthURLParam("code_challenge_method", "S256"))
	}
	return p.cfg.AuthCodeURL(req.State, opts...), nil
}

type user struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	Picture struct {
		Data struct {
			URL          string `json:"url"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			IsSilhouette bool   `json:"is_silhouette"`
		} `json:"data"`
	} `json:"picture"`
}

// CompleteAuth trades the code for a token and reads /me with the fields
// it needs.
func (p *provider) CompleteAuth(ctx context.Context, req *oauth.CompleteRequest) (*oauth.Identity, error) {
	var opts []oauth2.AuthCodeOption
	if req.CodeVerifier != "" {
		opts = append(opts, oauth2.SetAuthURLParam("code_verifier", req.CodeVerifier))
	}
	tok, err := p.cfg.Exchange(ctx, req.Code, opts...)
	if err != nil {
		return nil, fmt.Errorf("facebook: token exchange: %w", err)
	}
	var u user
	me := p.graph + "/me?fields=" + url.QueryEscape("id,name,email,picture")
	if err := oauth.GetJSON(ctx, p.cfg.Client(ctx, tok), me, http.Header{"Accept": {"application/json"}}, &u); err != nil {
		return nil, fmt.Errorf("facebook: fetch /me: %w", err)
	}
	return &oauth.Identity{
		Provider: "facebook", ProviderAccountID: u.ID, Email: u.Email, EmailVerified: u.Email != "",
		Name: u.Name, Picture: u.Picture.Data.URL,
		Raw: map[string]any{"id": u.ID, "name": u.Name, "graph": p.graph, "picture_data": u.Picture.Data},
	}, nil
}

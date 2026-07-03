package facebook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"
	fbendpoint "golang.org/x/oauth2/facebook"

	"github.com/zynthara/chok/v2/account"
)

// provider is the runtime facebook.AuthProvider. Holds the oauth2
// config + the Graph API base URL and version.
type provider struct {
	cfg         *oauth2.Config
	apiBase     string // "https://graph.facebook.com"
	apiVersion  string // "v18.0"
	redirectURL string
}

// New constructs a Facebook provider. The Graph API base URL is fixed
// in production (graph.facebook.com); tests inject an alternate via
// the optional NewWithAPIBase entry point so we don't need a global
// override variable.
func New(opts Options) (account.AuthProvider, error) {
	return NewWithAPIBase(opts, publicGraphAPI)
}

// NewWithAPIBase is the test-friendly constructor: it lets a
// httptest.Server URL stand in for the real Graph API. Production
// callers stick to New(); production deployments never need to
// change the API host.
func NewWithAPIBase(opts Options, apiBase string) (account.AuthProvider, error) {
	apiVersion := opts.APIVersion
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}

	endpoint := fbendpoint.Endpoint
	// When apiBase is overridden (tests), point the auth+token
	// endpoints at the same mock so we don't need separate hosts.
	if apiBase != publicGraphAPI {
		endpoint = oauth2.Endpoint{
			AuthURL:  apiBase + "/" + apiVersion + "/dialog/oauth",
			TokenURL: apiBase + "/" + apiVersion + "/oauth/access_token",
		}
	}

	return &provider{
		cfg: &oauth2.Config{
			ClientID:     opts.ClientID,
			ClientSecret: opts.ClientSecret,
			RedirectURL:  opts.RedirectURL,
			Scopes:       defaultScopes(opts.Scopes),
			Endpoint:     endpoint,
		},
		apiBase:     apiBase,
		apiVersion:  apiVersion,
		redirectURL: opts.RedirectURL,
	}, nil
}

// Name implements account.AuthProvider.
func (p *provider) Name() string { return "facebook" }

// Capabilities implements account.AuthProvider.
//
// Facebook is plain OAuth 2.0 — no OIDC, no nonce. PKCE is supported
// (Facebook documented it for mobile; web flow accepts it too); we
// declare SupportsPKCE=true for defense-in-depth. CallbackMethod is
// "GET" — Facebook never uses form_post.
func (p *provider) Capabilities() account.ProviderCapabilities {
	return account.ProviderCapabilities{
		CallbackMethod: "GET",
		RequiresNonce:  false,
		SupportsPKCE:   true,
	}
}

// RedirectURL implements account.RedirectURLProvider.
func (p *provider) RedirectURL() string { return p.redirectURL }

// BeginAuth implements account.AuthProvider.
func (p *provider) BeginAuth(_ context.Context, req *account.BeginRequest) (*account.BeginResponse, error) {
	authOpts := []oauth2.AuthCodeOption{}
	if req.CodeChallenge != "" {
		authOpts = append(authOpts,
			oauth2.SetAuthURLParam("code_challenge", req.CodeChallenge),
			oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		)
	}
	return &account.BeginResponse{
		RedirectTo: p.cfg.AuthCodeURL(req.State, authOpts...),
	}, nil
}

// CompleteAuth implements account.AuthProvider.
//
// Facebook's profile fetch goes through the versioned Graph API:
//
//	GET https://graph.facebook.com/v18.0/me?fields=id,name,email,picture
//
// The `fields=` query is REQUIRED — Graph API only returns explicitly
// requested fields, anything missing comes back as `null`. We pin a
// minimal set so we don't accidentally pull data the deployment didn't
// authorise scopes for.
//
// Facebook does not return an `email_verified` flag. Their stance is
// that any email Facebook returns has been confirmed via the user's
// account creation flow — we therefore treat Email != "" as
// EmailVerified=true. SPEC §8 LinkByEmail's defense-in-depth still
// requires the local user to have EmailVerified=true on the chok
// side, so this trust assumption can't get us into a squatting
// scenario by itself.
func (p *provider) CompleteAuth(ctx context.Context, req *account.CompleteRequest) (*account.ProviderIdentity, error) {
	exchangeOpts := []oauth2.AuthCodeOption{}
	if req.CodeVerifier != "" {
		exchangeOpts = append(exchangeOpts,
			oauth2.SetAuthURLParam("code_verifier", req.CodeVerifier),
		)
	}
	tok, err := p.cfg.Exchange(ctx, req.Code, exchangeOpts...)
	if err != nil {
		return nil, fmt.Errorf("facebook: token exchange: %w", err)
	}
	client := p.cfg.Client(ctx, tok)

	meURL := fmt.Sprintf("%s/%s/me?fields=%s",
		p.apiBase, p.apiVersion,
		url.QueryEscape("id,name,email,picture"),
	)
	var me fbUser
	if err := getJSON(ctx, client, meURL, &me); err != nil {
		return nil, fmt.Errorf("facebook: fetch /me: %w", err)
	}

	return &account.ProviderIdentity{
		Provider:          "facebook",
		ProviderAccountID: me.ID,
		Email:             me.Email,
		// Facebook doesn't expose email_verified — see method doc
		// for the trust rationale. Empty email surfaces as
		// EmailVerified=false so the §8.1 gate handles it.
		EmailVerified:  me.Email != "",
		Name:           me.Name,
		Picture:        me.Picture.Data.URL,
		IsAliasedEmail: false,
		Raw: map[string]any{
			"id":           me.ID,
			"name":         me.Name,
			"api_version":  p.apiVersion,
			"picture_data": me.Picture.Data,
		},
	}, nil
}

// getJSON is the Graph API helper. Same shape as github.getJSON; we
// don't deduplicate across packages because each provider's accept
// headers and error-body shapes differ slightly and copy-paste is
// cheaper than a shared HTTP utility leaking provider concerns.
func getJSON(ctx context.Context, client *http.Client, urlStr string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Compile-time interface assertions.
var (
	_ account.AuthProvider        = (*provider)(nil)
	_ account.RedirectURLProvider = (*provider)(nil)
)

// fbUser is the projection of GET /me?fields=id,name,email,picture.
// Picture is nested because Graph API wraps it in {"data":{...}}.
type fbUser struct {
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

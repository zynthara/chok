package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"golang.org/x/oauth2"
	githubendpoint "golang.org/x/oauth2/github"

	"github.com/zynthara/chok/v2/account"
)

// provider is the runtime github.AuthProvider. Holds the oauth2 config
// + the REST API base URL (different for github.com vs Enterprise).
type provider struct {
	cfg         *oauth2.Config
	apiBase     string // "https://api.github.com" or enterprise "https://gh.corp/api/v3"
	redirectURL string
}

// New constructs a GitHub provider against the supplied options. Unlike
// the google provider there's no OIDC discovery — github.com endpoints
// are static, and Enterprise installs derive theirs from EnterpriseURL.
func New(opts Options) (account.AuthProvider, error) {
	endpoint := githubendpoint.Endpoint
	apiBase := publicGitHubAPI
	if opts.EnterpriseURL != "" {
		endpoint = oauth2.Endpoint{
			AuthURL:  opts.EnterpriseURL + "/login/oauth/authorize",
			TokenURL: opts.EnterpriseURL + "/login/oauth/access_token",
		}
		apiBase = opts.EnterpriseURL + "/api/v3"
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
		redirectURL: opts.RedirectURL,
	}, nil
}

// Name implements account.AuthProvider.
func (p *provider) Name() string { return "github" }

// Capabilities implements account.AuthProvider.
//
// GitHub is plain OAuth 2.0 — no OIDC, no nonce. PKCE is supported as
// of GitHub Enterprise Server 3.10 and github.com (rolled out 2023);
// we declare SupportsPKCE=true so Module's defense-in-depth kicks in.
// CallbackMethod is "GET" (no form_post).
func (p *provider) Capabilities() account.ProviderCapabilities {
	return account.ProviderCapabilities{
		CallbackMethod: "GET",
		RequiresNonce:  false,
		SupportsPKCE:   true,
	}
}

// RedirectURL implements account.RedirectURLProvider so Module's
// dev-mode auto-detect can sniff HTTP-on-localhost from the live
// configuration.
func (p *provider) RedirectURL() string { return p.redirectURL }

// BeginAuth implements account.AuthProvider. GitHub honours the state
// param and (since 2023) the PKCE code_challenge; we forward both.
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

// CompleteAuth implements account.AuthProvider. Three-step:
//  1. exchange the code for an access token
//  2. GET /user to read the public profile + maybe-email
//  3. if email is empty (user has hidden the primary in GitHub
//     privacy settings), GET /user/emails and pick the primary +
//     verified entry
//
// GitHub's user ID (numeric `id` field) is the stable join key —
// users can rename their `login` (username) at any time, but `id`
// is permanent. We base ProviderAccountID on the numeric ID.
func (p *provider) CompleteAuth(ctx context.Context, req *account.CompleteRequest) (*account.ProviderIdentity, error) {
	exchangeOpts := []oauth2.AuthCodeOption{}
	if req.CodeVerifier != "" {
		exchangeOpts = append(exchangeOpts,
			oauth2.SetAuthURLParam("code_verifier", req.CodeVerifier),
		)
	}
	tok, err := p.cfg.Exchange(ctx, req.Code, exchangeOpts...)
	if err != nil {
		return nil, fmt.Errorf("github: token exchange: %w", err)
	}
	client := p.cfg.Client(ctx, tok)

	var u githubUser
	if err := getJSON(ctx, client, p.apiBase+"/user", &u); err != nil {
		return nil, fmt.Errorf("github: fetch /user: %w", err)
	}

	primary := u.Email
	verified := primary != "" // /user only returns the primary if it's verified

	if primary == "" {
		// Email is hidden in user privacy settings; walk
		// /user/emails for the primary+verified entry. Requires
		// the `user:email` scope, which defaultScopes() includes.
		var emails []githubEmail
		if err := getJSON(ctx, client, p.apiBase+"/user/emails", &emails); err != nil {
			return nil, fmt.Errorf("github: fetch /user/emails: %w", err)
		}
		for _, e := range emails {
			if e.Primary && e.Verified {
				primary = e.Email
				verified = true
				break
			}
		}
		// If no primary+verified entry exists, fall through with
		// primary="" and verified=false. account.ResolveOAuthIdentity's
		// SPEC §8.1 gate (Email != "" && EmailVerified) will reject
		// the create-new-User path with OAUTH_EMAIL_REQUIRED — that's
		// the expected outcome for a GitHub user who has no
		// publicly-bindable verified email.
	}

	return &account.ProviderIdentity{
		Provider: "github",
		// Numeric ID, stringified — login (username) is mutable, id
		// is permanent. SPEC §9 explicitly mandates id over login.
		ProviderAccountID: strconv.FormatInt(u.ID, 10),
		Email:             primary,
		EmailVerified:     verified,
		Name:              u.Name,
		Picture:           u.AvatarURL,
		// IsAliasedEmail is always false — GitHub doesn't issue
		// relay/alias addresses (that's Apple). A no-reply email
		// like "1234567+username@users.noreply.github.com" is what
		// /user returns when the user opted into "keep my email
		// addresses private", and we treat that the same as any
		// other email — it's stable, addressable, and the user owns
		// it for the duration of their account. (Apple SPEC §4.4
		// applies to Apple-only.)
		IsAliasedEmail: false,
		Raw: map[string]any{
			"login":    u.Login,
			"html_url": u.HTMLURL,
			"company":  u.Company,
			"location": u.Location,
			"bio":      u.Bio,
		},
	}, nil
}

// getJSON is the small REST helper for hitting GitHub's authenticated
// API. Builds context-aware requests, treats >=400 as a structured
// error so callers can surface API-side messages in their wrapped
// errors.
func getJSON(ctx context.Context, client *http.Client, urlStr string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

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

// githubUser is the projection of GET /user we care about. GitHub
// returns many more fields; ignoring them keeps the binding tight.
type githubUser struct {
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

// githubEmail is one row of GET /user/emails.
type githubEmail struct {
	Email      string `json:"email"`
	Primary    bool   `json:"primary"`
	Verified   bool   `json:"verified"`
	Visibility string `json:"visibility,omitempty"`
}

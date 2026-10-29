// Package oauth holds what identity and its provider packages share: the
// shape of a sign-in as identity drives it, and the helpers every
// provider uses. Only packages under identity/ can import it, which keeps
// the provider interface identity's own.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
)

// Capabilities is how a provider's sign-in runs.
type Capabilities struct {
	// CallbackMethod is GET, or POST for a provider that posts the
	// callback as a form (Apple).
	CallbackMethod string
	// RequiresNonce makes the flow send an OpenID Connect nonce, which the
	// provider checks in the ID token.
	RequiresNonce bool
	// SupportsPKCE makes the flow send a PKCE challenge (S256).
	SupportsPKCE bool
	// RequiresFormPost reads code and state from the posted form.
	RequiresFormPost bool
	// RedirectURL is the provider's registered callback; an http URL on
	// localhost puts the flow's cookies in development mode.
	RedirectURL string
	// AssumesEmail marks a provider whose addresses are only implied
	// verified (Identity.EmailVerifiedAssumed): with a Sender, identity
	// mounts the routes where a signed-in user confirms the address.
	AssumesEmail bool
}

// BeginRequest is what the flow gives a provider to build its
// authorization URL.
type BeginRequest struct {
	State         string
	Nonce         string // with RequiresNonce
	CodeChallenge string // with SupportsPKCE: the S256 challenge
	RedirectBack  string
}

// CompleteRequest is what the flow gives a provider at the callback,
// after it checked the state.
type CompleteRequest struct {
	Code         string
	State        string
	Nonce        string
	CodeVerifier string
	// FormBody is the posted form, with RequiresFormPost.
	FormBody url.Values
}

// Identity is the user as a provider knows them.
type Identity struct {
	Provider          string
	ProviderAccountID string
	Email             string
	// EmailVerified is the provider vouching that the user controls Email.
	EmailVerified bool
	// EmailVerifiedAssumed marks an EmailVerified the provider implies
	// but does not state: Facebook returns an address without saying it
	// is confirmed. Such an address creates an account, which holds it as
	// assumed (User.EmailAssumed), but joins none (link_by_email) and
	// creates none for an address auth.admins lists.
	EmailVerifiedAssumed bool
	// IsAliasedEmail marks a relay address (Apple's "hide my email"): it
	// never links to an existing account, verified or not.
	IsAliasedEmail bool
	Name           string
	Picture        string
	// Raw is the provider's payload, kept as the identity's profile.
	Raw map[string]any
}

// Broken is the provider New returns for a configuration it refuses:
// Check reports err, so identity.New fails with it; the flow methods
// return it too.
type Broken struct {
	ProviderName string
	Err          error
}

func (b Broken) Name() string               { return b.ProviderName }
func (b Broken) Check() error               { return b.Err }
func (b Broken) Capabilities() Capabilities { return Capabilities{CallbackMethod: http.MethodGet} }
func (b Broken) BeginAuth(context.Context, *BeginRequest) (string, error) {
	return "", b.Err
}
func (b Broken) CompleteAuth(context.Context, *CompleteRequest) (*Identity, error) {
	return nil, b.Err
}

// AbsoluteURL checks that raw is an absolute http(s) URL.
func AbsoluteURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s must be an absolute http(s) URL, got %q", field, raw)
	}
	return nil
}

// Unused refuses the fields a provider does not read: name each one set
// with its yaml key, and Unused reports the first.
func Unused(provider string, set map[string]bool) error {
	for _, key := range []string{"client_secret", "scopes", "endpoint", "hosted_domain", "team_id", "key_id"} {
		if set[key] {
			return fmt.Errorf("%s: %s is not one of its settings", provider, key)
		}
	}
	return nil
}

// maxBody caps what a provider reads from an API response.
const maxBody = 1 << 20

// GetJSON GETs url with client and decodes the JSON response into out;
// a 4xx or 5xx status is an error quoting the start of the body.
func GetJSON(ctx context.Context, client *http.Client, rawURL string, header http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	maps.Copy(req.Header, header)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return errors.Join(errors.New("decode the response"), err)
	}
	return nil
}

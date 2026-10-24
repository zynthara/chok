package identity

import (
	"context"

	"github.com/zynthara/chok/v3/identity/internal/oauth"
)

// Provider is an OAuth identity provider: what identity/google,
// identity/github, identity/apple and identity/facebook return from their
// New. Pass it to New with Providers; identity drives the sign-in through
// these methods, whose types belong to identity, so the four packages are
// the providers there are.
type Provider interface {
	// Name is the provider's key in auth.providers and its URL segment:
	// {prefix}/{name}/start.
	Name() string
	// Check reports a configuration the provider refuses; New fails with it.
	Check() error
	// Capabilities is how the provider's sign-in runs.
	Capabilities() oauth.Capabilities
	// BeginAuth returns the provider's authorization URL for one sign-in.
	BeginAuth(ctx context.Context, req *oauth.BeginRequest) (string, error)
	// CompleteAuth trades the callback's code for the user's identity.
	CompleteAuth(ctx context.Context, req *oauth.CompleteRequest) (*oauth.Identity, error)
}

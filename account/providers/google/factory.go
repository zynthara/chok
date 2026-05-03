package google

import (
	"context"
	"fmt"

	"github.com/zynthara/chok/account"
)

// rawDecoder is the subset of config.ProviderRawOptions Factory needs.
// Defining it as a local interface avoids importing the config
// package directly (which would still work — config doesn't import
// account/providers — but keeps this leaf package's dep graph as
// narrow as possible).
type rawDecoder interface {
	Decode(out any) error
}

// Factory is the account.ProviderFactory implementation registered
// for "google". chok's parts.DefaultAccountBuilder /
// account.RegisterConfiguredProviders look it up via
// account.LookupProviderFactory("google") and invoke it with the
// per-provider yaml block.
//
// Decode → Validate → New flow keeps each layer responsible for
// exactly one thing; errors at any step propagate upward with enough
// context (provider name, which field) for an operator to diagnose
// from chok's startup logs.
//
// We pass context.Background to oidc.NewProvider — discovery is a
// short-lived HTTP roundtrip on first ID Token verify (lazy), and
// chok's startup ctx isn't worth threading through the factory
// signature. If discovery later moves to eager-at-construction we'll
// reconsider.
func Factory(rawCfg any) (account.AuthProvider, error) {
	r, ok := rawCfg.(rawDecoder)
	if !ok {
		return nil, fmt.Errorf("google.Factory: expected ProviderRawOptions-like, got %T", rawCfg)
	}
	var opts Options
	if err := r.Decode(&opts); err != nil {
		return nil, fmt.Errorf("google.Factory: decode: %w", err)
	}
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("google.Factory: %w", err)
	}
	return New(context.Background(), opts)
}

// init registers the factory at process start. Because chok's main
// package transitively imports this package via
// account/providers/blessed, every chok-based binary has the "google"
// factory in account.providerRegistry from the moment it boots.
//
// Register-once invariant: account.RegisterProviderFactory panics on
// duplicate names. Imports of this package must not race with each
// other (they don't — Go init() runs single-threaded), and forks of
// chok that swap the curator must avoid double-registering by either
// using the existing init() or removing this file.
func init() {
	account.RegisterProviderFactory("google", Factory)
}

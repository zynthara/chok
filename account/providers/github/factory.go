package github

import (
	"fmt"

	"github.com/zynthara/chok/account"
)

// rawDecoder is the subset of config.ProviderRawOptions Factory needs.
type rawDecoder interface {
	Decode(out any) error
}

// Factory is the account.ProviderFactory implementation registered
// for "github". chok's parts.DefaultAccountBuilder /
// account.RegisterConfiguredProviders look it up via
// account.LookupProviderFactory("github") and invoke it with the
// per-provider yaml block.
func Factory(rawCfg any) (account.AuthProvider, error) {
	r, ok := rawCfg.(rawDecoder)
	if !ok {
		return nil, fmt.Errorf("github.Factory: expected ProviderRawOptions-like, got %T", rawCfg)
	}
	var opts Options
	if err := r.Decode(&opts); err != nil {
		return nil, fmt.Errorf("github.Factory: decode: %w", err)
	}
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("github.Factory: %w", err)
	}
	return New(opts)
}

// init registers the factory at process start. Because chok's main
// package transitively imports this package via
// account/providers/blessed, every chok-based binary has the "github"
// factory in account.providerRegistry from the moment it boots.
func init() {
	account.RegisterProviderFactory("github", Factory)
}

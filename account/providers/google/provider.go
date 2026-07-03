package google

import (
	"context"
	"fmt"

	"github.com/zynthara/chok/v2/account"
)

// Provider returns the assembly spec for the google IdP. Assemble it
// explicitly:
//
//	account.Module(account.WithProviders(google.Provider()))
//
// yaml stays the runtime switch: the spec only builds when
// `account.providers.google.enabled` is true, decoding the rest of
// that block into Options (Decode → Validate → New — each layer owns
// exactly one thing, and errors carry enough context to diagnose from
// startup logs). Explicit assembly replaces the v1 init()-time factory
// registry + blessed blank-import curator, so the linker carries only
// the providers a binary imports.
func Provider() account.ProviderSpec {
	return account.ProviderSpec{
		Name: "google",
		Build: func(ctx context.Context, raw map[string]any) (account.AuthProvider, error) {
			var opts Options
			if err := account.DecodeProviderConfig(raw, &opts); err != nil {
				return nil, fmt.Errorf("google: decode: %w", err)
			}
			if err := opts.Validate(); err != nil {
				return nil, fmt.Errorf("google: %w", err)
			}
			return New(ctx, opts)
		},
	}
}

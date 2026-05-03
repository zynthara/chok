package apple

import (
	"context"
	"fmt"

	"github.com/zynthara/chok/account"
)

// rawDecoder is the subset of config.ProviderRawOptions Factory needs.
type rawDecoder interface {
	Decode(out any) error
}

// Factory is the account.ProviderFactory implementation registered
// for "apple". context.Background is used for OIDC discovery — see
// google.Factory for the same rationale.
func Factory(rawCfg any) (account.AuthProvider, error) {
	r, ok := rawCfg.(rawDecoder)
	if !ok {
		return nil, fmt.Errorf("apple.Factory: expected ProviderRawOptions-like, got %T", rawCfg)
	}
	var opts Options
	if err := r.Decode(&opts); err != nil {
		return nil, fmt.Errorf("apple.Factory: decode: %w", err)
	}
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("apple.Factory: %w", err)
	}
	return New(context.Background(), opts)
}

// init registers the factory at process start.
func init() {
	account.RegisterProviderFactory("apple", Factory)
}

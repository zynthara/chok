package facebook

import (
	"fmt"

	"github.com/zynthara/chok/account"
)

// rawDecoder is the subset of config.ProviderRawOptions Factory needs.
type rawDecoder interface {
	Decode(out any) error
}

// Factory is the account.ProviderFactory implementation registered
// for "facebook".
func Factory(rawCfg any) (account.AuthProvider, error) {
	r, ok := rawCfg.(rawDecoder)
	if !ok {
		return nil, fmt.Errorf("facebook.Factory: expected ProviderRawOptions-like, got %T", rawCfg)
	}
	var opts Options
	if err := r.Decode(&opts); err != nil {
		return nil, fmt.Errorf("facebook.Factory: decode: %w", err)
	}
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("facebook.Factory: %w", err)
	}
	return New(opts)
}

// init registers the factory at process start.
func init() {
	account.RegisterProviderFactory("facebook", Factory)
}

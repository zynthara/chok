package account

import (
	"fmt"
	"sync"
)

// ProviderFactory builds an AuthProvider from a raw config blob.
//
// Used by the package-level providerRegistry so each provider package
// (account/providers/google, account/providers/github, …) self-registers
// in init() without the chok framework needing to import every provider
// package. Phase 3's DefaultAccountBuilder iterates the configured
// providers map and looks up the factory by name.
//
// The rawCfg parameter is intentionally typed as `any` rather than a
// concrete config struct: the framework does not know each provider's
// option shape. The provider package decodes it into its own typed
// Options inside the factory body.
type ProviderFactory func(rawCfg any) (AuthProvider, error)

var (
	providerRegistryMu sync.RWMutex
	providerRegistry   = map[string]ProviderFactory{}
)

// RegisterProviderFactory wires a provider's name to its constructor.
// Intended to be called from a provider package's init():
//
//	func init() {
//	    account.RegisterProviderFactory("google", func(raw any) (account.AuthProvider, error) {
//	        var opts Options
//	        if err := config.Decode(raw, &opts); err != nil { return nil, err }
//	        return New(opts)
//	    })
//	}
//
// Calling twice with the same name panics — provider names are global
// identifiers and a silent overwrite would mask import-order bugs.
func RegisterProviderFactory(name string, factory ProviderFactory) {
	if name == "" {
		panic("account: RegisterProviderFactory called with empty name")
	}
	if factory == nil {
		panic("account: RegisterProviderFactory called with nil factory")
	}
	providerRegistryMu.Lock()
	defer providerRegistryMu.Unlock()
	if _, exists := providerRegistry[name]; exists {
		panic(fmt.Sprintf("account: provider %q already registered", name))
	}
	providerRegistry[name] = factory
}

// LookupProviderFactory retrieves a previously registered factory.
// Returns (nil, false) when the name is unknown — Phase 3 builder
// surfaces that as a fail-fast startup error so a typo in
// chok.yaml.account.providers doesn't silently disable an IdP.
func LookupProviderFactory(name string) (ProviderFactory, bool) {
	providerRegistryMu.RLock()
	defer providerRegistryMu.RUnlock()
	f, ok := providerRegistry[name]
	return f, ok
}

// resetProviderRegistry is the in-package test hook used by registry
// tests. It is not exported because production code never needs to
// rewind the registry — provider packages register exactly once per
// process via init().
func resetProviderRegistry() {
	providerRegistryMu.Lock()
	defer providerRegistryMu.Unlock()
	providerRegistry = map[string]ProviderFactory{}
}

// ResetProviderRegistryForTest clears the global provider factory
// registry. Intended for tests that need a clean slate before
// registering a fixture factory; production code never calls this.
//
// Pair with t.Cleanup so a test failure mid-flow doesn't leak a
// registered factory into the next test:
//
//	t.Cleanup(account.ResetProviderRegistryForTest)
//	account.RegisterProviderFactory("fake", testfake.Factory)
func ResetProviderRegistryForTest() {
	resetProviderRegistry()
}

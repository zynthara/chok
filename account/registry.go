package account

import "context"

// ProviderSpec is one assembled OAuth provider: a name plus the build
// function that turns the provider's yaml block
// (`account.providers.<name>.*`, minus the enabled switch) into a live
// AuthProvider. Each blessed provider package exports
//
//	func Provider() account.ProviderSpec
//
// and applications assemble them explicitly:
//
//	account.Module(account.WithProviders(google.Provider(), apple.Provider()))
//
// Explicit assembly replaces v1's global factory registry +
// blank-import curator: the linker only carries providers the binary
// imports, and yaml keeps its role as the runtime switch — an enabled
// yaml provider with no assembled spec is a fail-fast startup error,
// an assembled spec with no yaml entry (or enabled: false) is skipped.
type ProviderSpec struct {
	// Name is the provider's yaml key and URL path segment
	// ("google" → account.providers.google, /auth/google/start).
	Name string

	// Build decodes raw (the provider's yaml block) into the
	// provider's Options and constructs it. ctx serves providers that
	// perform discovery at construction (OIDC issuer metadata).
	Build func(ctx context.Context, raw map[string]any) (AuthProvider, error)
}

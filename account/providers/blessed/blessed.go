// Package blessed bundles every chok-blessed OAuth provider via blank
// imports. The chok core package depends on this package implicitly, so
// any binary built on chok inherits the full provider set without
// per-application blank import lines.
//
// Operators control which providers are *registered at runtime* via
// chok.yaml — listing a provider under `account.providers` with
// `enabled: true` activates it; absent or `enabled: false` entries
// stay dormant. The factories themselves (registered via
// account.RegisterProviderFactory in each provider package's init())
// always live in the binary; runtime cost is one map entry per
// provider.
//
// Trade-off: chok's "全家桶" positioning (Rails-for-Go ergonomics)
// trumps binary-size minimalism here. Empirical measurement of every
// blessed provider together costs ~1.8 MB stripped on top of a
// chok-based binary (see commit message for measurement detail). For
// the rare deployment that needs a leaner footprint, the escape hatch
// is to fork chok and replace the providers.go import — chok's
// account.RegisterProviderFactory remains the public extension point
// so a custom curator package works the same way as this one.
//
// Phase 4 lands `google`. Phase 5 lands `github` / `facebook` /
// `apple`. Each provider package self-registers in init(), so adding
// a blank import line below is the only step required to extend the
// blessed set.
package blessed

// Each provider package's init() registers its factory against
// account.providerRegistry, so the moment chok core blank-imports
// this package every blessed factory is reachable.
//
// Phase 4: google.
// Phase 5 will add github / facebook / apple.

import (
	_ "github.com/zynthara/chok/account/providers/google"
)

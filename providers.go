package chok

// Pull every chok-blessed OAuth provider into the dependency graph so
// applications inherit them automatically. The blessed curator package
// expands as Phase 4-5 land each provider; this single import line is
// the only chok-side touch point.
//
// Why bundle by default rather than make operators opt-in:
//   - chok's design positioning is "Rails for Go" / 全家桶 — feature
//     surface arrives at runtime via yaml config, not via per-feature
//     Go imports.
//   - Empirical binary-size cost measured on darwin/arm64 stripped
//     (`-ldflags="-s -w"`) builds: +0.1 MB for google+github+facebook
//     (they share golang.org/x/oauth2), +1.8 MB once Apple ships (jwx
//     library for ES256 client_secret + JWK rotation). For a typical
//     chok app already in the 35-40 MB range, this is < 4 % growth.
//   - Operators who want a leaner footprint fork chok and remove the
//     blessed import; account.RegisterProviderFactory remains the
//     public extension point so a custom curator works identically.
//
// Runtime cost when no providers are configured: one map entry per
// blessed provider in account's global factory registry. RegisterRoutes
// only mounts /auth/{name}/* for providers actually registered via
// m.RegisterProvider, which the parts.DefaultAccountBuilder /
// account.Setup paths gate on `enabled=true` in yaml.
import _ "github.com/zynthara/chok/account/providers/blessed"

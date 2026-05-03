// Package casbin is chok's blessed Casbin implementation of
// authz.Authorizer + authz.DomainAuthorizer.
//
// The package produces:
//   - a parts.AuthzBuilder (via Builder(opts))
//   - a Service interface for runtime policy management
//     (GrantRole / RevokeRole / AddUserToRoleInDomain / ...)
//   - a Bootstrap helper for idempotent admin seeding
//
// Wire it via parts.NewAuthzComponent + the Builder, or just enable
// it through chok.yaml's `authz.enabled: true` and let
// autoregister.autoRegisterAuthz attach the component for you.
//
// Casbin model: RBAC-with-domains (see model.go), with chok-specific
// matcher additions for global-policy passthrough (`p.dom == "*"`)
// and direct user grants (`r.sub == p.sub`).
package casbin

// Options configures the Casbin Authorizer the Builder produces.
// All fields default to disabled / empty; the only one a deployment
// often touches is RedisWatcher (multi-instance policy sync).
type Options struct {
	// Model overrides the embedded RBAC-with-domains Casbin model.
	// Empty (the default) uses rbacWithDomainsModel.
	//
	// Override only when you need a different policy shape — e.g. a
	// tenant-aware ABAC matcher that consults more attributes than
	// (sub, dom, obj, act). Custom models break compatibility with
	// the bundled Service implementation, since GrantRole et al.
	// hard-code the four-tuple.
	Model string

	// RedisWatcher enables the chok-shipped Casbin Watcher backed by
	// the existing parts.RedisComponent: when policies change on
	// instance A (Service writes), instance B's enforcer reloads
	// them sub-second. Required for multi-instance deployments —
	// single-instance can leave it false.
	//
	// When true, the Builder pulls the existing chok RedisComponent
	// from the Kernel; a missing or unconfigured redis component is
	// a fatal Build error so the operator notices the misconfig at
	// startup rather than silently degrading to single-pod scope.
	//
	// The watcher only implements persist.Watcher (not WatcherEx /
	// UpdatableWatcher), so peers full-reload on every event rather
	// than apply incremental deltas. Policy mutation rates are low
	// in typical RBAC use; full LoadPolicy is simpler to reason
	// about and easier to keep race-free with shutdown.
	RedisWatcher bool

	// RedisWatcherChannel is the Redis pub/sub channel name used by
	// Watcher peers to notify each other of policy changes. Empty
	// uses the default "chok:authz:policy" — only override when two
	// chok deployments share a single Redis and need to keep their
	// authz traffic separate.
	RedisWatcherChannel string

	// AuditEnabled toggles per-policy-change audit hooks. When true,
	// the Builder pulls a parts.AuditComponent and wires the
	// authorizer's policy-change callback to log entries (subject,
	// resource, action, decision metadata).
	//
	// Audit is stubbed in this initial Phase 6 ship — the Service
	// methods accept but don't yet emit events. A follow-up will
	// fill in the audit layer once parts.AuditComponent ships.
	AuditEnabled bool
}

// validate is internal — Options has no required fields beyond the
// runtime invariants checked at Build time (e.g. RedisWatcher=true
// without a redis component). Defaults applied at Build.
func (o Options) defaultedChannel() string {
	if o.RedisWatcherChannel == "" {
		return "chok:authz:policy"
	}
	return o.RedisWatcherChannel
}

func (o Options) modelOrDefault() string {
	if o.Model == "" {
		return rbacWithDomainsModel
	}
	return o.Model
}

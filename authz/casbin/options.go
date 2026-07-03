// Package casbin is chok's blessed Casbin implementation of the authz
// interfaces (structurally — this package deliberately does not import
// chok/v2/authz, see Engine).
//
// The package produces:
//   - the Engine (NewEngine): enforcer + chok-shipped GORM adapter,
//     with attach points for the Redis Watcher and the audit hook
//   - a Service interface for runtime policy management
//     (GrantRole / RevokeRole / AddUserToRoleInDomain / ...)
//   - a Bootstrap helper for idempotent admin seeding
//
// Wire it via authz.Module() — the module owns config decoding,
// casbin_rule schema creation (Migrate phase), watcher/audit wiring
// and bootstrap seeding. Direct NewEngine use is for kernel-less
// embedding and tests.
//
// Casbin model: RBAC-with-domains (see model.go), with chok-specific
// matcher additions for global-policy passthrough (`p.dom == "*"`)
// and direct user grants (`r.sub == p.sub`).
package casbin

// Options is the "authz.casbin" yaml subsection (nested inside
// authz.Options). All fields default to disabled / empty; the only one
// a deployment often touches is RedisWatcher (multi-instance policy
// sync). Every field is restart-only — policy hot-sync is the
// Watcher's job, not config reload's.
type Options struct {
	// Model overrides the embedded RBAC-with-domains Casbin model.
	// Empty (the default) uses rbacWithDomainsModel.
	//
	// Override only when you need a different policy shape — e.g. a
	// tenant-aware ABAC matcher that consults more attributes than
	// (sub, dom, obj, act). Custom models break compatibility with
	// the bundled Service implementation, since GrantRole et al.
	// hard-code the four-tuple.
	Model string `mapstructure:"model"`

	// RedisWatcher enables the chok-shipped Casbin Watcher backed by
	// the redis module's shared client: when policies change on
	// instance A (Service writes), instance B's enforcer reloads
	// them sub-second. Required for multi-instance deployments —
	// single-instance can leave it false.
	//
	// When true, the authz module pulls the client from the assembled
	// redis module; a missing or disabled redis module is a fatal
	// startup error so the operator notices the misconfig at startup
	// rather than silently degrading to single-pod scope.
	//
	// The watcher only implements persist.Watcher (not WatcherEx /
	// UpdatableWatcher), so peers full-reload on every event rather
	// than apply incremental deltas. Policy mutation rates are low
	// in typical RBAC use; full LoadPolicy is simpler to reason
	// about and easier to keep race-free with shutdown.
	RedisWatcher bool `mapstructure:"redis_watcher"`

	// RedisWatcherChannel is the Redis pub/sub channel name used by
	// Watcher peers to notify each other of policy changes. Empty
	// uses the default "chok:authz:policy" — only override when two
	// chok deployments share a single Redis and need to keep their
	// authz traffic separate.
	RedisWatcherChannel string `mapstructure:"redis_watcher_channel" default:"chok:authz:policy"`

	// AuditEnabled turns policy-mutation auditing on. true upgrades
	// the audit module to a hard prerequisite: not assembled, disabled
	// by config, failed Init or an unavailable sink each fail authz
	// startup (fail-fast — "must audit policy mutations" never
	// silently degrades to a no-op; SPEC §6 truth table).
	AuditEnabled bool `mapstructure:"audit_enabled"`

	// BootstrapAdminUserID, when non-empty, idempotently seeds that
	// user with the admin role (full-wildcard permission) at startup
	// — the day-one escape hatch for "who grants the first grant".
	// Sensitive: an identity-shaped value, masked on diagnostic dumps.
	BootstrapAdminUserID string `mapstructure:"bootstrap_admin_user_id" sensitive:"true"`
}

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

// ModelOrDefault resolves the effective Casbin model text (the
// embedded RBAC-with-domains model when Model is empty). Exported for
// the authz module, which drives NewEngine from decoded config.
func (o Options) ModelOrDefault() string { return o.modelOrDefault() }

// DefaultedChannel resolves the effective watcher channel name.
// Exported for the authz module.
func (o Options) DefaultedChannel() string { return o.defaultedChannel() }

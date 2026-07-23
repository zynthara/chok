// Package blessed is the single inventory of chok's built-in modules
// and account providers — the one table the CLI generators (chok sync,
// chok docs gen, the JSON-Schema emitter) consume. Axiom 5 ("one
// source of truth") in package form: descriptions, codegen
// expressions and schema hints live here; Descriptor and Options
// stay authoritative for everything they already declare.
//
// The package is internal on purpose: it imports every battery, which
// is exactly what user binaries must not be forced to do. Only the
// chok CLI links it.
package blessed

import (
	"github.com/zynthara/chok/v2/account"
	"github.com/zynthara/chok/v2/account/providers/apple"
	"github.com/zynthara/chok/v2/account/providers/facebook"
	"github.com/zynthara/chok/v2/account/providers/github"
	"github.com/zynthara/chok/v2/account/providers/google"
	"github.com/zynthara/chok/v2/audit"
	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/cache"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/debug"
	"github.com/zynthara/chok/v2/health"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/metrics"
	"github.com/zynthara/chok/v2/outbox"
	"github.com/zynthara/chok/v2/redis"
	"github.com/zynthara/chok/v2/scheduler"
	"github.com/zynthara/chok/v2/swagger"
	"github.com/zynthara/chok/v2/tracing"
	"github.com/zynthara/chok/v2/web"
)

// Module is one built-in module as the generators see it.
type Module struct {
	// ImportPath and Pkg drive code generation (chok sync).
	ImportPath string
	Pkg        string
	// Constructor is the assembly expression sync emits.
	Constructor string
	// New returns a fresh, un-inited instance; generators read its
	// Describe() for kind / config key / needs and reflect over
	// Descriptor.Options for the config reference and JSON Schema.
	New func() kernel.Component
	// DescEN / DescZH are the one-line component-table descriptions
	// (English README / Chinese README and design doc).
	DescEN string
	DescZH string
	// Enums maps a section-relative field path (dot-joined
	// mapstructure keys) to its closed value set. Only contracts the
	// SPEC freezes belong here — everything else stays structural so
	// the schema cannot drift from code.
	Enums map[string][]string
	// MultiInstance marks modules whose yaml section accepts the
	// framework-owned instances.<name> subtree (db.As today).
	MultiInstance bool
}

// Provider is one blessed OAuth provider (account.WithProviders arg).
type Provider struct {
	Name       string // yaml key under account.providers.*
	ImportPath string
	Pkg        string
	Spec       func() account.ProviderSpec
}

const root = "github.com/zynthara/chok/v2/"

// Modules returns the inventory in canonical assembly order (the
// order sync emits and the docs tables render): observability shell
// first, then data, then batteries — the m4-fixture order.
func Modules() []Module {
	return []Module{
		{
			ImportPath: root + "log", Pkg: "log", Constructor: "log.Module()",
			New:    log.Module,
			DescEN: "Root logger section (level/format/outputs); hot level reload.",
			DescZH: "根日志段（级别/格式/输出）；级别热更新。",
			Enums: map[string][]string{
				"level":  {"debug", "info", "warn", "error"},
				"format": {"json", "text"},
			},
		},
		{
			ImportPath: root + "web", Pkg: "web", Constructor: "web.Module()",
			New:    func() kernel.Component { return web.Module() },
			DescEN: "stdlib HTTP server + router + default middleware stack.",
			DescZH: "stdlib HTTP 服务器 + 路由 + 默认中间件栈。",
		},
		{
			ImportPath: root + "health", Pkg: "health", Constructor: "health.Module()",
			New:    health.Module,
			DescEN: "/healthz /livez /readyz probes; drains readiness on shutdown.",
			DescZH: "/healthz /livez /readyz 探针；停机时先摘除就绪。",
		},
		{
			ImportPath: root + "metrics", Pkg: "metrics", Constructor: "metrics.Module()",
			New:    metrics.Module,
			DescEN: "Prometheus registry + /metrics endpoint.",
			DescZH: "Prometheus 注册表 + /metrics 端点。",
		},
		{
			ImportPath: root + "debug", Pkg: "debug", Constructor: "debug.Module()",
			New:    debug.Module,
			DescEN: "/componentz topology and lifecycle-event dump (off by default).",
			DescZH: "/componentz 拓扑与生命周期事件视图（默认关闭）。",
		},
		{
			ImportPath: root + "swagger", Pkg: "swagger", Constructor: "swagger.Module()",
			New:    swagger.Module,
			DescEN: "OpenAPI 3 spec generated from the route table + Swagger UI.",
			DescZH: "由路由表生成 OpenAPI 3 spec + Swagger UI。",
		},
		{
			ImportPath: root + "tracing", Pkg: "tracing", Constructor: "tracing.Module()",
			New:    tracing.Module,
			DescEN: "OpenTelemetry tracer provider (stdout/OTLP exporters).",
			DescZH: "OpenTelemetry tracer provider（stdout/OTLP 导出）。",
			Enums:  map[string][]string{"exporter": {"stdout", "otlp"}},
		},
		{
			ImportPath: root + "db", Pkg: "db", Constructor: "db.Module()",
			New:    func() kernel.Component { return db.Module() },
			DescEN: "Database pool (sqlite/mysql/postgres) + migrations (auto/versioned/off); sqlite runs the pure-Go split-pool production shape.",
			DescZH: "数据库连接池（sqlite/mysql/postgres）+ 迁移（auto/versioned/off）。sqlite 为纯 Go 驱动 + 读写分池 + 内建维护循环（§7.5）。",
			Enums: map[string][]string{
				"driver":  {"sqlite", "mysql", "postgres"},
				"migrate": {"auto", "versioned", "off"},
			},
			MultiInstance: true,
		},
		{
			ImportPath: root + "redis", Pkg: "redis", Constructor: "redis.Module()",
			New:    redis.Module,
			DescEN: "go-redis client with TLS/CA support; health probe.",
			DescZH: "go-redis 客户端（TLS/CA 支持）；健康探针。",
		},
		{
			ImportPath: root + "cache", Pkg: "cache", Constructor: "cache.Module()",
			New:    cache.Module,
			DescEN: "Layered cache: otter memory + redis + circuit breaker.",
			DescZH: "分层缓存：otter 内存层 + redis 层 + 熔断器。",
		},
		{
			ImportPath: root + "scheduler", Pkg: "scheduler", Constructor: "scheduler.Module()",
			New:    scheduler.Module,
			DescEN: "robfig cron with panic-safety, overlap policies and stats.",
			DescZH: "robfig cron（panic 防护、重叠策略、统计）。",
		},
		{
			ImportPath: root + "audit", Pkg: "audit", Constructor: "audit.Module()",
			New:    audit.Module,
			DescEN: "Compliance audit log: async DB sink, purge cron, admin API (opt-in).",
			DescZH: "合规审计日志：异步 DB sink、清理 cron、admin API（显式启用）。",
		},
		{
			ImportPath: root + "outbox", Pkg: "outbox", Constructor: "outbox.Module()",
			New:    func() kernel.Component { return outbox.Module() },
			DescEN: "Transactional outbox: same-transaction enqueue + at-least-once relay delivery.",
			DescZH: "事务性 outbox：同事务入队 + at-least-once relay 投递。",
		},
		{
			ImportPath: root + "authz", Pkg: "authz", Constructor: "authz.Module()",
			New:    authz.Module,
			DescEN: "casbin RBAC engine: adapter, Redis watcher, bootstrap seeding, decision audit.",
			DescZH: "casbin RBAC 引擎：adapter、Redis watcher、bootstrap 播种、决策审计。",
			Enums:  map[string][]string{"driver": {"casbin"}},
		},
		{
			ImportPath: root + "account", Pkg: "account", Constructor: "account.Module()",
			New:    func() kernel.Component { return account.Module() },
			DescEN: "User module: register/login/JWT/reset + login rate limit + OAuth providers.",
			DescZH: "用户模块：注册/登录/JWT/重置 + 登录限速 + OAuth providers。",
		},
	}
}

// Providers returns the blessed OAuth providers in canonical order.
func Providers() []Provider {
	return []Provider{
		{Name: "google", ImportPath: root + "account/providers/google", Pkg: "google", Spec: google.Provider},
		{Name: "github", ImportPath: root + "account/providers/github", Pkg: "github", Spec: github.Provider},
		{Name: "facebook", ImportPath: root + "account/providers/facebook", Pkg: "facebook", Spec: facebook.Provider},
		{Name: "apple", ImportPath: root + "account/providers/apple", Pkg: "apple", Spec: apple.Provider},
	}
}

// MigrationSequences returns the built-in owner-managed database histories in
// stable component order. The CLI consumes these exact descriptors; runtime
// modules call the same package-level constructors.
func MigrationSequences() []db.Sequence {
	return []db.Sequence{
		account.MigrationSequence(),
		audit.MigrationSequence(),
		authz.MigrationSequence(),
		outbox.MigrationSequence(),
	}
}

// BySection returns the inventory keyed by config section (note the
// web module's section is "http", not "web").
func BySection() map[string]Module {
	out := make(map[string]Module)
	for _, m := range Modules() {
		out[kernel.SectionKeyOf(m.New().Describe())] = m
	}
	return out
}

// ProviderByName returns the blessed provider for a yaml key.
func ProviderByName(name string) (Provider, bool) {
	for _, p := range Providers() {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

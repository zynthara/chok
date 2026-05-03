package config

import (
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
)

// Validatable is implemented by config structs that need validation.
// Run() calls Validate() on each sub-Options after unmarshal,
// then on the root config if it implements Validatable.
type Validatable interface {
	Validate() error
}

// SelfValidating signals that a Validatable type already validates its
// nested fields itself and the framework's recursive walker must NOT
// descend into them. Discriminator-shaped configs (e.g. DatabaseOptions
// where `driver: sqlite` selects between two mutually exclusive
// branches) implement this marker because blindly recursing would trip
// the validators on the *unselected* branch — those branches commonly
// have `enabled: true` defaults that demand fields the user
// deliberately left blank because they picked the other driver.
//
// The interface intentionally has no methods; it is a pure type tag.
type SelfValidating interface {
	Validatable
	selfValidating()
}

type HTTPOptions struct {
	Enabled           bool          `mapstructure:"enabled"            default:"true"`
	Addr              string        `mapstructure:"addr"               default:":8080"`
	ReadTimeout       time.Duration `mapstructure:"read_timeout"       default:"30s"`
	WriteTimeout      time.Duration `mapstructure:"write_timeout"      default:"30s"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout" default:"10s"`
	IdleTimeout       time.Duration `mapstructure:"idle_timeout"       default:"120s"`
	// RequestTimeout is the maximum duration for processing a single HTTP
	// request. When non-zero, a Timeout middleware is automatically added
	// to the middleware stack, cancelling the request context and returning
	// 504 if the handler does not complete in time. Zero disables the
	// middleware (the default).
	RequestTimeout time.Duration `mapstructure:"request_timeout"`
	// DrainDelay is the pause between marking /readyz as 503 and actually
	// stopping the HTTP server. In Kubernetes deployments this gives the
	// load balancer time to deregister the pod before in-flight requests
	// are drained. Default 5s; zero disables the delay.
	DrainDelay time.Duration `mapstructure:"drain_delay" default:"5s"`

	// TrustedProxies is the list of CIDRs / IPs whose X-Forwarded-For and
	// X-Real-IP headers gin may honour when computing c.ClientIP(). Empty
	// slice (the default) trusts NO proxy — c.ClientIP() returns the
	// direct socket peer. Set to ["127.0.0.1"] when fronted by a local
	// reverse proxy, ["10.0.0.0/8"] behind an in-cluster LB, etc.
	//
	// Not setting this means loginLimiter's IP-keyed bucket is bypassable
	// by any client spoofing X-Forwarded-For; the fail-closed default
	// avoids that trap.
	TrustedProxies []string `mapstructure:"trusted_proxies"`
}

func (o *HTTPOptions) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Addr == "" {
		return fmt.Errorf("http: addr must not be empty")
	}
	if o.ReadTimeout < 0 {
		return fmt.Errorf("http: read_timeout must not be negative")
	}
	if o.WriteTimeout < 0 {
		return fmt.Errorf("http: write_timeout must not be negative")
	}
	if o.ReadHeaderTimeout < 0 {
		return fmt.Errorf("http: read_header_timeout must not be negative")
	}
	if o.IdleTimeout < 0 {
		return fmt.Errorf("http: idle_timeout must not be negative")
	}
	// TrustedProxies entries must be parseable as IP addresses or CIDR
	// blocks. Catching malformed values here surfaces misconfiguration
	// during config load rather than panicking in NewHTTPServer at
	// Component Init.
	for _, p := range o.TrustedProxies {
		if p == "" {
			return fmt.Errorf("http: trusted_proxies entries must not be empty")
		}
		if _, _, err := net.ParseCIDR(p); err == nil {
			continue
		}
		if ip := net.ParseIP(p); ip != nil {
			continue
		}
		return fmt.Errorf("http: invalid trusted_proxies entry %q: must be IP or CIDR", p)
	}
	return nil
}

// DatabaseOptions provides a discriminator-based database configuration.
// Instead of having separate MySQLOptions.Enabled and SQLiteOptions.Enabled
// fields (which can conflict), users set a single Driver field:
//
//	database:
//	  driver: sqlite
//	  sqlite:
//	    path: "app.db"
//	  mysql:
//	    host: "127.0.0.1"
//
// When Driver is empty, the component is disabled. The Driver field selects
// which nested config block is used; the other is ignored.
type DatabaseOptions struct {
	Driver string        `mapstructure:"driver"` // "sqlite" or "mysql"; empty = disabled
	SQLite SQLiteOptions `mapstructure:"sqlite"`
	MySQL  MySQLOptions  `mapstructure:"mysql"`
}

// selfValidating tags DatabaseOptions so the framework's recursive
// validator stops here. Without this, the walker would descend into
// SQLite/MySQL after our Validate succeeded and re-run their own
// Validate methods — which check `enabled: true` defaults and demand
// fields the user did not set on the *unselected* driver branch.
func (*DatabaseOptions) selfValidating() {}

func (o *DatabaseOptions) Validate() error {
	// Driver is the sole enable switch for DatabaseOptions — a non-empty
	// driver selects the nested block and demands its fields, regardless
	// of that block's Enabled flag. This keeps the discriminator contract
	// obvious ("driver=sqlite means sqlite is on") and prevents the
	// surprising configuration where `driver: sqlite` + `sqlite.enabled:
	// false` both validates and still starts the DB at Init time.
	switch o.Driver {
	case "":
		return nil // disabled
	case "sqlite":
		if o.SQLite.Path == "" {
			return fmt.Errorf("sqlite: path must not be empty")
		}
		return nil
	case "mysql":
		if o.MySQL.Host == "" {
			return fmt.Errorf("mysql: host must not be empty")
		}
		if o.MySQL.Port <= 0 || o.MySQL.Port > 65535 {
			return fmt.Errorf("mysql: port must be 1-65535, got %d", o.MySQL.Port)
		}
		if o.MySQL.Database == "" {
			return fmt.Errorf("mysql: database must not be empty")
		}
		return nil
	default:
		return fmt.Errorf("database: unsupported driver %q (use sqlite or mysql)", o.Driver)
	}
}

type MySQLOptions struct {
	Enabled         bool          `mapstructure:"enabled"            default:"true"`
	Host            string        `mapstructure:"host"               default:"127.0.0.1"`
	Port            int           `mapstructure:"port"               default:"3306"`
	Username        string        `mapstructure:"username"`
	Password        string        `mapstructure:"password"           sensitive:"true"`
	Database        string        `mapstructure:"database"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"     default:"100"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"     default:"10"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"  default:"1h"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time" default:"10m"`
}

func (o *MySQLOptions) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Host == "" {
		return fmt.Errorf("mysql: host must not be empty")
	}
	if o.Port <= 0 || o.Port > 65535 {
		return fmt.Errorf("mysql: port must be 1–65535, got %d", o.Port)
	}
	if o.Database == "" {
		return fmt.Errorf("mysql: database must not be empty")
	}
	return nil
}

type SQLiteOptions struct {
	Enabled bool   `mapstructure:"enabled" default:"true"`
	Path    string `mapstructure:"path"    default:"app.db"`
}

func (o *SQLiteOptions) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Path == "" {
		return fmt.Errorf("sqlite: path must not be empty")
	}
	return nil
}

type RedisOptions struct {
	Enabled  bool   `mapstructure:"enabled"  default:"true"`
	Addr     string `mapstructure:"addr"     default:"127.0.0.1:6379"`
	Password string `mapstructure:"password"  sensitive:"true"`
	DB       int    `mapstructure:"db"       default:"0"`

	// Network timeouts. Defaults are tighter than go-redis' library
	// defaults (DialTimeout 5s, ReadTimeout 3s) because Redis on the hot
	// path of a web request should fail fast and let the caller fall back
	// (cache miss, circuit breaker) instead of stretching every request
	// to the library timeout.
	DialTimeout  time.Duration `mapstructure:"dial_timeout"  default:"1s"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"  default:"500ms"`
	WriteTimeout time.Duration `mapstructure:"write_timeout" default:"500ms"`
	PoolTimeout  time.Duration `mapstructure:"pool_timeout"  default:"1s"`
	PoolSize     int           `mapstructure:"pool_size"     default:"10"`
}

func (o *RedisOptions) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Addr == "" {
		return fmt.Errorf("redis: addr must not be empty")
	}
	if o.DB < 0 {
		return fmt.Errorf("redis: db must not be negative")
	}
	if o.DialTimeout < 0 {
		return fmt.Errorf("redis: dial_timeout must not be negative")
	}
	if o.ReadTimeout < 0 {
		return fmt.Errorf("redis: read_timeout must not be negative")
	}
	if o.WriteTimeout < 0 {
		return fmt.Errorf("redis: write_timeout must not be negative")
	}
	if o.PoolTimeout < 0 {
		return fmt.Errorf("redis: pool_timeout must not be negative")
	}
	if o.PoolSize < 0 {
		return fmt.Errorf("redis: pool_size must not be negative")
	}
	return nil
}

type SlogOptions struct {
	Level  string           `mapstructure:"level"  default:"info"`
	Format string           `mapstructure:"format" default:"json"`
	Output []string         `mapstructure:"output" default:"stdout"`
	Files  []LogFileOptions `mapstructure:"files"`
	// AccessFiles, if non-empty, routes access-log entries (the ones written via
	// App.AccessLogger()) to a separate set of rotating files. Output is still
	// applied (stdout shows both streams) but app-log Files do NOT receive access
	// entries when this is set. Empty = access shares the main logger (legacy).
	AccessFiles []LogFileOptions `mapstructure:"access_files"`
	// AccessEnabled controls whether the application should record HTTP access
	// logs at all. Set false when a fronting proxy (nginx, traefik, etc.) already
	// captures access logs and you want to avoid double-logging. Application code
	// should consult App.AccessLogEnabled() before mounting the AccessLog
	// middleware. Default: true.
	AccessEnabled bool `mapstructure:"access_enabled" default:"true"`
}

// LogFileOptions configures a rotating log file output (lumberjack-backed).
// Empty path disables the entry; rotation thresholds are size-based with optional age/backup caps.
type LogFileOptions struct {
	Path       string `mapstructure:"path"`
	MaxSizeMB  int    `mapstructure:"max_size_mb"  default:"100"`
	MaxBackups int    `mapstructure:"max_backups"  default:"7"`
	MaxAgeDays int    `mapstructure:"max_age_days" default:"30"`
	Compress   bool   `mapstructure:"compress"     default:"true"`
}

func (o *SlogOptions) Validate() error {
	switch o.Level {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("log: unsupported level %q (use debug/info/warn/error)", o.Level)
	}
	switch o.Format {
	case "json", "text":
	default:
		return fmt.Errorf("log: unsupported format %q (use json/text)", o.Format)
	}
	// Reject blank Output entries up front. Blank strings would silently
	// drop through buildWriter and could confuse later debugging when an
	// operator stares at a config that "should write to a file".
	for i, name := range o.Output {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("log.output[%d]: must not be empty", i)
		}
	}
	// Determine whether at least one *effective* sink is configured.
	// LogFileOptions documents a blank Path as "disabled", so a slice of
	// all-blank entries equates to no file sink even though len > 0.
	// The previous len-only check let `files: [{path: ""}]` pass while
	// silently producing zero outputs.
	effectiveFiles := 0
	for i := range o.Files {
		if err := o.Files[i].Validate(); err != nil {
			return fmt.Errorf("log.files[%d]: %w", i, err)
		}
		if o.Files[i].Path != "" {
			effectiveFiles++
		}
	}
	for i := range o.AccessFiles {
		if err := o.AccessFiles[i].Validate(); err != nil {
			return fmt.Errorf("log.access_files[%d]: %w", i, err)
		}
	}
	if len(o.Output) == 0 && effectiveFiles == 0 {
		return fmt.Errorf("log: output or files must not both be empty")
	}
	return nil
}

func (f *LogFileOptions) Validate() error {
	if f.Path == "" {
		return nil // empty path disables the entry
	}
	if f.MaxSizeMB < 0 {
		return fmt.Errorf("max_size_mb must not be negative")
	}
	if f.MaxBackups < 0 {
		return fmt.Errorf("max_backups must not be negative")
	}
	if f.MaxAgeDays < 0 {
		return fmt.Errorf("max_age_days must not be negative")
	}
	return nil
}

type CacheMemoryOptions struct {
	Enabled  bool          `mapstructure:"enabled"  default:"false"`
	Capacity int           `mapstructure:"capacity" default:"10000"`
	TTL      time.Duration `mapstructure:"ttl"      default:"5m"`
}

func (o *CacheMemoryOptions) Validate() error {
	if o.Enabled && o.Capacity <= 0 {
		return fmt.Errorf("cache.memory: capacity must be positive when enabled")
	}
	if o.TTL < 0 {
		return fmt.Errorf("cache.memory: ttl must not be negative")
	}
	return nil
}

type SwaggerOptions struct {
	Enabled    bool   `mapstructure:"enabled"     default:"false"`
	Title      string `mapstructure:"title"`
	Version    string `mapstructure:"version"     default:"1.0.0"`
	Prefix     string `mapstructure:"prefix"      default:"/swagger"`
	BearerAuth bool   `mapstructure:"bearer_auth" default:"true"`
}

type AccountOptions struct {
	Enabled         bool          `mapstructure:"enabled"          default:"false"`
	SigningKey      string        `mapstructure:"signing_key"       sensitive:"true"`
	Expiration      time.Duration `mapstructure:"expiration"       default:"2h"`
	ResetExpiration time.Duration `mapstructure:"reset_expiration" default:"15m"`
	// LoginRateWindow + LoginRateLimit configure per-email login attempt
	// throttling on /login. When both are positive the account module
	// installs a sliding-window limiter; on threshold exceedance the
	// endpoint returns 429 Too Many Requests. Both zero (the default)
	// disables the limiter entirely. Recommended production values:
	// window=15m, limit=10.
	LoginRateWindow time.Duration `mapstructure:"login_rate_window"`
	LoginRateLimit  int           `mapstructure:"login_rate_limit"`
	// DisableRegister 关闭 POST /register 公开注册端点。true 时仍支持 login、
	// change-password、reset-password 等已认证路径；公开注册路径不注册，
	// 直接访问返回 404。适合"内部使用 / 仅 admin 创建账号"的场景。
	DisableRegister bool `mapstructure:"disable_register" default:"false"`

	// LinkByEmail enables the SPEC §8 LinkByEmail auto-merge path. The
	// account module enforces additional double-checks (IdP-side
	// EmailVerified=true, !IsAliasedEmail, local-side EmailVerified=true)
	// even with this flag on, so default-off is safe; turn on only after
	// the application has wired its own email-verification UI.
	LinkByEmail bool `mapstructure:"link_by_email" default:"false"`

	// AllowedRedirectBacks lists absolute URL prefixes that
	// /auth/{name}/start will accept on its ?redirect_back parameter.
	// Empty (the default) means relative paths only — the strictest
	// posture. Each entry must be a fully-qualified https:// URL with
	// no userinfo / query / fragment; account.Module rejects malformed
	// entries at startup. See SPEC §6.1.
	AllowedRedirectBacks []string `mapstructure:"allowed_redirect_backs"`

	// OAuthCallbackFrontendURL is the fixed front-end landing URL the
	// OAuth callback flow redirects to after issuing the one-shot auth
	// code. The SPA there calls POST /auth/exchange to swap the code
	// for a JWT (which never appears in the URL). REQUIRED whenever any
	// provider in Providers has enabled=true.
	OAuthCallbackFrontendURL string `mapstructure:"oauth_callback_frontend_url"`

	// Providers maps provider name → raw config. Each entry is decoded
	// by the provider package's factory (registered via
	// account.RegisterProviderFactory in init()). Unknown provider
	// names cause the builder to fail-fast at startup so a typo doesn't
	// silently disable an IdP. SPEC §10.3.
	Providers map[string]ProviderRawOptions `mapstructure:"providers"`
}

// ProviderRawOptions is the yaml-side representation of a single
// provider's configuration. Concrete shape (client_id, client_secret,
// scopes, …) varies per provider, so the typed fields here are minimal
// (just Enabled) and provider-specific keys land in Raw via
// mapstructure's `,remain` mechanism. The provider package then calls
// Decode to convert Raw into its own typed Options struct.
//
// This lets config.AccountOptions stay independent of the provider
// packages — no circular import: account/providers/google imports
// config, not the other way round.
type ProviderRawOptions struct {
	// Enabled is the master switch. The builder skips entries with
	// Enabled=false even if they appear in yaml.
	Enabled bool `mapstructure:"enabled"`
	// Raw collects every key under the provider entry that isn't
	// `enabled`. mapstructure routes unknown keys here when the
	// `,remain` tag is present.
	Raw map[string]any `mapstructure:",remain"`
}

// Decode converts the provider-specific Raw map into a typed Options
// struct. Provider factories use it to extract their config:
//
//	var opts google.Options
//	if err := raw.Decode(&opts); err != nil { return nil, err }
//
// The `mapstructure` tags on the target struct drive the field mapping.
// time.Duration and similar string-like types are honoured via the
// default decoder hooks chok already wires for viper.Unmarshal.
func (r *ProviderRawOptions) Decode(out any) error {
	if r == nil {
		return fmt.Errorf("ProviderRawOptions: nil receiver")
	}
	cfg := &mapstructure.DecoderConfig{
		Result:           out,
		TagName:          "mapstructure",
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	}
	dec, err := mapstructure.NewDecoder(cfg)
	if err != nil {
		return err
	}
	return dec.Decode(r.Raw)
}

func (o *AccountOptions) Validate() error {
	if !o.Enabled {
		return nil
	}
	if len(o.SigningKey) < 32 {
		return fmt.Errorf("account: signing_key must be at least 32 bytes")
	}
	if o.Expiration < 0 {
		return fmt.Errorf("account: expiration must not be negative")
	}
	if o.ResetExpiration < 0 {
		return fmt.Errorf("account: reset_expiration must not be negative")
	}
	if o.LoginRateWindow < 0 {
		return fmt.Errorf("account: login_rate_window must not be negative")
	}
	if o.LoginRateLimit < 0 {
		return fmt.Errorf("account: login_rate_limit must not be negative")
	}
	// Half-configured limiter is almost certainly an operator mistake
	// (one field set, the other left zero) — refuse rather than silently
	// disabling. Either both > 0 (limiter on) or both == 0 (off).
	if (o.LoginRateWindow > 0) != (o.LoginRateLimit > 0) {
		return fmt.Errorf("account: login_rate_window and login_rate_limit must both be set or both be zero")
	}
	// SPEC §10.3: any enabled provider requires the front-end landing
	// URL because the callback 302 ends in `?code=…` and the SPA there
	// is the one running /auth/exchange. Without it the OAuth round
	// trip can't complete.
	hasEnabledProvider := false
	for _, p := range o.Providers {
		if p.Enabled {
			hasEnabledProvider = true
			break
		}
	}
	if hasEnabledProvider && o.OAuthCallbackFrontendURL == "" {
		return fmt.Errorf("account: oauth_callback_frontend_url is required when any provider is enabled")
	}
	return nil
}

type CacheFileOptions struct {
	Enabled bool          `mapstructure:"enabled" default:"false"`
	Path    string        `mapstructure:"path"    default:".cache"`
	TTL     time.Duration `mapstructure:"ttl"     default:"1h"`
}

func (o *CacheFileOptions) Validate() error {
	if o.Enabled && o.Path == "" {
		return fmt.Errorf("cache.file: path must not be empty when enabled")
	}
	if o.TTL < 0 {
		return fmt.Errorf("cache.file: ttl must not be negative")
	}
	return nil
}

// HealthOptions configures the /healthz endpoint.
// When Enabled is true and an HTTP server exists, the HealthComponent
// is auto-registered. Enabled defaults to true so health is available
// out of the box; set false to disable.
type HealthOptions struct {
	Enabled bool   `mapstructure:"enabled" default:"true"`
	Path    string `mapstructure:"path"    default:"/healthz"`
}

func (o *HealthOptions) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Path != "" && !strings.HasPrefix(o.Path, "/") {
		return fmt.Errorf("health: path must start with /, got %q", o.Path)
	}
	return nil
}

// MetricsOptions configures the /metrics (Prometheus) endpoint.
// Same auto-registration semantics as HealthOptions.
type MetricsOptions struct {
	Enabled bool   `mapstructure:"enabled" default:"true"`
	Path    string `mapstructure:"path"    default:"/metrics"`
}

func (o *MetricsOptions) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Path != "" && !strings.HasPrefix(o.Path, "/") {
		return fmt.Errorf("metrics: path must start with /, got %q", o.Path)
	}
	return nil
}

// DebugOptions configures the /componentz debug endpoint.
// Disabled by default — enable for development or staging environments
// to expose component topology and init timing via HTTP.
type DebugOptions struct {
	Enabled bool `mapstructure:"enabled" default:"false"`
}

// --- Sensitive field audit ---------------------------------------------------

// SensitiveWarning describes a sensitive config field that appears to hold a
// default / placeholder value rather than a real secret.
type SensitiveWarning struct {
	Path    string // e.g. "Account.SigningKey"
	EnvHint string // e.g. "MYAPP_ACCOUNT_SIGNING_KEY"
}

// AuditSensitiveDefaults walks a config struct recursively and returns
// warnings for any field tagged sensitive:"true" whose value looks like
// a placeholder (contains "CHANGE", "TODO", "FIXME", "example", or is
// empty while the parent struct is enabled).
//
// envPrefix is the application's environment variable prefix (e.g. "BLOG")
// so the warning can suggest the correct env var override.
func AuditSensitiveDefaults(cfg any, envPrefix string) []SensitiveWarning {
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	var warnings []SensitiveWarning
	auditSensitiveFields(v, "", envPrefix, &warnings)
	return warnings
}

func auditSensitiveFields(v reflect.Value, prefix, envPrefix string, out *[]SensitiveWarning) {
	t := v.Type()
	for i := range t.NumField() {
		ft := t.Field(i)
		if !ft.IsExported() {
			continue
		}
		fv := v.Field(i)

		path := ft.Name
		if prefix != "" {
			path = prefix + "." + ft.Name
		}

		mapKey := ft.Tag.Get("mapstructure")
		if mapKey == "" {
			mapKey = strings.ToLower(ft.Name)
		}

		if fv.Kind() == reflect.Ptr {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
		}

		// Check sensitive string fields.
		if ft.Tag.Get("sensitive") == "true" && ft.Type.Kind() == reflect.String {
			val := fv.String()
			if looksLikePlaceholder(val) {
				envKey := envPrefix
				if prefix != "" {
					parent := ft.Tag.Get("mapstructure")
					if parent == "" {
						parent = strings.ToLower(ft.Name)
					}
					// Build full env key from path.
					envKey = envPrefix + "_" + strings.ToUpper(strings.ReplaceAll(prefix, ".", "_")) + "_" + strings.ToUpper(mapKey)
				} else {
					envKey = envPrefix + "_" + strings.ToUpper(mapKey)
				}
				*out = append(*out, SensitiveWarning{
					Path:    path,
					EnvHint: envKey,
				})
			}
		}

		// Recurse into nested structs.
		if fv.Kind() == reflect.Struct {
			childPrefix := mapKey
			if prefix != "" {
				childPrefix = prefix + "." + mapKey
			}
			auditSensitiveFields(fv, path, envPrefix, out)
			_ = childPrefix // prefix for human-readable path uses struct name
		}
	}
}

func looksLikePlaceholder(val string) bool {
	if val == "" {
		return false // empty is not a placeholder — might be legitimately optional
	}
	upper := strings.ToUpper(val)
	for _, marker := range []string{"CHANGE", "TODO", "FIXME", "EXAMPLE", "REPLACE", "SECRET", "CHANGEME", "CHANGE-ME"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// --- Sensitive field masking ------------------------------------------------

const redactedPlaceholder = "***"

// Redact returns a shallow copy of the config struct with all fields tagged
// sensitive:"true" replaced by "***". Works on any struct (including nested
// ones). The original struct is never modified.
//
// Use this before logging or serializing config to prevent credential leaks:
//
//	logger.Info("config loaded", "config", config.Redact(&cfg))
func Redact(cfg any) any {
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return cfg
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return cfg
	}
	return redactValue(v).Interface()
}

// redactValue recursively copies a struct value, masking sensitive fields.
//
// Walks through:
//   - struct fields tagged sensitive:"true" (string only — replaced verbatim)
//   - nested structs / pointers to structs (recurse)
//   - map[string]V values (recurse into each value, plus heuristic
//     mask of map keys whose name looks like a secret — see
//     redactSensitiveMap)
//   - slices / arrays of structs (recurse element-wise)
//
// Without the map and slice paths, ProviderRawOptions.Raw
// (map[string]any) leaks every key the provider yaml carried — most
// of which are exactly the secrets we want hidden (client_secret,
// private_key, ...). Maps are by-reference in Go's reflect so we
// reflect.MakeMap a fresh copy rather than mutating the caller's data.
func redactValue(v reflect.Value) reflect.Value {
	out := reflect.New(v.Type()).Elem()
	out.Set(v)
	t := v.Type()
	for i := range t.NumField() {
		ft := t.Field(i)
		if !ft.IsExported() {
			continue
		}
		fv := out.Field(i)
		if ft.Tag.Get("sensitive") == "true" && ft.Type.Kind() == reflect.String {
			if fv.String() != "" {
				fv.SetString(redactedPlaceholder)
			}
			continue
		}
		fv.Set(redactReflectValue(fv))
	}
	return out
}

// redactReflectValue dispatches on kind. Struct → redactValue;
// Ptr/Interface → unwrap and recurse;  Map → redactSensitiveMap;
// Slice/Array → redact each element. Anything else passes through
// unchanged. Returns a value of the same type as v so callers can
// Set() directly.
func redactReflectValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return v
		}
		inner := redactReflectValue(v.Elem())
		ptr := reflect.New(v.Type().Elem())
		ptr.Elem().Set(inner)
		return ptr
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		inner := redactReflectValue(v.Elem())
		// Wrap the redacted inner value back into the interface type.
		out := reflect.New(v.Type()).Elem()
		out.Set(inner)
		return out
	case reflect.Struct:
		return redactValue(v)
	case reflect.Map:
		return redactSensitiveMap(v)
	case reflect.Slice, reflect.Array:
		// Skip []byte and similar — only recurse when elements are
		// containers. Strings inside slices have no sensitive: tag
		// (it lives on struct fields) so leaving them untouched is
		// correct.
		ek := v.Type().Elem().Kind()
		if ek != reflect.Struct && ek != reflect.Ptr && ek != reflect.Interface && ek != reflect.Map {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(redactReflectValue(v.Index(i)))
		}
		return out
	default:
		return v
	}
}

// redactSensitiveMap returns a fresh map with sensitive keys masked.
// "Sensitive" means the key name (case-insensitive) matches one of the
// well-known secret-shaped tokens — see isSensitiveKey. Non-sensitive
// keys pass through, including a recurse for nested struct/map values.
func redactSensitiveMap(v reflect.Value) reflect.Value {
	if v.IsNil() {
		return v
	}
	out := reflect.MakeMapWithSize(v.Type(), v.Len())
	iter := v.MapRange()
	for iter.Next() {
		k := iter.Key()
		val := iter.Value()
		if k.Kind() == reflect.String && isSensitiveKey(k.String()) {
			masked := reflect.New(val.Type()).Elem()
			// Replace the value with the redactedPlaceholder string,
			// re-wrapped into whatever interface{} / typed slot the
			// map holds. Non-string slots get a generic "***" string
			// boxed via interface{} — adequate for the diagnostic
			// path Redact serves.
			placeholder := reflect.ValueOf(redactedPlaceholder)
			if placeholder.Type().AssignableTo(val.Type()) {
				masked.Set(placeholder)
			} else if val.Kind() == reflect.Interface {
				masked.Set(placeholder)
			}
			out.SetMapIndex(k, masked)
			continue
		}
		out.SetMapIndex(k, redactReflectValue(val))
	}
	return out
}

// isSensitiveKey reports whether a config map key name looks like it
// holds a secret. We err on the side of redaction — false positives
// only blank a value in a diagnostic dump, while a false negative
// leaks credentials.
func isSensitiveKey(name string) bool {
	lower := strings.ToLower(name)
	for _, tok := range []string{
		"secret",
		"password",
		"passwd",
		"private_key",
		"privatekey",
		"api_key",
		"apikey",
		"token",
		"signing_key",
		"client_secret",
	} {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// Defined-but-unmethoded twins of the *Options types whose GoString
// implementations need to print "raw struct fields" without recursing
// through their own GoString. Go's method-set rules: methods declared
// on type T do NOT automatically belong to `type U T` — only struct
// embedding promotes methods, and that promotion path is what made
// the earlier `struct{ T }{o}` trick infinite-loop on %#v. A separate
// named type with the same underlying layout sidesteps the recursion
// entirely.
type (
	mysqlOptionsRaw   MySQLOptions
	redisOptionsRaw   RedisOptions
	accountOptionsRaw AccountOptions
)

// GoString implements fmt.GoStringer for MySQLOptions.
// Prevents accidental credential leaks when using %#v.
func (o MySQLOptions) GoString() string {
	o.Password = redactSensitive(o.Password)
	return fmt.Sprintf("%#v", mysqlOptionsRaw(o))
}

// GoString implements fmt.GoStringer for RedisOptions.
func (o RedisOptions) GoString() string {
	o.Password = redactSensitive(o.Password)
	return fmt.Sprintf("%#v", redisOptionsRaw(o))
}

// GoString implements fmt.GoStringer for AccountOptions.
//
// Mask both SigningKey and any sensitive map keys inside
// Providers[name].Raw — without the latter, %#v on an AccountOptions
// with OAuth providers configured leaks every client_secret / private_key
// the operator put in yaml.
func (o AccountOptions) GoString() string {
	o.SigningKey = redactSensitive(o.SigningKey)
	if len(o.Providers) > 0 {
		safe := make(map[string]ProviderRawOptions, len(o.Providers))
		for name, raw := range o.Providers {
			safe[name] = ProviderRawOptions{
				Enabled: raw.Enabled,
				Raw:     redactSensitiveAnyMap(raw.Raw),
			}
		}
		o.Providers = safe
	}
	return fmt.Sprintf("%#v", accountOptionsRaw(o))
}

// redactSensitiveAnyMap is the AccountOptions-specific shim around
// redactSensitiveMap that takes / returns the concrete
// map[string]any type. Reflect-free for the hot path; callers of
// Redact() still hit the generic version through redactReflectValue.
func redactSensitiveAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if isSensitiveKey(k) {
			out[k] = redactedPlaceholder
			continue
		}
		// Nested map values get a recursive pass so a struct-shaped
		// provider config (like nested objects) still gets cleaned.
		if nested, ok := v.(map[string]any); ok {
			out[k] = redactSensitiveAnyMap(nested)
			continue
		}
		out[k] = v
	}
	return out
}

// String implements fmt.Stringer for MySQLOptions.
func (o MySQLOptions) String() string {
	return formatOptions("mysql", map[string]any{
		"host": o.Host, "port": o.Port, "database": o.Database,
		"username": o.Username, "password": redactSensitive(o.Password),
	})
}

// String implements fmt.Stringer for RedisOptions.
func (o RedisOptions) String() string {
	return formatOptions("redis", map[string]any{
		"addr": o.Addr, "db": o.DB, "password": redactSensitive(o.Password),
	})
}

// String implements fmt.Stringer for AccountOptions.
func (o AccountOptions) String() string {
	return formatOptions("account", map[string]any{
		"enabled": o.Enabled, "signing_key": redactSensitive(o.SigningKey),
	})
}

func redactSensitive(v string) string {
	if v == "" {
		return ""
	}
	return redactedPlaceholder
}

func formatOptions(name string, fields map[string]any) string {
	parts := make([]string, 0, len(fields))
	for k, v := range fields {
		parts = append(parts, fmt.Sprintf("%s:%v", k, v))
	}
	return fmt.Sprintf("{%s %s}", name, strings.Join(parts, " "))
}

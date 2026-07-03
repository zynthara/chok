package web

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// Options is the "http" yaml section (the v1 section shape carried
// over; h2c and shutdown_timeout are v2 additions). Everything here
// shapes server construction, so every field is restart-only — the
// framework reload diff warns on changes without dispatching.
type Options struct {
	Enabled bool   `mapstructure:"enabled" default:"true"`
	Addr    string `mapstructure:"addr"    default:":8080"    reload:"restart"`

	ReadTimeout       time.Duration `mapstructure:"read_timeout"        default:"30s"  reload:"restart"`
	WriteTimeout      time.Duration `mapstructure:"write_timeout"       default:"30s"  reload:"restart"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout" default:"10s"  reload:"restart"`
	IdleTimeout       time.Duration `mapstructure:"idle_timeout"        default:"120s" reload:"restart"`

	// RequestTimeout, when positive, adds the Timeout middleware:
	// the request context is cancelled and a 504 envelope written if
	// the handler produced nothing in time. Zero disables (default).
	RequestTimeout time.Duration `mapstructure:"request_timeout" reload:"restart"`

	// ShutdownTimeout bounds the graceful Shutdown on stop; when
	// in-flight requests outlive it, the server force-Closes so hung
	// handlers can't outlive registry teardown (v2 addition — the v1
	// server borrowed the App's stop context, but the kernel Serve
	// contract carries a cancellation signal, not a deadline).
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout" default:"10s" reload:"restart"`

	// DrainDelay is the pause between readiness flipping to 503
	// (draining phase) and Serve contexts being cancelled. The App
	// inherits it into the kernel draining phase unless WithDrainDelay
	// was set explicitly (SPEC §9; default 5s, zero disables).
	DrainDelay time.Duration `mapstructure:"drain_delay" default:"5s" reload:"restart"`

	// TrustedProxies is the list of CIDRs / IPs whose X-Forwarded-For
	// and X-Real-IP headers the ClientIP resolver may honour. Empty
	// (the default) trusts NO proxy — the client IP is the direct
	// socket peer. Set to ["127.0.0.1"] behind a local reverse proxy,
	// ["10.0.0.0/8"] behind an in-cluster LB, etc.
	//
	// Not setting this means the account login limiter's IP-keyed
	// bucket would be bypassable by any client spoofing
	// X-Forwarded-For; the fail-closed default avoids that trap.
	TrustedProxies []string `mapstructure:"trusted_proxies" reload:"restart"`

	// H2C enables cleartext HTTP/2 (v2 addition): gRPC-style clients
	// and in-cluster meshes can speak h2 without TLS termination.
	H2C bool `mapstructure:"h2c" default:"false" reload:"restart"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
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
	if o.ShutdownTimeout <= 0 {
		return fmt.Errorf("http: shutdown_timeout must be positive")
	}
	if o.DrainDelay < 0 {
		return fmt.Errorf("http: drain_delay must not be negative")
	}
	// TrustedProxies entries must be parseable as IP addresses or CIDR
	// blocks. Catching malformed values here surfaces misconfiguration
	// during config load rather than during component Init.
	for _, p := range o.TrustedProxies {
		p = strings.TrimSpace(p)
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

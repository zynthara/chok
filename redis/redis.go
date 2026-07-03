// Package redis is the chok v2 redis battery: a shared *redis.Client
// as a kernel component plus the library-level constructor and TLS
// helper for kernel-less use.
package redis

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	"github.com/redis/go-redis/v9"
)

// New creates a Redis client from Options — the same constructor the
// redis module uses at Init. Validation runs first, so a misconfigured
// Options fails here rather than at first command.
//
// Note: the returned client is lazily connected — connection errors
// surface on first use. Use the module's Health probe or an explicit
// Ping for startup verification.
//
// Network timeouts fall through to go-redis library defaults only when
// the config value is zero; applications that explicitly want unbounded
// waits must set a negative value (go-redis convention).
func New(opts Options) (*redis.Client, error) {
	o := opts
	o.Enabled = true // library-level New means "use it"; the kill switch is a module concern
	if err := o.Validate(); err != nil {
		return nil, err
	}

	// Callers that construct Options in code (tests, custom wiring) may
	// skip the conf-default layer. Match the declared default="10" here
	// so PoolSize is never implicitly left as go-redis' own default
	// (10 * GOMAXPROCS — wildly different from the documented value).
	poolSize := o.PoolSize
	if poolSize <= 0 {
		poolSize = 10
	}

	tlsConfig, err := TLSConfigFor(o.Addr, o.TLS, o.CACert)
	if err != nil {
		return nil, err
	}

	client := redis.NewClient(&redis.Options{
		Addr:         o.Addr,
		Username:     o.Username,
		Password:     o.Password,
		DB:           o.DB,
		DialTimeout:  o.DialTimeout,
		ReadTimeout:  o.ReadTimeout,
		WriteTimeout: o.WriteTimeout,
		PoolTimeout:  o.PoolTimeout,
		PoolSize:     poolSize,
		TLSConfig:    tlsConfig,
	})
	return client, nil
}

// TLSConfigFor builds the *tls.Config implied by useTLS / caCert, or nil when
// neither is set. ServerName is derived from addr for SNI / hostname
// verification; a caCert, when present, replaces the system root CAs (managed
// providers present a private-CA certificate). Exported so callers that build
// their own go-redis client — e.g. a Casbin redis-watcher pointed at the same
// managed instance — get identical TLS behavior.
func TLSConfigFor(addr string, useTLS bool, caCert string) (*tls.Config, error) {
	if !useTLS && caCert == "" {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	cfg := &tls.Config{ServerName: host}
	if caCert != "" {
		pem, err := os.ReadFile(caCert)
		if err != nil {
			return nil, fmt.Errorf("redis: read ca_cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("redis: ca_cert %q contained no PEM certificates", caCert)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

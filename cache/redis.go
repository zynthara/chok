package cache

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/redis/go-redis/v9"
)

// go-redis reports its own trouble (a dial that failed, a connection it
// dropped) through a process-wide logger that writes to stderr; it is set
// once, before any client exists, to log through slog instead.
func init() { redis.SetLogger(redisLog{}) }

// redisLog passes go-redis's messages to slog.Default at warn.
type redisLog struct{}

func (redisLog) Printf(ctx context.Context, format string, v ...any) {
	slog.Default().WarnContext(ctx, fmt.Sprintf(format, v...), "logger", "go-redis")
}

// newClient builds the redis layer's client; it connects on first use.
func newClient(cfg Config) (*redis.Client, error) {
	r := cfg.Redis
	tlsCfg, err := tlsConfig(r.Addr, r.TLS, r.CACert)
	if err != nil {
		return nil, err
	}
	return redis.NewClient(&redis.Options{
		Addr:         r.Addr,
		Username:     r.Username,
		Password:     r.Password,
		DB:           r.DB,
		DialTimeout:  r.DialTimeout,
		ReadTimeout:  r.ReadTimeout,
		WriteTimeout: r.WriteTimeout,
		PoolSize:     r.PoolSize,
		TLSConfig:    tlsCfg,
	}), nil
}

// tlsConfig is the TLS configuration redis.tls and redis.ca_cert ask for,
// nil when neither is set. The server name comes from addr; a CA file
// replaces the system roots, as managed Redis services with a private CA
// need, and implies TLS.
func tlsConfig(addr string, useTLS bool, caCert string) (*tls.Config, error) {
	if !useTLS && caCert == "" {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if caCert != "" {
		pem, err := os.ReadFile(caCert)
		if err != nil {
			return nil, fmt.Errorf("cache: redis.ca_cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cache: redis.ca_cert %s holds no PEM certificate", caCert)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

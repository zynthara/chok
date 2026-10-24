package cache

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTLSConfig_Disabled(t *testing.T) {
	cfg, err := tlsConfig("127.0.0.1:6379", false, "")
	if err != nil || cfg != nil {
		t.Fatalf("no TLS config without tls and ca_cert: %v %v", cfg, err)
	}
}

func TestTLSConfig_EnabledNoCA(t *testing.T) {
	cfg, err := tlsConfig("redis.example.com:25061", true, "")
	if err != nil || cfg == nil {
		t.Fatalf("%v %v", cfg, err)
	}
	if cfg.ServerName != "redis.example.com" || cfg.RootCAs != nil {
		t.Fatalf("server name from addr, system roots: %q %v", cfg.ServerName, cfg.RootCAs)
	}
}

func TestTLSConfig_CACertImpliesTLS(t *testing.T) {
	cfg, err := tlsConfig("redis.example.com:25061", false, writeTestCA(t))
	if err != nil || cfg == nil || cfg.RootCAs == nil {
		t.Fatalf("ca_cert alone turns TLS on with that CA: %v %v", cfg, err)
	}
}

func TestTLSConfig_CACertMissing(t *testing.T) {
	if _, err := tlsConfig("r:6379", false, "/no/such/ca.pem"); err == nil || !strings.Contains(err.Error(), "ca_cert") {
		t.Fatalf("a missing ca_cert file: %v", err)
	}
}

func TestTLSConfig_CACertNotPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tlsConfig("r:6379", false, path); err == nil || !strings.Contains(err.Error(), "no PEM") {
		t.Fatalf("a ca_cert without PEM: %v", err)
	}
}

// TestNew_MapsOptionsAndTLS: the redis section becomes the client's
// options, with the defaults New fills in.
func TestNew_MapsOptionsAndTLS(t *testing.T) {
	var cfg Config
	cfg.Redis.Enabled = true
	cfg.Redis.Addr = "redis.example.com:25061"
	cfg.Redis.Username, cfg.Redis.Password, cfg.Redis.DB, cfg.Redis.TLS = "acl-user", "s3cret", 3, true
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(t.Context()) }()
	got := c.Redis().Options()
	if got.Username != "acl-user" || got.Password != "s3cret" || got.DB != 3 {
		t.Fatalf("credentials not mapped: %+v", got)
	}
	if got.TLSConfig == nil || got.TLSConfig.ServerName != "redis.example.com" {
		t.Fatalf("TLS with the server name from addr: %+v", got.TLSConfig)
	}
	if got.PoolSize != 10 || got.DialTimeout != time.Second || got.ReadTimeout != 500*time.Millisecond || got.WriteTimeout != 500*time.Millisecond {
		t.Fatalf("the defaults: pool %d, dial %s, read %s, write %s", got.PoolSize, got.DialTimeout, got.ReadTimeout, got.WriteTimeout)
	}
}

// TestNew_BadCACertFails: a ca_cert that cannot be read fails New, by
// name, before any connection.
func TestNew_BadCACertFails(t *testing.T) {
	var cfg Config
	cfg.Redis.Enabled = true
	cfg.Redis.CACert = "/no/such/ca.pem"
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "ca_cert") {
		t.Fatalf("a missing ca_cert: %v", err)
	}
}

// writeTestCA writes a self-signed CA certificate as PEM and returns its
// path.
func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "chok-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

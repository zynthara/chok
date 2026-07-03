package redis

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

func TestTLSConfigFor_Disabled(t *testing.T) {
	cfg, err := TLSConfigFor("127.0.0.1:6379", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatal("expected nil tls config when TLS disabled and no ca_cert")
	}
}

func TestTLSConfigFor_EnabledNoCA(t *testing.T) {
	cfg, err := TLSConfigFor("redis.example.com:25061", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil tls config")
	}
	if cfg.ServerName != "redis.example.com" {
		t.Fatalf("expected ServerName derived from addr, got %q", cfg.ServerName)
	}
	if cfg.RootCAs != nil {
		t.Fatal("expected nil RootCAs without ca_cert (falls back to system roots)")
	}
}

func TestTLSConfigFor_CACertImpliesTLS(t *testing.T) {
	// caCert set, useTLS false — the helper must still build a TLS config.
	cfg, err := TLSConfigFor("redis.example.com:25061", false, writeTestCA(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.RootCAs == nil {
		t.Fatal("expected tls config with RootCAs loaded from ca_cert")
	}
}

func TestTLSConfigFor_CACertMissing(t *testing.T) {
	if _, err := TLSConfigFor("r:6379", false, "/no/such/ca.pem"); err == nil {
		t.Fatal("expected error for missing ca_cert file")
	}
}

func TestTLSConfigFor_CACertNotPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := TLSConfigFor("r:6379", false, path); err == nil {
		t.Fatal("expected error for a ca_cert file without PEM certificates")
	}
}

func TestNew_MapsOptionsAndTLS(t *testing.T) {
	client, err := New(Options{
		Addr:     "redis.example.com:25061",
		Username: "acl-user",
		Password: "s3cret",
		DB:       3,
		TLS:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	got := client.Options()
	if got.Username != "acl-user" || got.Password != "s3cret" || got.DB != 3 {
		t.Fatalf("credentials not mapped: %+v", got)
	}
	if got.TLSConfig == nil || got.TLSConfig.ServerName != "redis.example.com" {
		t.Fatalf("expected TLS config with derived ServerName, got %+v", got.TLSConfig)
	}
	if got.PoolSize != 10 {
		t.Fatalf("expected PoolSize default 10, got %d", got.PoolSize)
	}
}

func TestNew_RejectsInvalidOptions(t *testing.T) {
	if _, err := New(Options{Addr: ""}); err == nil {
		t.Fatal("expected validation error for empty addr")
	}
	if _, err := New(Options{Addr: "r:6379", DialTimeout: -time.Second}); err == nil {
		t.Fatal("expected validation error for negative dial_timeout")
	}
}

func TestOptions_GoStringRedactsPassword(t *testing.T) {
	o := Options{Addr: "r:6379", Password: "super-secret"}
	for _, s := range []string{o.GoString(), o.String()} {
		if strings.Contains(s, "super-secret") {
			t.Fatalf("formatted output leaked the password: %s", s)
		}
	}
}

// writeTestCA generates a self-signed certificate, writes it as PEM to a temp
// file, and returns the path.
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
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	return path
}

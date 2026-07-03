package db

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
)

// writeTestCA writes a self-signed CA certificate PEM and returns its
// path — enough for mysqlTLSConfig's pool-building path (no live MySQL
// needed to verify registration mechanics).
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
	var buf strings.Builder
	if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMySQLTLSConfig_NoCA_PassesThroughTLSString(t *testing.T) {
	for _, mode := range []string{"", "true", "skip-verify", "preferred"} {
		got, err := mysqlTLSConfig(&MySQLOptions{Host: "h", TLS: mode})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if got != mode {
			t.Fatalf("mode %q must pass through, got %q", mode, got)
		}
	}
}

func TestMySQLTLSConfig_CACert_RegistersVerifyingConfig(t *testing.T) {
	ca := writeTestCA(t)
	host := "db-tls-test.internal"
	name, err := mysqlTLSConfig(&MySQLOptions{Host: host, TLS: "skip-verify", CACert: ca})
	if err != nil {
		t.Fatal(err)
	}
	// CACert takes precedence over the TLS string (toffs v0.4.2
	// semantics): the returned value is the per-host registration key,
	// not "skip-verify".
	if name != "chok-mysql-"+host {
		t.Fatalf("registration key must be per-host, got %q", name)
	}

	// The key must wire into the DSN — that's how the driver picks the
	// registered verifying config up.
	dsn := (&gomysql.Config{User: "u", Net: "tcp", Addr: "h:3306", DBName: "d", TLSConfig: name}).FormatDSN()
	if !strings.Contains(dsn, "tls=chok-mysql-"+host) {
		t.Fatalf("DSN must reference the registered TLS config, got %s", dsn)
	}

	// Re-registering the same host must be idempotent (in-process
	// restarts, repeated Opens).
	if _, err := mysqlTLSConfig(&MySQLOptions{Host: host, CACert: ca}); err != nil {
		t.Fatalf("re-register same host: %v", err)
	}
}

func TestMySQLTLSConfig_BadCA(t *testing.T) {
	if _, err := mysqlTLSConfig(&MySQLOptions{Host: "h", CACert: "/nonexistent/ca.pem"}); err == nil {
		t.Fatal("missing CA file must error")
	}
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mysqlTLSConfig(&MySQLOptions{Host: "h", CACert: garbage}); err == nil {
		t.Fatal("PEM-free CA file must error")
	}
}

var errAbort = errors.New("abort")

func TestOpen_SQLiteHandleWorks(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	ctx := context.Background()
	if err := h.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := h.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatalf("handle Migrate: %v", err)
	}
	if err := h.Unsafe(ctx).Create(&TestItem{Code: "H1"}).Error; err != nil {
		t.Fatalf("Unsafe create: %v", err)
	}

	// Unsafe must be transaction-aware: inside RunInTx it returns the
	// tx handle, so raw escape-hatch code joins the transaction and a
	// rollback takes its writes with it.
	err = h.RunInTx(ctx, func(txCtx context.Context) error {
		if err := h.Unsafe(txCtx).Create(&TestItem{Code: "H2"}).Error; err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("expected abort error, got %v", err)
	}
	var count int64
	h.Unsafe(ctx).Model(&TestItem{}).Count(&count)
	if count != 1 {
		t.Fatalf("Unsafe write inside rolled-back tx must vanish; rows=%d", count)
	}
}

func TestOpen_ValidatesFirst(t *testing.T) {
	if _, err := Open(Options{Driver: "sqlite"}); err == nil {
		t.Fatal("Open must validate options (empty sqlite path)")
	}
	if _, err := Open(Options{Driver: "oracle"}); err == nil {
		t.Fatal("Open must reject unknown drivers")
	}
}


// TestOpen_MemorySQLiteSurvivesConcurrentPoolUse pins the :memory:
// pool fix: without capping the pool to one immortal connection, every
// connection database/sql opens beyond the first is a fresh empty
// database, and concurrent use (an async sink goroutine next to the
// caller) intermittently fails with "no such table". Eight goroutines
// hammering queries force pool growth deterministically pre-fix.
func TestOpen_MemorySQLiteSurvivesConcurrentPoolUse(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ctx := context.Background()
	if err := h.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				if err := h.Unsafe(ctx).Create(&TestItem{Code: fmt.Sprintf("g%d-%d", g, i)}).Error; err != nil {
					errCh <- err
					return
				}
				var n int64
				if err := h.Unsafe(ctx).Model(&TestItem{}).Count(&n).Error; err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		t.Fatalf("concurrent pool use over :memory: must not lose the schema: %v", err)
	}
}

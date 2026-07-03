package parts

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/zynthara/chok/v2/audit"
	"github.com/zynthara/chok/v2/component"
	"github.com/zynthara/chok/v2/config"
)

type auditTestCfg struct {
	Audit *config.AuditOptions
}

func auditResolver(t *testing.T) (*AuditComponent, *DBComponent, *mockKernel) {
	t.Helper()
	dbc := NewDBComponent(func(component.Kernel) (*gorm.DB, error) {
		return gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	})
	if err := dbc.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	cfg := &auditTestCfg{Audit: &config.AuditOptions{
		Enabled:         true,
		AsyncBufferSize: 32,
		DropOnFull:      false,
		RetentionDays:   30,
		PurgeInterval:   24 * time.Hour,
		PurgeBatchSize:  500,
		EnableAdminAPI:  true,
	}}
	k := newMockKernel(cfg)
	k.store["db"] = dbc
	a := NewAuditComponent(func(c any) *config.AuditOptions { return c.(*auditTestCfg).Audit })
	return a, dbc, k
}

// TestAuditComponent_Init_AutoMigratesTable proves Init creates the
// audit_logs table — Migrate phase consumers (chok components that
// log on startup) can rely on the table existing.
func TestAuditComponent_Init_AutoMigratesTable(t *testing.T) {
	a, dbc, k := auditResolver(t)
	if err := a.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if !dbc.DB().Migrator().HasTable("audit_logs") {
		t.Fatal("Init should AutoMigrate audit_logs")
	}
	if a.Logger() == nil {
		t.Fatal("Logger() should be non-nil after enabled Init")
	}
}

// TestAuditComponent_Disabled_NoOp pins the disabled path: nil/
// disabled config short-circuits Init, Logger() returns nil,
// Close is a no-op, Health returns OK with no details.
func TestAuditComponent_Disabled_NoOp(t *testing.T) {
	dbc := NewDBComponent(func(component.Kernel) (*gorm.DB, error) {
		return gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	})
	if err := dbc.Init(context.Background(), newMockKernel(nil)); err != nil {
		t.Fatal(err)
	}
	cfg := &auditTestCfg{Audit: &config.AuditOptions{Enabled: false}}
	k := newMockKernel(cfg)
	k.store["db"] = dbc

	a := NewAuditComponent(func(c any) *config.AuditOptions { return c.(*auditTestCfg).Audit })
	if err := a.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	if a.Logger() != nil {
		t.Error("Logger() should be nil when disabled")
	}
	if dbc.DB().Migrator().HasTable("audit_logs") {
		t.Error("disabled Init should NOT AutoMigrate the table — operators expect off=fully off")
	}
	h := a.Health(context.Background())
	if h.Status != component.HealthOK {
		t.Errorf("disabled Health should be OK, got %q", h.Status)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("disabled Close should be nil, got %v", err)
	}
}

// TestAuditComponent_Init_RequiresDB pins the hard-dep contract:
// Init without a registered DBComponent must fail-fast with a
// readable error, not nil-deref.
func TestAuditComponent_Init_RequiresDB(t *testing.T) {
	cfg := &auditTestCfg{Audit: &config.AuditOptions{
		Enabled:         true,
		AsyncBufferSize: 32,
		RetentionDays:   30,
		PurgeInterval:   24 * time.Hour,
		PurgeBatchSize:  500,
	}}
	k := newMockKernel(cfg)
	a := NewAuditComponent(func(c any) *config.AuditOptions { return c.(*auditTestCfg).Audit })

	err := a.Init(context.Background(), k)
	if err == nil {
		t.Fatal("Init without DBComponent should fail")
	}
	if got := err.Error(); got == "" || !contains(got, "DBComponent not registered") {
		t.Errorf("error message should explain missing DB, got %q", got)
	}
}

// TestAuditComponent_LogRoundTrip — drive a Log through the
// component and verify it lands in the table after Close (which
// drains the sink).
func TestAuditComponent_LogRoundTrip(t *testing.T) {
	a, dbc, k := auditResolver(t)
	if err := a.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	a.Logger().Log(context.Background(), audit.Entry{
		ActorID:    "usr_alice",
		Action:     "task.create",
		Resource:   "task",
		ResourceID: "tsk_001",
	})

	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	var rows []audit.Log
	if err := dbc.DB().Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after Close drain, got %d", len(rows))
	}
}

// TestAuditComponent_Health_DegradedOnFailures — sink failures
// flip Health from OK to Degraded so /healthz operators see the
// problem without grepping logs.
func TestAuditComponent_Health_DegradedOnFailures(t *testing.T) {
	a, dbc, k := auditResolver(t)
	if err := a.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	// Drop the table so the next sink commit fails.
	if err := dbc.DB().Migrator().DropTable("audit_logs"); err != nil {
		t.Fatal(err)
	}

	// LogSync surfaces the failure synchronously and bumps Failed.
	if err := a.Logger().LogSync(context.Background(), audit.Entry{
		ActorID: "usr_alice",
		Action:  "task.create",
	}); err == nil {
		t.Fatal("LogSync should fail when audit_logs table missing")
	}

	h := a.Health(context.Background())
	if h.Status != component.HealthDegraded {
		t.Errorf("Health = %q, want %q after sink failures", h.Status, component.HealthDegraded)
	}
	if _, ok := h.Details["last_error"]; !ok {
		t.Errorf("Health Details should include last_error, got %+v", h.Details)
	}
}

// TestAuditComponent_Migrate_Idempotent — repeated Migrate calls
// don't error.
func TestAuditComponent_Migrate_Idempotent(t *testing.T) {
	a, _, k := auditResolver(t)
	if err := a.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	for range 3 {
		if err := a.Migrate(context.Background()); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
}

// TestAuditComponent_Reload_WarnsRestartOnly — changing
// AsyncBufferSize / DropOnFull at reload time should warn but not
// error; the live sink keeps using the old values until restart.
func TestAuditComponent_Reload_WarnsRestartOnly(t *testing.T) {
	a, _, k := auditResolver(t)
	if err := a.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	// Mutate restart-only fields.
	cfg := k.cfg.(*auditTestCfg)
	cfg.Audit.AsyncBufferSize = 8192
	cfg.Audit.DropOnFull = true

	if err := a.Reload(context.Background()); err != nil {
		t.Fatalf("Reload should not error on restart-only field changes, got %v", err)
	}
}

// TestAuditComponent_Reload_ReloadSafeFieldsApply — changing
// reload-safe fields applies; opts.Load() returns the new value
// (purge cron will pick it up in 7.D).
func TestAuditComponent_Reload_ReloadSafeFieldsApply(t *testing.T) {
	a, _, k := auditResolver(t)
	if err := a.Init(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	cfg := k.cfg.(*auditTestCfg)
	cfg.Audit.RetentionDays = 365
	cfg.Audit.PurgeInterval = 6 * time.Hour
	cfg.Audit.PurgeBatchSize = 200

	if err := a.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := a.opts.Load()
	if got.RetentionDays != 365 || got.PurgeInterval != 6*time.Hour || got.PurgeBatchSize != 200 {
		t.Errorf("Reload did not apply reload-safe fields: %+v", got)
	}
}

// contains is a tiny helper to keep the imports clean — strings
// would normally do this but the test file doesn't need a package
// import for one substring check.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

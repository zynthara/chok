package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"gorm.io/gorm"
)

func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if !metricHasLabels(metric, labels) {
				continue
			}
			switch {
			case metric.Gauge != nil:
				return metric.GetGauge().GetValue(), true
			case metric.Counter != nil:
				return metric.GetCounter().GetValue(), true
			case metric.Histogram != nil:
				return float64(metric.GetHistogram().GetSampleCount()), true
			}
		}
	}
	return 0, false
}

func metricHasLabels(metric *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(metric.Label))
	for _, pair := range metric.Label {
		got[pair.GetName()] = pair.GetValue()
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

func TestDBMetrics_MultipleInstancesQueriesAndPools(t *testing.T) {
	ctx := context.Background()
	defaultDB, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "default.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = defaultDB.Close() })
	readDB, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readDB.Close() })

	reg := prometheus.NewRegistry()
	defaultMetrics, err := newDBMetrics(reg, defaultDB, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer defaultMetrics.close()
	readMetrics, err := newDBMetrics(reg, readDB, "read")
	if err != nil {
		t.Fatal(err)
	}
	defer readMetrics.close()
	if err := enableQueryMetrics(defaultDB.gdb, defaultMetrics); err != nil {
		t.Fatal(err)
	}
	if err := enableQueryMetrics(readDB.gdb, readMetrics); err != nil {
		t.Fatal(err)
	}

	if err := defaultDB.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}
	if err := defaultDB.Unsafe(ctx).Create(&TestItem{Code: "observed"}).Error; err != nil {
		t.Fatal(err)
	}
	var missing TestItem
	if err := defaultDB.Unsafe(ctx).Where("code = ?", "missing").First(&missing).Error; err != gorm.ErrRecordNotFound {
		t.Fatalf("not-found precondition: %v", err)
	}
	if err := defaultDB.Unsafe(ctx).Exec("SELECT * FROM table_that_does_not_exist").Error; err == nil {
		t.Fatal("bad SQL must fail")
	}

	if value, ok := metricValue(t, reg, "db_query_duration_seconds", map[string]string{"instance": "default", "op": "create"}); !ok || value < 1 {
		t.Fatalf("create duration count = %v, present=%v", value, ok)
	}
	if value, ok := metricValue(t, reg, "db_query_errors_total", map[string]string{"instance": "default", "op": "raw"}); !ok || value != 1 {
		t.Fatalf("raw errors = %v, present=%v, want 1", value, ok)
	}
	if value, ok := metricValue(t, reg, "db_query_errors_total", map[string]string{"instance": "default", "op": "query"}); ok && value != 0 {
		t.Fatalf("record-not-found incremented query errors: %v", value)
	}

	for _, labels := range []map[string]string{
		{"instance": "default", "pool": "primary", "state": "open"},
		{"instance": "default", "pool": "read", "state": "open"},
		{"instance": "read", "pool": "primary", "state": "open"},
	} {
		if _, ok := metricValue(t, reg, "db_pool_connections", labels); !ok {
			t.Fatalf("pool series missing: %v", labels)
		}
	}
}

func TestMigrationMetrics_RefreshDirtyStateAfterStartup(t *testing.T) {
	ctx := context.Background()
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	migrations := fstest.MapFS{
		"0001_items.sql": &fstest.MapFile{Data: []byte("CREATE TABLE observed_items (id INTEGER PRIMARY KEY);")},
	}
	if _, err := ApplyMigrations(ctx, h, migrations); err != nil {
		t.Fatal(err)
	}

	reg := prometheus.NewRegistry()
	m, err := newDBMetrics(reg, h, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	files, err := LoadMigrations(migrations)
	if err != nil {
		t.Fatal(err)
	}
	m.setExpectedMigrationVersion(files)
	if err := m.observeMigrationStatus(ctx, h, migrations); err != nil {
		t.Fatal(err)
	}
	monitor := startMigrationMonitor(ctx, 5*time.Millisecond, h, migrations, m, nopKernelLogger{})
	defer monitor.close(context.Background())

	if err := h.Unsafe(ctx).Exec(
		"INSERT INTO schema_migrations (version, name, checksum, started_at, dirty, last_error) VALUES (?, ?, ?, CURRENT_TIMESTAMP, ?, ?)",
		2, "external", strings.Repeat("a", 64), true, "external failure",
	).Error; err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if value, ok := metricValue(t, reg, "db_migrations_dirty", map[string]string{"instance": "default"}); ok && value == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic migration metrics never observed the external dirty row")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if value, ok := metricValue(t, reg, "db_migration_expected_version", map[string]string{"instance": "default"}); !ok || value != 1 {
		t.Fatalf("expected version = %v, present=%v", value, ok)
	}
	if value, ok := metricValue(t, reg, "db_migration_applied_version", map[string]string{"instance": "default"}); !ok || value != 1 {
		t.Fatalf("applied version = %v, present=%v", value, ok)
	}
}

func TestSQLiteMaintenance_ExportsClassifiedResult(t *testing.T) {
	ctx := context.Background()
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "maintenance.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	reg := prometheus.NewRegistry()
	m, err := newDBMetrics(reg, h, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()

	maintenance := newSQLiteMaintenance(ctx, h, &SQLiteOptions{OptimizeInterval: time.Hour}, nopKernelLogger{}, "default")
	maintenance.onTick = m.observeMaintenance
	maintenance.optimize(ctx)
	if value, ok := metricValue(t, reg, "db_sqlite_maintenance_runs_total", map[string]string{
		"instance": "default", "job": "optimize", "result": maintenanceOK,
	}); !ok || value != 1 {
		t.Fatalf("maintenance ok count = %v, present=%v", value, ok)
	}
}

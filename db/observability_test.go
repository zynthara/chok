package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/conf"
	"github.com/zynthara/chok/v2/kernel"
	choklog "github.com/zynthara/chok/v2/log"
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

type capturedLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *capturedLog) add(msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, fmt.Sprint(append([]any{msg}, kv...)...))
}

func (l *capturedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.entries, "\n")
}

func (l *capturedLog) Debug(msg string, kv ...any) { l.add(msg, kv...) }
func (l *capturedLog) Info(msg string, kv ...any)  { l.add(msg, kv...) }
func (l *capturedLog) Warn(msg string, kv ...any)  { l.add(msg, kv...) }
func (l *capturedLog) Error(msg string, kv ...any) { l.add(msg, kv...) }
func (l *capturedLog) DebugContext(_ context.Context, msg string, kv ...any) {
	l.add(msg, kv...)
}
func (l *capturedLog) InfoContext(_ context.Context, msg string, kv ...any) {
	l.add(msg, kv...)
}
func (l *capturedLog) WarnContext(_ context.Context, msg string, kv ...any) {
	l.add(msg, kv...)
}
func (l *capturedLog) ErrorContext(_ context.Context, msg string, kv ...any) {
	l.add(msg, kv...)
}
func (l *capturedLog) With(kv ...any) choklog.Logger {
	return &capturedLogWith{parent: l, prefix: kv}
}
func (l *capturedLog) SetLevel(string) error { return nil }

type capturedLogWith struct {
	parent *capturedLog
	prefix []any
}

func (l *capturedLogWith) values(kv []any) []any       { return append(append([]any{}, l.prefix...), kv...) }
func (l *capturedLogWith) Debug(msg string, kv ...any) { l.parent.Debug(msg, l.values(kv)...) }
func (l *capturedLogWith) Info(msg string, kv ...any)  { l.parent.Info(msg, l.values(kv)...) }
func (l *capturedLogWith) Warn(msg string, kv ...any)  { l.parent.Warn(msg, l.values(kv)...) }
func (l *capturedLogWith) Error(msg string, kv ...any) { l.parent.Error(msg, l.values(kv)...) }
func (l *capturedLogWith) DebugContext(ctx context.Context, msg string, kv ...any) {
	l.parent.DebugContext(ctx, msg, l.values(kv)...)
}
func (l *capturedLogWith) InfoContext(ctx context.Context, msg string, kv ...any) {
	l.parent.InfoContext(ctx, msg, l.values(kv)...)
}
func (l *capturedLogWith) WarnContext(ctx context.Context, msg string, kv ...any) {
	l.parent.WarnContext(ctx, msg, l.values(kv)...)
}
func (l *capturedLogWith) ErrorContext(ctx context.Context, msg string, kv ...any) {
	l.parent.ErrorContext(ctx, msg, l.values(kv)...)
}
func (l *capturedLogWith) With(kv ...any) choklog.Logger {
	return &capturedLogWith{parent: l.parent, prefix: l.values(kv)}
}
func (l *capturedLogWith) SetLevel(string) error { return nil }

func TestGORMLogger_ParameterizedAndThresholdSemantics(t *testing.T) {
	ctx := context.Background()
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: ":memory:"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if err := h.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}

	secret := "secret-that-must-not-enter-logs"
	logger := &capturedLog{}
	h.gdb.Logger = newGORMLogger(logger, time.Nanosecond)
	if err := h.Unsafe(ctx).Create(&TestItem{Code: secret}).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.Unsafe(ctx).Exec("SELECT * FROM missing_table WHERE token = ?", secret).Error; err == nil {
		t.Fatal("bad SQL must fail")
	}
	if got := logger.String(); strings.Contains(got, secret) {
		t.Fatalf("query parameter leaked into module-managed SQL log: %s", got)
	} else if !strings.Contains(got, "?") {
		t.Fatalf("parameterized SQL structure was not retained in log: %s", got)
	}

	silent := &capturedLog{}
	h.gdb.Logger = newGORMLogger(silent, 0)
	var missing TestItem
	if err := h.Unsafe(ctx).Where("code = ?", "absent").First(&missing).Error; err != gorm.ErrRecordNotFound {
		t.Fatalf("not-found precondition: %v", err)
	}
	if got := silent.String(); got != "" {
		t.Fatalf("threshold 0 or record-not-found produced a log: %s", got)
	}
	if err := h.Unsafe(ctx).Exec("SELECT * FROM another_missing_table").Error; err == nil {
		t.Fatal("bad SQL must fail")
	}
	if got := silent.String(); !strings.Contains(got, "gorm") {
		t.Fatalf("threshold 0 must not disable error logs: %s", got)
	}
}

func TestModule_InstallsParameterizedSlowQueryLogger(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "chok.yaml")
	if err := os.WriteFile(configPath, []byte(`
db:
  driver: sqlite
  slow_threshold: 1ns
  sqlite:
    path: ":memory:"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	loader := conf.NewLoader("observability", "OBSERVABILITY")
	loader.SetPath(configPath)
	if err := loader.Register("db", Options{}); err != nil {
		t.Fatal(err)
	}
	store, err := conf.NewStore(loader)
	if err != nil {
		t.Fatal(err)
	}
	logger := &capturedLog{}
	registry, err := kernel.New(kernel.Config{
		Store: store, Logger: logger,
		Components: []kernel.Component{Module(WithTables(Table(&TestItem{})))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Stop(context.Background()) })

	component, ok := kernel.Get[*Component](registry, "db")
	if !ok {
		t.Fatal("managed db component unavailable")
	}
	secret := "module-secret-that-must-not-enter-logs"
	if err := component.Handle().Unsafe(context.Background()).Create(&TestItem{Code: secret}).Error; err != nil {
		t.Fatal(err)
	}
	got := logger.String()
	if strings.Contains(got, secret) {
		t.Fatalf("module-managed logger leaked query parameter: %s", got)
	}
	if !strings.Contains(got, "gorm slow query") {
		t.Fatalf("module did not install configured slow-query logger: %s", got)
	}
}

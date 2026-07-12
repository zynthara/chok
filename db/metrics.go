package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/internal/promutil"
	"github.com/zynthara/chok/v2/kernel"
)

const migrationStatusQueryTimeout = 5 * time.Second

// dbMetrics contains the shared metric vectors plus the one collector owned by
// this database instance. Vecs are registered-or-reused because named database
// components share the process registry; the pool collector uses instance as a
// const label, giving every component a distinct Prometheus collector ID.
type dbMetrics struct {
	instance string

	queryDuration *prometheus.HistogramVec
	queryErrors   *prometheus.CounterVec

	migrationExpected *prometheus.GaugeVec
	migrationApplied  *prometheus.GaugeVec
	migrationsApplied *prometheus.GaugeVec
	migrationsPending *prometheus.GaugeVec
	migrationsDirty   *prometheus.GaugeVec

	maintenanceRuns *prometheus.CounterVec

	registry      *prometheus.Registry
	poolCollector *poolCollector
}

func newDBMetrics(reg *prometheus.Registry, h *DB, instance string) (*dbMetrics, error) {
	m := &dbMetrics{instance: instance, registry: reg}
	var errs []error

	m.queryDuration, _ = registerHistogramVec(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "db_query_duration_seconds",
		Help:    "Database operation duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"instance", "op"}), &errs)
	m.queryErrors, _ = registerCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "db_query_errors_total",
		Help: "Total database operation errors, excluding record-not-found results.",
	}, []string{"instance", "op"}), &errs)

	m.migrationExpected, _ = registerGaugeVec(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "db_migration_expected_version",
		Help: "Highest migration version embedded in this process.",
	}, []string{"instance", "sequence"}), &errs)
	m.migrationApplied, _ = registerGaugeVec(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "db_migration_applied_version",
		Help: "Highest clean migration version currently observed in the database.",
	}, []string{"instance", "sequence"}), &errs)
	m.migrationsApplied, _ = registerGaugeVec(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "db_migrations_applied",
		Help: "Number of clean migration ledger entries currently observed.",
	}, []string{"instance", "sequence"}), &errs)
	m.migrationsPending, _ = registerGaugeVec(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "db_migrations_pending",
		Help: "Number of embedded migrations not yet applied to the database.",
	}, []string{"instance", "sequence"}), &errs)
	m.migrationsDirty, _ = registerGaugeVec(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "db_migrations_dirty",
		Help: "Number of dirty migration attempts currently observed in the database.",
	}, []string{"instance", "sequence"}), &errs)

	m.maintenanceRuns, _ = registerCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "db_sqlite_maintenance_runs_total",
		Help: "Total SQLite maintenance runs by job and result.",
	}, []string{"instance", "job", "result"}), &errs)

	primary, err := h.gdb.DB()
	if err != nil {
		errs = append(errs, fmt.Errorf("db metrics: primary pool: %w", err))
	} else {
		m.poolCollector = newPoolCollector(instance, primary, h.readPool)
		if err := reg.Register(m.poolCollector); err != nil {
			errs = append(errs, fmt.Errorf("db metrics: register pool collector: %w", err))
			m.poolCollector = nil
		}
	}

	if err := errors.Join(errs...); err != nil {
		return m, err
	}
	return m, nil
}

func registerCounterVec(reg prometheus.Registerer, v *prometheus.CounterVec, errs *[]error) (*prometheus.CounterVec, error) {
	got, err := promutil.RegisterOrReuseCounterVec(reg, v)
	if err != nil {
		*errs = append(*errs, err)
	}
	return got, err
}

func registerHistogramVec(reg prometheus.Registerer, v *prometheus.HistogramVec, errs *[]error) (*prometheus.HistogramVec, error) {
	got, err := promutil.RegisterOrReuseHistogramVec(reg, v)
	if err != nil {
		*errs = append(*errs, err)
	}
	return got, err
}

func registerGaugeVec(reg prometheus.Registerer, v *prometheus.GaugeVec, errs *[]error) (*prometheus.GaugeVec, error) {
	got, err := promutil.RegisterOrReuseGaugeVec(reg, v)
	if err != nil {
		*errs = append(*errs, err)
	}
	return got, err
}

func (m *dbMetrics) close() {
	if m != nil && m.poolCollector != nil {
		m.registry.Unregister(m.poolCollector)
		m.poolCollector = nil
	}
}

func (m *dbMetrics) observeMaintenance(job, result string) {
	if m != nil {
		m.maintenanceRuns.WithLabelValues(m.instance, job, result).Inc()
	}
}

func (m *dbMetrics) setExpectedMigrationVersion(sequence string, files []Migration) {
	var maxVersion int64
	for _, migration := range files {
		if migration.Version > maxVersion {
			maxVersion = migration.Version
		}
	}
	m.migrationExpected.WithLabelValues(m.instance, sequence).Set(float64(maxVersion))
}

func (m *dbMetrics) observeMigrationStatus(sequence string, st *MigrationStatus) {
	var maxApplied int64
	for _, applied := range st.Applied {
		if applied.Version > maxApplied {
			maxApplied = applied.Version
		}
	}
	m.migrationApplied.WithLabelValues(m.instance, sequence).Set(float64(maxApplied))
	m.migrationsApplied.WithLabelValues(m.instance, sequence).Set(float64(len(st.Applied)))
	m.migrationsPending.WithLabelValues(m.instance, sequence).Set(float64(len(st.Pending)))
	m.migrationsDirty.WithLabelValues(m.instance, sequence).Set(float64(len(st.Dirty)))
}

// enableQueryMetrics mirrors the tracing callback shape. It deliberately
// measures the GORM operation around the actual SQL callback rather than model
// hooks, and records Raw/Row operations in addition to CRUD callbacks.
func enableQueryMetrics(gdb *gorm.DB, m *dbMetrics) error {
	before := func(op string) func(*gorm.DB) {
		return func(tx *gorm.DB) {
			tx.InstanceSet("chok:metrics:start:"+op, time.Now())
		}
	}
	after := func(op string) func(*gorm.DB) {
		return func(tx *gorm.DB) {
			value, ok := tx.InstanceGet("chok:metrics:start:" + op)
			if !ok {
				return
			}
			started, ok := value.(time.Time)
			if !ok {
				return
			}
			m.queryDuration.WithLabelValues(m.instance, op).Observe(time.Since(started).Seconds())
			if tx.Error != nil && !errors.Is(tx.Error, gorm.ErrRecordNotFound) {
				m.queryErrors.WithLabelValues(m.instance, op).Inc()
			}
		}
	}

	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(gdb.Callback().Create().Before("gorm:create").Register("chok:metrics:before_create", before("create")))
	add(gdb.Callback().Create().After("gorm:create").Register("chok:metrics:after_create", after("create")))
	add(gdb.Callback().Query().Before("gorm:query").Register("chok:metrics:before_query", before("query")))
	add(gdb.Callback().Query().After("gorm:query").Register("chok:metrics:after_query", after("query")))
	add(gdb.Callback().Update().Before("gorm:update").Register("chok:metrics:before_update", before("update")))
	add(gdb.Callback().Update().After("gorm:update").Register("chok:metrics:after_update", after("update")))
	add(gdb.Callback().Delete().Before("gorm:delete").Register("chok:metrics:before_delete", before("delete")))
	add(gdb.Callback().Delete().After("gorm:delete").Register("chok:metrics:after_delete", after("delete")))
	add(gdb.Callback().Row().Before("gorm:row").Register("chok:metrics:before_row", before("row")))
	add(gdb.Callback().Row().After("gorm:row").Register("chok:metrics:after_row", after("row")))
	add(gdb.Callback().Raw().Before("gorm:raw").Register("chok:metrics:before_raw", before("raw")))
	add(gdb.Callback().Raw().After("gorm:raw").Register("chok:metrics:after_raw", after("raw")))
	return errors.Join(errs...)
}

type poolCollector struct {
	connections *prometheus.Desc
	waitTotal   *prometheus.Desc
	waitSeconds *prometheus.Desc
	pools       map[string]*sql.DB
}

func newPoolCollector(instance string, primary, read *sql.DB) *poolCollector {
	labels := prometheus.Labels{"instance": instance}
	pools := map[string]*sql.DB{"primary": primary}
	if read != nil {
		pools["read"] = read
	}
	return &poolCollector{
		connections: prometheus.NewDesc("db_pool_connections", "Database pool connections by state.", []string{"pool", "state"}, labels),
		waitTotal:   prometheus.NewDesc("db_pool_wait_total", "Total connection requests that waited for an available connection.", []string{"pool"}, labels),
		waitSeconds: prometheus.NewDesc("db_pool_wait_seconds_total", "Total time blocked waiting for an available connection.", []string{"pool"}, labels),
		pools:       pools,
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.connections
	ch <- c.waitTotal
	ch <- c.waitSeconds
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	for name, pool := range c.pools {
		stats := pool.Stats()
		ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(stats.OpenConnections), name, "open")
		ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(stats.InUse), name, "in_use")
		ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(stats.Idle), name, "idle")
		ch <- prometheus.MustNewConstMetric(c.waitTotal, prometheus.CounterValue, float64(stats.WaitCount), name)
		ch <- prometheus.MustNewConstMetric(c.waitSeconds, prometheus.CounterValue, stats.WaitDuration.Seconds(), name)
	}
}

type migrationMonitor struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func startMigrationMonitor(parent context.Context, interval time.Duration, sample func(context.Context) error, metrics *dbMetrics, logger kernel.Logger) *migrationMonitor {
	if interval <= 0 || metrics == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	monitor := &migrationMonitor{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(monitor.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		failing := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				queryCtx, queryCancel := context.WithTimeout(ctx, migrationStatusQueryTimeout)
				err := sample(queryCtx)
				queryCancel()
				if err != nil && !failing {
					logger.Warn("db: migration metrics refresh failed", "instance", metrics.instance, "error", err)
				}
				if err == nil && failing {
					logger.Info("db: migration metrics refresh recovered", "instance", metrics.instance)
				}
				failing = err != nil
			}
		}
	}()
	return monitor
}

func (m *migrationMonitor) close(ctx context.Context) {
	if m == nil {
		return
	}
	m.once.Do(m.cancel)
	select {
	case <-m.done:
	case <-ctx.Done():
	}
}

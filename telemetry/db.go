package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbx"
)

// dbMetrics are the database client metrics, named after the
// OpenTelemetry database semantic conventions (db.client.operation.duration),
// plus the count of slow statements.
type dbMetrics struct {
	duration *prometheus.HistogramVec
	slow     *prometheus.CounterVec
}

func newDBMetrics() *dbMetrics {
	return &dbMetrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "db_client_operation_duration_seconds",
			Help: "Duration of database statements (OpenTelemetry db.client.operation.duration).",
			// The bucket boundaries the semantic conventions advise.
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10},
		}, []string{"db_system_name", "db_operation_name", "db_collection_name", "error_type"}),
		slow: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "db_client_slow_operations_total",
			Help: "Database statements that took db.slow_threshold or longer.",
		}, []string{"db_system_name", "db_operation_name", "db_collection_name"}),
	}
}

func (m *dbMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.duration, m.slow}
}

// Instrument observes every SQL statement d runs: a client span named
// "{operation} {table}", such as "SELECT posts", carrying the SQL as gorm
// built it, with placeholders for the bound values (the text of the
// slow-query log), and its outcome; the
// statement's duration in db_client_operation_duration_seconds, by
// operation, table and error type (canceled, timeout or error; a missing
// record is not an error); and db_client_slow_operations_total, counting
// the statements that took db.slow_threshold or longer, the threshold of
// the slow-query log (0 counts none). d's connection pools are exported
// as the go_sql_* metrics, labelled db_name with the dialect (a SQLite
// file's read pool with _read; a second handle of the same dialect with
// _2, and so on).
//
// Instrument may be called at any time, even while d is in use; calling
// it again for d does nothing. With tracing and metrics off it records
// nothing.
func (t *Telemetry) Instrument(d *db.DB) {
	if d == nil {
		return
	}
	if t.db == nil && t.sdk == nil {
		return // nothing to record
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dbs[d] {
		return
	}
	t.dbs[d] = true
	o := &dbObserver{t: t}
	desc := dbx.Observe(d, o)
	o.system, o.slow = systemName(desc.Driver), desc.SlowThreshold
	if t.db == nil {
		return
	}
	t.registerPool(desc.Driver, desc.Primary)
	if desc.Readers != nil {
		t.registerPool(desc.Driver+"_read", desc.Readers)
	}
}

// registerPool exports a pool's statistics under the first free db_name
// derived from name. Called with t.mu held.
func (t *Telemetry) registerPool(name string, pool *sql.DB) {
	if pool == nil {
		return
	}
	id := name
	for i := 2; t.poolIDs[id]; i++ {
		id = name + "_" + strconv.Itoa(i)
	}
	if err := t.reg.Register(collectors.NewDBStatsCollector(pool, id)); err != nil {
		t.logger.Warn("telemetry: the connection pool metrics are not exported", "db_name", id, "error", err)
		return
	}
	t.poolIDs[id] = true
}

// dbObserver is the statement observer Instrument attaches.
type dbObserver struct {
	t      *Telemetry
	system string // db.system.name
	slow   time.Duration
}

const stmtKey = "chok:telemetry"

type stmtState struct {
	begin time.Time
	span  trace.Span
}

func (o *dbObserver) Before(tx *gorm.DB, op string) {
	st := tx.Statement
	if st == nil || st.Context == nil {
		return
	}
	ctx, span := o.t.tracer.Start(st.Context, op,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(semconv.DBSystemNameKey.String(o.system)))
	st.Context = ctx
	tx.InstanceSet(stmtKey, &stmtState{begin: time.Now(), span: span})
}

func (o *dbObserver) After(tx *gorm.DB, op string) {
	v, ok := tx.InstanceGet(stmtKey)
	if !ok {
		return
	}
	s, ok := v.(*stmtState)
	if !ok {
		return
	}
	elapsed := time.Since(s.begin)
	sql := tx.Statement.SQL.String()
	operation := sqlOperation(sql, op)
	table := tx.Statement.Table
	errType := dbErrorType(tx.Error)

	if m := o.t.db; m != nil {
		m.duration.WithLabelValues(o.system, operation, table, errType).Observe(elapsed.Seconds())
		if o.slow > 0 && elapsed >= o.slow {
			m.slow.WithLabelValues(o.system, operation, table).Inc()
		}
	}

	if !s.span.IsRecording() {
		s.span.End()
		return
	}
	name := operation
	if table != "" {
		name += " " + table
	}
	s.span.SetName(name)
	attrs := []attribute.KeyValue{semconv.DBOperationName(operation)}
	if table != "" {
		attrs = append(attrs, semconv.DBCollectionName(table))
	}
	if sql != "" {
		attrs = append(attrs, semconv.DBQueryText(sql))
	}
	if operation == "SELECT" && tx.Error == nil {
		attrs = append(attrs, semconv.DBResponseReturnedRows(int(tx.RowsAffected)))
	}
	if errType != "" {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errType))
		s.span.SetStatus(codes.Error, tx.Error.Error())
	}
	s.span.SetAttributes(attrs...)
	s.span.End()
}

// systemName is db.system.name for a dialect.
func systemName(driver string) string {
	if driver == "postgres" {
		return "postgresql"
	}
	return driver // sqlite, mysql
}

// opNames stand in for the SQL keyword when a statement failed before it
// was built.
var opNames = map[string]string{"create": "INSERT", "query": "SELECT", "update": "UPDATE", "delete": "DELETE", "row": "SELECT", "raw": "EXEC"}

// sqlOperation is the statement's leading keyword, upper-cased: a label
// whose values the SQL grammar bounds.
func sqlOperation(sql, op string) string {
	sql = strings.TrimLeft(sql, " \t\r\n(")
	end := strings.IndexFunc(sql, func(r rune) bool { return !unicode.IsLetter(r) })
	if end < 0 {
		end = len(sql)
	}
	if end == 0 || end > 16 {
		if name, ok := opNames[op]; ok {
			return name
		}
		return "OTHER"
	}
	return strings.ToUpper(sql[:end])
}

// dbErrorType classifies a statement's error for the error_type label: ""
// when it succeeded or found no record.
func dbErrorType(err error) string {
	switch {
	case err == nil, errors.Is(err, gorm.ErrRecordNotFound):
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "error"
}

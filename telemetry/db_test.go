package telemetry

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"gorm.io/gorm"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
)

type note struct {
	db.Model
	Title string `gorm:"size:100;not null"`
}

func (note) RIDPrefix() string { return "nte" }

// openNotes opens a fresh database of the lane's dialect with the notes
// table and the given slow-query threshold.
func openNotes(t *testing.T, slow time.Duration) *db.DB {
	t.Helper()
	cfg := dbtest.Config(t)
	cfg.SlowThreshold = slow
	return dbtest.OpenConfig(t, cfg, db.Tables(db.Table(&note{})))
}

func spanNamed(spans *tracetest.InMemoryExporter, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans.GetSpans() {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

// TestInstrument_SpansAndDurations: every statement is a client span
// named after its operation and table, a child of the request's span,
// and a sample of the duration histogram; a missing record is not an
// error, a failed statement is.
func TestInstrument_SpansAndDurations(t *testing.T) {
	tel, spans := tracing(t, metricsOn())
	d := openNotes(t, 0)
	tel.Instrument(d)
	tel.Instrument(d) // a second call changes nothing
	system := systemName(dbtest.Driver())

	ctx, parent := tel.TracerProvider().Tracer("test").Start(t.Context(), "request")
	gdb := d.Unsafe(ctx)
	if err := gdb.Create(&note{Title: "first"}).Error; err != nil {
		t.Fatal(err)
	}
	var found []note
	if err := gdb.Where("title = ?", "first").Find(&found).Error; err != nil || len(found) != 1 {
		t.Fatalf("find: %v %v", found, err)
	}
	if err := gdb.First(&note{}, 999999).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("missing record: %v", err)
	}
	if err := gdb.Exec("UPDATE no_such_table SET x = 1").Error; err == nil {
		t.Fatal("a statement on a missing table fails")
	}
	parent.End()

	insert, ok := spanNamed(spans, "INSERT notes")
	if !ok {
		t.Fatalf("no INSERT notes span in %v", spans.GetSpans())
	}
	attrs := spanAttrs(insert)
	if insert.Parent.SpanID() != parent.SpanContext().SpanID() || insert.SpanKind.String() != "client" ||
		attrs["db.system.name"] != system || attrs["db.operation.name"] != "INSERT" || attrs["db.collection.name"] != "notes" ||
		!strings.Contains(attrs["db.query.text"], "notes") || strings.Contains(attrs["db.query.text"], "first") {
		t.Fatalf("INSERT span: parent %s, kind %s, attributes %v", insert.Parent.SpanID(), insert.SpanKind, attrs)
	}
	sel, ok := spanNamed(spans, "SELECT notes")
	if !ok || spanAttrs(sel)["db.response.returned_rows"] == "" {
		t.Fatalf("SELECT span with the rows it returned: %v", spanAttrs(sel))
	}
	failed, ok := spanNamed(spans, "UPDATE")
	if !ok || failed.Status.Code != codes.Error || spanAttrs(failed)["error.type"] != "error" {
		t.Fatalf("the failed statement's span: %v %v", failed.Status, spanAttrs(failed))
	}

	reg := tel.Registry()
	for _, tc := range []struct {
		op, table, errType string
		want               float64
	}{
		{"INSERT", "notes", "", 1},
		{"SELECT", "notes", "", 2}, // the Find, and the First that found nothing
		{"UPDATE", "", "error", 1},
	} {
		n, ok := metric(t, reg, "db_client_operation_duration_seconds", map[string]string{
			"db_system_name": system, "db_operation_name": tc.op, "db_collection_name": tc.table, "error_type": tc.errType,
		})
		if !ok || n != tc.want {
			t.Fatalf("%s %s %q: %v statements (found %v), want %v", tc.op, tc.table, tc.errType, n, ok, tc.want)
		}
	}
	if _, ok := metric(t, reg, "db_client_slow_operations_total", nil); ok {
		t.Fatal("db.slow_threshold 0 counts no statement as slow")
	}
}

// TestInstrument_FailedStatusNamesNoValue: a failed statement's span
// words its error as the statement log does, by driver, code and names:
// the value the driver's message quotes (Postgres's invalid input syntax
// for type integer "x", MySQL's Incorrect integer value 'x', SQLite's
// bad JSON path 'x') stays out of the trace.
func TestInstrument_FailedStatusNamesNoValue(t *testing.T) {
	tel, spans := tracing(t, metricsOn())
	d := openNotes(t, 0)
	tel.Instrument(d)
	gdb := d.Unsafe(t.Context())
	const secret = "s3cret-value"
	// SQLite keeps text in an integer column; it quotes a JSON path it
	// refuses instead.
	query, name := "INSERT INTO traced_numbers (n) VALUES (?)", "INSERT"
	if dbtest.Driver() == "sqlite" {
		query, name = "SELECT json_extract('{}', ?)", "SELECT"
	} else if err := gdb.Exec("CREATE TABLE traced_numbers (n integer)").Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(query, secret).Error; err == nil {
		t.Fatalf("%s refuses %q on %s", query, secret, dbtest.Driver())
	}
	want := map[string]string{"sqlite": "sqlite 1", "postgres": "postgres 22P02", "mysql": "mysql 1366 (HY000)"}[dbtest.Driver()]
	failed, ok := spanNamed(spans, name)
	if !ok || failed.Status.Code != codes.Error || failed.Status.Description != want {
		t.Fatalf("the failed statement's status on %s: %+v, want %q", dbtest.Driver(), failed.Status, want)
	}
}

// TestPhase_FailedStatusNamesNoValue: a transaction phase that fails, a
// COMMIT a deferred constraint trigger refuses with the row's value in
// its message, words its error the same way.
func TestPhase_FailedStatusNamesNoValue(t *testing.T) {
	tel, spans := tracing(t, metricsOn())
	o := &dbObserver{t: tel, system: "postgresql"}
	o.Phase(t.Context(), "COMMIT", time.Now(), &pgconn.PgError{Severity: "ERROR", Code: "P0001", Message: "booking s3cret-value overlaps"})
	commit, ok := spanNamed(spans, "COMMIT")
	if !ok || commit.Status.Code != codes.Error || commit.Status.Description != "postgres P0001" {
		t.Fatalf("the failed COMMIT's status: %+v", commit.Status)
	}
}

// TestInstrument_SlowStatementsAtTheThreshold: a statement at or over
// db.slow_threshold counts as slow, one under it does not. The
// thresholds are far apart (1ns, 1h) so the outcome cannot depend on the
// machine's speed.
func TestInstrument_SlowStatementsAtTheThreshold(t *testing.T) {
	tel := newTel(t, metricsOn())
	fast := openNotes(t, time.Hour)       // every statement is under it
	slow := openNotes(t, time.Nanosecond) // every statement is over it
	tel.Instrument(fast)
	tel.Instrument(slow)
	system := systemName(dbtest.Driver())
	labels := map[string]string{"db_system_name": system, "db_operation_name": "SELECT", "db_collection_name": "notes"}

	if err := fast.Unsafe(t.Context()).Find(&[]note{}).Error; err != nil {
		t.Fatal(err)
	}
	if n, ok := metric(t, tel.Registry(), "db_client_slow_operations_total", labels); ok || n != 0 {
		t.Fatalf("a statement under the threshold: %v", n)
	}
	for range 2 {
		if err := slow.Unsafe(t.Context()).Find(&[]note{}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if n, ok := metric(t, tel.Registry(), "db_client_slow_operations_total", labels); !ok || n != 2 {
		t.Fatalf("the statements over the threshold: %v (found %v), want 2", n, ok)
	}
	if n, _ := metric(t, tel.Registry(), "db_client_operation_duration_seconds", labels); n != 3 {
		t.Fatalf("all three are in the histogram: %v", n)
	}
}

// TestInstrument_ExportsThePools: the connection pools of each
// instrumented handle are go_sql_* series, a second handle of the same
// dialect under its own db_name.
func TestInstrument_ExportsThePools(t *testing.T) {
	tel := newTel(t, metricsOn())
	tel.Instrument(openNotes(t, 0))
	tel.Instrument(openNotes(t, 0))
	driver := dbtest.Driver()
	want := []string{driver, driver + "_2"}
	if driver == "sqlite" {
		want = append(want, "sqlite_read", "sqlite_read_2")
	}
	for _, name := range want {
		if _, ok := metric(t, tel.Registry(), "go_sql_max_open_connections", map[string]string{"db_name": name}); !ok {
			t.Fatalf("no go_sql_* series for db_name %q", name)
		}
	}
}

// TestInstrument_WithEverythingOff records nothing and does not fail.
func TestInstrument_WithEverythingOff(t *testing.T) {
	tel := newTel(t, Config{})
	d := openNotes(t, time.Nanosecond)
	tel.Instrument(d)
	tel.Instrument(nil)
	if err := d.Unsafe(t.Context()).Find(&[]note{}).Error; err != nil {
		t.Fatal(err)
	}
	if fams, _ := tel.Registry().Gather(); len(fams) != 0 {
		t.Fatalf("metrics off: %d families", len(fams))
	}
}

func TestSQLOperation(t *testing.T) {
	for _, tc := range []struct{ sql, op, want string }{
		{"SELECT * FROM notes", "query", "SELECT"},
		{"  (select 1)", "row", "SELECT"},
		{"insert into notes", "create", "INSERT"},
		{"WITH x AS (SELECT 1) SELECT * FROM x", "raw", "WITH"},
		{"", "delete", "DELETE"},
		{"", "raw", "EXEC"},
		{"123", "update", "UPDATE"},
		{"", "unknown", "OTHER"},
	} {
		if got := sqlOperation(tc.sql, tc.op); got != tc.want {
			t.Errorf("sqlOperation(%q, %q) = %q, want %q", tc.sql, tc.op, got, tc.want)
		}
	}
}

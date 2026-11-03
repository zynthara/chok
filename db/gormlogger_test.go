package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestGORMLogger_ThresholdAndClassification(t *testing.T) {
	var buf bytes.Buffer
	d := &DB{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	g := &gormLogger{d: d, slow: 50 * time.Millisecond, level: gormlogger.Warn}
	sql := func() (string, int64) { return "SELECT * FROM t WHERE email = ?", 1 }
	ctx := context.Background()

	g.Trace(ctx, time.Now().Add(-time.Second), sql, nil)
	if !strings.Contains(buf.String(), "slow query") || !strings.Contains(buf.String(), "email = ?") {
		t.Fatalf("a slow query is a warning with placeholders: %s", buf.String())
	}
	buf.Reset()
	g.Trace(ctx, time.Now(), sql, nil)
	if buf.Len() != 0 {
		t.Fatalf("a fast query is debug: %s", buf.String())
	}
	pg := func(code, msg string) error { return &pgconn.PgError{Severity: "ERROR", Code: code, Message: msg} }
	my := func(n uint16, msg string) error { return &gomysql.MySQLError{Number: n, Message: msg} }
	// An outcome is told by the error's type and code, wrapped or not:
	// Postgres's class 23, MySQL's duplicate key, foreign key, NOT NULL
	// and CHECK refusals and a taken index name, SQLite's
	// SQLITE_CONSTRAINT codes.
	for _, err := range []error{
		gorm.ErrRecordNotFound,
		context.Canceled,
		pg("23505", ""), pg("23503", ""), pg("23514", ""),
		pg("23502", `null value in column "email" of relation "users" violates not-null constraint`),
		pg("23P01", `conflicting key value violates exclusion constraint "ex_booking"`),
		my(1022, ""), my(1062, ""), my(1216, ""), my(1217, ""), my(1451, ""), my(1452, ""), my(1761, ""), my(1762, ""), my(3819, ""),
		my(1048, "Column 'email' cannot be null"),
		my(1364, "Field 'email' doesn't have a default value"),
		// createIndex (db/table.go) expects it of a concurrent migration.
		my(1061, "Duplicate key name 'uk_user_email'"),
		liteError{2067, ""}, liteError{1555, ""}, liteError{1299, ""}, liteError{787, ""},
		liteError{275, ""}, liteError{1811, ""}, liteError{3091, ""},
	} {
		for _, err := range []error{err, fmt.Errorf("create: %w", err)} {
			g.Trace(ctx, time.Now(), sql, err)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("expected outcomes are not errors: %s", buf.String())
	}
	// A fault stays one whatever its message reads: the message may quote
	// a value, and an error without a driver's type is no violation.
	for _, err := range []error{
		errors.New("connection reset by peer"),
		context.DeadlineExceeded,
		errors.New("Error 1062 (23000): Duplicate entry 'a' for key 'uk'"),
		my(1366, "Incorrect integer value: 'duplicate' for column 'n' at row 1"),
		my(1060, "Duplicate column name 'email'"),
		pg("22P02", `invalid input syntax for type integer: "unique constraint"`),
		pg("P0001", "duplicate booking"),
		liteError{1, "SQL logic error: no such table: duplicate_checks (1)"},
	} {
		buf.Reset()
		g.Trace(ctx, time.Now(), sql, err)
		if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "query failed") {
			t.Fatalf("%v is a fault: %s", err, buf.String())
		}
	}
	if s, args := g.ParamsFilter(ctx, "SELECT ?", "secret"); s != "SELECT ?" || args != nil {
		t.Fatal("bound values never reach the log")
	}
	// gorm logs a query read with Scan through its recorder, which asks
	// this variable instead of the handle's logger.
	if s, args := gormlogger.RecorderParamsFilter(ctx, "SELECT ?", "secret"); s != "SELECT ?" || args != nil {
		t.Fatal("nor do the bound values of a query read with Scan")
	}
	g0 := g.LogMode(gormlogger.Silent).(*gormLogger)
	buf.Reset()
	g0.Trace(ctx, time.Now().Add(-time.Second), sql, errors.New("boom"))
	if buf.Len() != 0 {
		t.Fatal("silent mode logs nothing")
	}
}

func TestGORMLogger_SlowThresholdZeroDisables(t *testing.T) {
	var buf bytes.Buffer
	d := &DB{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	g := &gormLogger{d: d, level: gormlogger.Warn}
	g.Trace(context.Background(), time.Now().Add(-time.Hour), func() (string, int64) { return "SELECT 1", 0 }, nil)
	if buf.Len() != 0 {
		t.Fatalf("slow_threshold 0 disables slow logs: %s", buf.String())
	}
}

// liteError imitates the pure-Go SQLite driver's error: an extended
// result code and the driver's message.
type liteError struct {
	code int
	msg  string
}

func (e liteError) Error() string { return e.msg }
func (e liteError) Code() int     { return e.code }

// TestErrorText: a Postgres or MySQL error, bare or wrapped, is its
// driver, code and names, never the value its message quotes; so is a
// SQLite error of SQLITE_ERROR or a trigger's RAISE. Any other error is
// logged as it is.
func TestErrorText(t *testing.T) {
	state := func(s string) (b [5]byte) {
		copy(b[:], s)
		return b
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{&pgconn.PgError{Severity: "ERROR", Code: "22P02", Message: `invalid input syntax for type integer: "s3cret-value"`}, "postgres 22P02"},
		{&pgconn.PgError{Severity: "ERROR", Code: "22003", Message: `value "s3cret-value" is out of range for type integer`}, "postgres 22003"},
		{&pgconn.PgError{
			Severity: "ERROR", Code: "23505", Message: `duplicate key value violates unique constraint "uk_user_email"`,
			Detail: "Key (email)=(s3cret-value) already exists.", ConstraintName: "uk_user_email", TableName: "users",
		}, "postgres 23505 constraint uk_user_email table users"},
		{&pgconn.PgError{
			Severity: "ERROR", Code: "23502", Message: `null value in column "email" of relation "users" violates not-null constraint`,
			Detail: "Failing row contains (1, s3cret-value, null).", TableName: "users", ColumnName: "email",
		}, "postgres 23502 table users column email"},
		{&gomysql.MySQLError{Number: 1062, SQLState: state("23000"), Message: "Duplicate entry 's3cret-value' for key 'users.uk_user_email'"},
			"mysql 1062 (23000) key users.uk_user_email"},
		// A value that reads like a key does not stand in for the key.
		{&gomysql.MySQLError{Number: 1062, SQLState: state("23000"), Message: "Duplicate entry 'x' for key 's3cret-value' for key 'users.uk_user_email'"},
			"mysql 1062 (23000) key users.uk_user_email"},
		{&gomysql.MySQLError{Number: 1366, SQLState: state("HY000"), Message: "Incorrect integer value: 's3cret-value' for column 'age' at row 1"},
			"mysql 1366 (HY000)"},
		{&gomysql.MySQLError{Number: 1292, SQLState: state("22007"), Message: "Truncated incorrect DOUBLE value: 's3cret-value'"}, "mysql 1292 (22007)"},
		{&gomysql.MySQLError{Number: 1105, Message: "s3cret-value"}, "mysql 1105"},
		{liteError{1, "SQL logic error: bad JSON path: 's3cret-value' (1)"}, "sqlite 1"},
		// The code cannot tell SQL that does not compile from a refused
		// value, so its message goes too.
		{liteError{1, "SQL logic error: no such table: users (1)"}, "sqlite 1"},
		{liteError{257, "SQL logic error: no such collation sequence: nocase2 (257)"}, "sqlite 257"},
		{liteError{1811, "constraint failed: email s3cret-value is taken (1811)"}, "sqlite 1811"},
	} {
		for _, err := range []error{c.err, fmt.Errorf("create: %w", c.err)} {
			if got := errorText(err); got != c.want {
				t.Errorf("%v: %q, want %q", err, got, c.want)
			}
		}
	}
	for _, err := range []error{
		liteError{2067, "constraint failed: UNIQUE constraint failed: users.email (2067)"},
		liteError{5, "database is locked (5) (SQLITE_BUSY)"},
		errors.New("connection reset by peer"),
		context.Canceled,
		gorm.ErrRecordNotFound,
	} {
		for _, err := range []error{err, fmt.Errorf("create: %w", err)} {
			if got := errorText(err); got != err.Error() {
				t.Errorf("%q, want it as it is: %q", got, err.Error())
			}
		}
	}
	// database/sql quotes a value it cannot scan with %q, %d or %v; the
	// value goes and the rest stays. A quote or a closing parenthesis in
	// the value does not end it early.
	scan := func(format string, v ...any) error {
		return fmt.Errorf("sql: Scan error on column index 0, name %q: %w", "n", fmt.Errorf(format, v...))
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{scan("converting driver.Value type %T (%q) to a %s: %v", "", "s3cret-value", "int", "invalid syntax"),
			`sql: Scan error on column index 0, name "n": converting driver.Value type string to a int: invalid syntax`},
		{scan("converting driver.Value type %T (%q) to a %s: %v", []byte(nil), `s3cret "value") to a int: x`, "float64", "invalid syntax"),
			`sql: Scan error on column index 0, name "n": converting driver.Value type []uint8 to a float64: invalid syntax`},
		{scan("converting driver.Value type string (%q) to a UUID: %v", "s3cret-value", "invalid uuid"),
			`sql: Scan error on column index 0, name "n": converting driver.Value type string to a UUID: invalid uuid`},
		{scan("sql/driver: couldn't convert %q into type bool", `s3cret "value" into type bool`),
			`sql: Scan error on column index 0, name "n": sql/driver: couldn't convert into type bool`},
		{scan("sql/driver: couldn't convert %d into type bool", 31337),
			`sql: Scan error on column index 0, name "n": sql/driver: couldn't convert into type bool`},
		{scan("sql/driver: couldn't convert %v (%T) into type bool", 3.5, 3.5),
			`sql: Scan error on column index 0, name "n": sql/driver: couldn't convert into type bool`},
	} {
		if got := errorText(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
		if got := errorText(fmt.Errorf("find: %w", c.err)); got != "find: "+c.want {
			t.Errorf("wrapped %v: %q, want %q", c.err, got, "find: "+c.want)
		}
	}
	// pgx quotes, with %#v or %v, an argument it cannot encode for the
	// type of its parameter, after failed to encode args[i]: the index
	// stays and the rest goes, whatever the value holds.
	encode := func(i int, err error) error { return fmt.Errorf("failed to encode args[%d]: %w", i, err) }
	for _, c := range []struct {
		err  error
		want string
	}{
		{encode(0, fmt.Errorf("unable to encode %#v into %s format for %s (OID %d): %w",
			struct{ V string }{"s3cret-value"}, "binary", "int4", 23, errors.New("cannot find encode plan"))),
			"failed to encode args[0]"},
		{encode(1, fmt.Errorf("unable to encode %v into OID %d in binary format", "s3cret-value", 16385)), "failed to encode args[1]"},
		{encode(12, fmt.Errorf("unable to encode %v", "s3cret; value\ns3cret-value")), "failed to encode args[12]"},
	} {
		if got := errorText(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
		if got := errorText(fmt.Errorf("exec: %w", c.err)); got != "exec: "+c.want {
			t.Errorf("wrapped %v: %q, want %q", c.err, got, "exec: "+c.want)
		}
	}
	// gorm adds an error to a statement's as %v; %w, so only the last
	// keeps its type: a driver's error before it, the statement's when
	// the rollback fails too, is text, logged as its driver and code.
	// MySQL's message runs to the separator, a line break in the value
	// included; Postgres's runs to its last SQLSTATE, so a value that reads
	// like one goes as well.
	join := func(first, last error) error { return fmt.Errorf("%v; %w", first, last) }
	rollback := errors.New("rollback failed")
	dup := &gomysql.MySQLError{Number: 1062, SQLState: state("23000"), Message: "Duplicate entry 's3cret-value' for key 'users.uk_user_email'"}
	for _, c := range []struct {
		err  error
		want string
	}{
		{join(dup, rollback), "mysql 1062 (23000); rollback failed"},
		{join(&gomysql.MySQLError{Number: 1366, SQLState: state("HY000"), Message: "Incorrect integer value: 's3cret\nvalue' for column 'age' at row 1"}, rollback),
			"mysql 1366 (HY000); rollback failed"},
		{join(join(dup, errors.New("driver: bad connection")), rollback), "mysql 1062 (23000); driver: bad connection; rollback failed"},
		{join(&pgconn.PgError{Severity: "ERROR", Code: "23505", Message: `duplicate key value violates unique constraint "uk_user_email"`}, rollback),
			"postgres 23505; rollback failed"},
		{join(&pgconn.PgError{Severity: "ERROR", Code: "22P02", Message: `invalid input syntax for type integer: "s3cret (SQLSTATE 00000); value"`}, rollback),
			"postgres 22P02; rollback failed"},
		{join(&pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}, rollback),
			"postgres 57P01; rollback failed"},
	} {
		if got := errorText(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
		if got := errorText(fmt.Errorf("create: %w", c.err)); got != "create: "+c.want {
			t.Errorf("wrapped %v: %q, want %q", c.err, got, "create: "+c.want)
		}
	}
}

// TestGORMLogger_LogsErrorText: a failed and a refused statement log
// what errorText makes of their errors.
func TestGORMLogger_LogsErrorText(t *testing.T) {
	var buf bytes.Buffer
	d := &DB{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	g := &gormLogger{d: d, level: gormlogger.Warn}
	sql := func() (string, int64) { return "INSERT INTO users (email) VALUES (?)", 0 }
	g.Trace(t.Context(), time.Now(), sql, &pgconn.PgError{Severity: "ERROR", Code: "22P02", Message: `invalid input syntax for type integer: "s3cret-value"`})
	g.Trace(t.Context(), time.Now(), sql, fmt.Errorf("create: %w", &gomysql.MySQLError{
		Number: 1062, SQLState: [5]byte{'2', '3', '0', '0', '0'}, Message: "Duplicate entry 's3cret-value' for key 'users.uk_user_email'",
	}))
	s := buf.String()
	for _, want := range []string{
		`level=ERROR msg="db: query failed" error="postgres 22P02"`,
		`level=DEBUG msg="db: query refused" error="mysql 1062 (23000) key users.uk_user_email"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("want %s in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "s3cret-value") {
		t.Errorf("the value stays out of the log:\n%s", s)
	}
}

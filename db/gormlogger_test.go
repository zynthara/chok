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
	g.Trace(ctx, time.Now(), sql, errors.New("Error 1062 (23000): Duplicate entry 'a' for key 'uk'"))
	g.Trace(ctx, time.Now(), sql, gorm.ErrRecordNotFound)
	g.Trace(ctx, time.Now(), sql, context.Canceled)
	if buf.Len() != 0 {
		t.Fatalf("expected outcomes are not errors: %s", buf.String())
	}
	g.Trace(ctx, time.Now(), sql, errors.New("connection reset by peer"))
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "query failed") {
		t.Fatalf("a fault is an error: %s", buf.String())
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

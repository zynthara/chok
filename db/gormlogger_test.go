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

// TestGORMLogger_LogsErrorText: a failed and a refused statement log
// what dberr.Text makes of their errors.
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

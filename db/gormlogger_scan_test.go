package db_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
)

// boundValue is bound to one query in each test and appears nowhere else,
// neither in a table name nor in an error the database returns.
const boundValue = "s3cret-value"

// debugHandle opens a fresh database whose handle logs every statement to
// the returned buffer.
func debugHandle(t *testing.T) (*db.DB, *logBuffer) {
	t.Helper()
	log := new(logBuffer)
	d := dbtest.Open(t, db.Logger(slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	return d, log
}

// statementLine returns the line of s logged as msg for query, whatever
// stands where query has its placeholder: the dialect's placeholder or a
// value.
func statementLine(s, msg, query string) string {
	before, after, _ := strings.Cut(query, "?")
	for line := range strings.Lines(s) {
		if strings.Contains(line, `msg="`+msg+`"`) && strings.Contains(line, `sql="`+before) && strings.Contains(line, after+`"`) {
			return line
		}
	}
	return ""
}

// TestGORMLogger_ScanLogsPlaceholders: a query read with Scan is logged
// with its placeholders, as every other statement is. gorm runs it under
// a recorder logger, which asks a package variable of gorm's logger
// rather than the handle's logger what to keep of the bound values.
func TestGORMLogger_ScanLogsPlaceholders(t *testing.T) {
	d, log := debugHandle(t)
	const query = "SELECT ? AS v"
	var v string
	if err := d.Unsafe(t.Context()).Raw(query, boundValue).Scan(&v).Error; err != nil || v != boundValue {
		t.Fatalf("got %q, %v", v, err)
	}
	s := log.String()
	if statementLine(s, "db: query", query) == "" {
		t.Fatalf("the query is logged on %s:\n%s", dbtest.Driver(), s)
	}
	if strings.Contains(s, boundValue) {
		t.Fatalf("the bound value stays out of the log on %s:\n%s", dbtest.Driver(), s)
	}
}

// TestGORMLogger_FailedScanLogsPlaceholders: a query read with Scan that
// fails is logged at error, with its placeholders too.
func TestGORMLogger_FailedScanLogsPlaceholders(t *testing.T) {
	d, log := debugHandle(t)
	const query = "SELECT c FROM no_such_table WHERE c = ?"
	var v string
	if err := d.Unsafe(t.Context()).Raw(query, boundValue).Scan(&v).Error; err == nil {
		t.Fatal("there is no such table")
	}
	s := log.String()
	if line := statementLine(s, "db: query failed", query); !strings.Contains(line, "level=ERROR") {
		t.Fatalf("the failed query is logged at error on %s:\n%s", dbtest.Driver(), s)
	}
	if strings.Contains(s, boundValue) {
		t.Fatalf("the bound value stays out of the log on %s:\n%s", dbtest.Driver(), s)
	}
}

// TestGORMLogger_RowScanLogsPlaceholders: a query read through Row, which
// gorm logs through the handle's logger itself, keeps its placeholders.
func TestGORMLogger_RowScanLogsPlaceholders(t *testing.T) {
	d, log := debugHandle(t)
	const query = "SELECT ? AS v"
	var v string
	if err := d.Unsafe(t.Context()).Raw(query, boundValue).Row().Scan(&v); err != nil || v != boundValue {
		t.Fatalf("got %q, %v", v, err)
	}
	s := log.String()
	if statementLine(s, "db: query", query) == "" {
		t.Fatalf("the query is logged on %s:\n%s", dbtest.Driver(), s)
	}
	if strings.Contains(s, boundValue) {
		t.Fatalf("the bound value stays out of the log on %s:\n%s", dbtest.Driver(), s)
	}
}

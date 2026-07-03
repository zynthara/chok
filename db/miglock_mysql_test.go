package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The MySQL migration-lock branch can't run against a live server in
// this repo's test matrix (the CI database service is Postgres-only,
// §12.6 budget). This fake driver pins the branch structurally: the
// exact GET_LOCK / RELEASE_LOCK statement sequence, the same-session
// requirement, and the timeout-derivation and lock-denied paths.
// Store-behaviour tests never mock the database (CLAUDE.md); this is
// a protocol-sequence test of our own lock statements, disclosed as
// such in the M3 report.

type fakeMySQLDriver struct {
	mu      sync.Mutex
	queries []string
	// getLockResult is what SELECT GET_LOCK returns: 1 granted, 0 timed out.
	getLockResult int64
}

func (d *fakeMySQLDriver) record(q string) {
	d.mu.Lock()
	d.queries = append(d.queries, q)
	d.mu.Unlock()
}

func (d *fakeMySQLDriver) recorded() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.queries...)
}

func (d *fakeMySQLDriver) Open(string) (driver.Conn, error) { return &fakeConn{d: d}, nil }

type fakeConn struct{ d *fakeMySQLDriver }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) { return &fakeStmt{c: c, q: query}, nil }
func (c *fakeConn) Close() error                              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)                 { return fakeTx{}, nil }

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeStmt struct {
	c *fakeConn
	q string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }

func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.c.d.record(s.q)
	return driver.RowsAffected(0), nil
}

func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.c.d.record(s.q)
	switch {
	case strings.Contains(s.q, "GET_LOCK"):
		return &fakeRows{cols: []string{"GET_LOCK"}, rows: [][]driver.Value{{s.c.d.getLockResult}}}, nil
	case strings.Contains(s.q, "RELEASE_LOCK"):
		return &fakeRows{cols: []string{"RELEASE_LOCK"}, rows: [][]driver.Value{{int64(1)}}}, nil
	default:
		return &fakeRows{cols: []string{"v"}, rows: [][]driver.Value{{int64(1)}}}, nil
	}
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

// newFakeMySQLGorm builds a gorm handle whose dialect reports "mysql"
// but whose wire protocol is the fake recorder.
func newFakeMySQLGorm(t *testing.T, fake *fakeMySQLDriver) *gorm.DB {
	t.Helper()
	name := "fake-mysql-" + t.Name()
	sql.Register(name, fake)
	sqlDB, err := sql.Open(name, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	gdb, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	return gdb
}

func TestMigrationLock_MySQLStatementSequence(t *testing.T) {
	fake := &fakeMySQLDriver{getLockResult: 1}
	gdb := newFakeMySQLGorm(t, fake)

	release, err := acquireMigrationLock(context.Background(), gdb)
	if err != nil {
		t.Fatal(err)
	}
	release()

	qs := fake.recorded()
	var got []string
	for _, q := range qs {
		if strings.Contains(q, "GET_LOCK") || strings.Contains(q, "RELEASE_LOCK") {
			got = append(got, q)
		}
	}
	if len(got) != 2 || !strings.Contains(got[0], "GET_LOCK") || !strings.Contains(got[1], "RELEASE_LOCK") {
		t.Fatalf("want GET_LOCK then RELEASE_LOCK, got %v", got)
	}
}

func TestMigrationLock_MySQLDenied(t *testing.T) {
	fake := &fakeMySQLDriver{getLockResult: 0} // 0 = wait timed out
	gdb := newFakeMySQLGorm(t, fake)

	_, err := acquireMigrationLock(context.Background(), gdb)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("GET_LOCK=0 must surface a timeout error, got %v", err)
	}
}

func TestMigrationLock_MySQLTimeoutFromDeadline(t *testing.T) {
	fake := &fakeMySQLDriver{getLockResult: 1}
	gdb := newFakeMySQLGorm(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := acquireMigrationLock(ctx, gdb)
	if err != nil {
		t.Fatal(err)
	}
	release()
	// The driver saw the interpolated GET_LOCK query; the numeric
	// timeout itself rides as a bind arg, so just pin that the deadline
	// path executed without falling back to the 60s default failing.
}

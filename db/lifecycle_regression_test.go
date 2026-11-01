package db_test

import (
	"context"
	"log/slog"
	"testing"
	"testing/fstest"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
	"github.com/zynthara/chok/internal/dbx"
)

func TestMigration_SingleConnectionPool(t *testing.T) {
	if dbtest.Driver() == "sqlite" {
		t.Skip("server dialects only")
	}
	cfg := dbtest.Config(t)
	cfg.Migrate = "versioned"
	cfg.Pool.MaxOpenConns = 1
	cfg.Pool.MaxIdleConns = 1
	cfg.MigrationTimeout = time.Second
	files := fstest.MapFS{"20261101000001_probe.sql": {Data: []byte("CREATE TABLE architecture_probe (id BIGINT PRIMARY KEY);")}}
	d := dbtest.OpenConfig(t, cfg, db.Migrations(files))
	pool, err := d.Unsafe(t.Context()).DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err = d.Migrate(ctx)
	if err != nil {
		t.Fatalf("single-connection migration: %v", err)
	}

}

func TestTransactions_RejectCrossHandleNesting(t *testing.T) {
	a, b := dbtest.Open(t), dbtest.Open(t)
	called := false
	err := a.RunInTx(t.Context(), func(ctx context.Context) error {
		return b.RunInTx(ctx, func(context.Context) error { called = true; return nil })
	})
	if err == nil || called {
		t.Fatalf("cross-handle nesting: err=%v called=%v", err, called)
	}
}

func TestLogger_IsCapturedAtOpen(t *testing.T) {
	before := slog.Default()
	t.Cleanup(func() { slog.SetDefault(before) })
	first := slog.New(slog.DiscardHandler)
	second := slog.New(slog.DiscardHandler)
	slog.SetDefault(first)
	d, err := db.Open(dbtest.Config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(t.Context()) })
	slog.SetDefault(second)
	if d.Logger() != first {
		t.Fatal("later default logger redirected an open database")
	}
}

func TestObserver_ReplacedDuringStatementStillPairsBeforeAndAfter(t *testing.T) {
	d := dbtest.Open(t)
	b := &pairedObserver{}
	a := &pairedObserver{before: func() { dbx.Observe(d, b) }}
	dbx.Observe(d, a)
	var n int
	if err := d.Unsafe(t.Context()).Raw("SELECT 1").Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if a.after != 1 || b.after != 0 {
		t.Fatalf("unpaired observers: old=%d new=%d", a.after, b.after)
	}
	if err := d.Unsafe(t.Context()).Raw("SELECT 1").Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if b.after != 1 {
		t.Fatal("new observer did not receive the next statement")
	}
}

type pairedObserver struct {
	before func()
	after  int
}

func (o *pairedObserver) Before(*gorm.DB, string) {
	if o.before != nil {
		o.before()
	}
}
func (o *pairedObserver) After(*gorm.DB, string) { o.after++ }

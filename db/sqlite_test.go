package db

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type item struct {
	Model
	Code string `gorm:"size:64;not null"`
}

type child struct {
	Model
	ItemID uint `gorm:"not null"`
	Item   item `gorm:"foreignKey:ItemID"`
}

func openFile(t *testing.T, pool int) *DB {
	t.Helper()
	d, err := Open(Config{Driver: "sqlite", SQLite: SQLiteConfig{Path: filepath.Join(t.TempDir(), "d.db")}, Pool: PoolConfig{MaxOpenConns: pool}},
		Tables(Table(&item{})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}

func TestOpenSQLite_SplitPoolSizes(t *testing.T) {
	d := openFile(t, 3)
	writer, err := d.Unsafe(context.Background()).DB()
	if err != nil {
		t.Fatal(err)
	}
	if got := writer.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("the writer is one connection, got %d", got)
	}
	if d.readers == nil || d.readers.Stats().MaxOpenConnections != 3 {
		t.Fatal("db.pool.max_open_conns sizes the read pool")
	}
	def := openFile(t, 0)
	if got, want := def.readers.Stats().MaxOpenConnections, max(4, runtime.NumCPU()); got != want {
		t.Fatalf("default read pool %d, want %d", got, want)
	}
}

func TestOpenSQLite_ReadsBypassTheWriteQueue(t *testing.T) {
	d := openFile(t, 0)
	ctx := context.Background()
	if err := d.Unsafe(ctx).Create(&item{Code: "seed"}).Error; err != nil {
		t.Fatal(err)
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- d.RunInTx(ctx, func(txCtx context.Context) error {
			if err := d.Unsafe(txCtx).Create(&item{Code: "writer"}).Error; err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var n int64
	err := d.Unsafe(readCtx).Model(&item{}).Count(&n).Error
	var raw int64
	rawErr := d.Unsafe(readCtx).Raw("SELECT COUNT(*) FROM items").Scan(&raw).Error
	close(release)
	if err != nil || rawErr != nil {
		t.Fatalf("reads must not wait for the held writer: %v / %v", err, rawErr)
	}
	if n != 1 || raw != 1 {
		t.Fatalf("snapshot reads saw %d and %d rows, want 1", n, raw)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOpenSQLite_ConcurrentReadModifyWrite_NoBusy(t *testing.T) {
	d := openFile(t, 0)
	ctx := context.Background()
	if err := d.Unsafe(ctx).Create(&item{Code: "seed"}).Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for g := range 10 {
		wg.Go(func() {
			for i := range 5 {
				err := d.RunInTx(ctx, func(txCtx context.Context) error {
					var it item
					if err := d.Unsafe(txCtx).First(&it).Error; err != nil {
						return err
					}
					return d.Unsafe(txCtx).Model(&it).Update("code", fmt.Sprintf("g%d-%d", g, i)).Error
				})
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		t.Fatalf("read-modify-write transactions queue on the writer instead of failing busy: %v", err)
	}
}

func TestOpenSQLite_ForeignKeysEnforced(t *testing.T) {
	d, err := Open(Config{Driver: "sqlite", SQLite: SQLiteConfig{Path: filepath.Join(t.TempDir(), "d.db")}}, Tables(Table(&item{}), Table(&child{})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	err = d.Unsafe(context.Background()).Create(&child{ItemID: 999}).Error
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("an orphan row must be refused: %v", err)
	}
}

func TestOpen_MemorySQLiteSurvivesConcurrentPoolUse(t *testing.T) {
	d, err := Open(Config{Driver: "sqlite", SQLite: SQLiteConfig{Path: ":memory:"}}, Tables(Table(&item{})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			_ = d.Unsafe(context.Background()).Create(&item{Code: fmt.Sprint(i)}).Error
		})
	}
	wg.Wait()
	var n int64
	if err := d.Unsafe(context.Background()).Model(&item{}).Count(&n).Error; err != nil || n != 8 {
		t.Fatalf("every connection must see the one in-memory database: %d %v", n, err)
	}
}

func TestOpenSQLite_CloseClosesReadPool(t *testing.T) {
	d, err := Open(Config{Driver: "sqlite", SQLite: SQLiteConfig{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.readers.Ping(); err == nil {
		t.Fatal("Close must close the read pool too")
	}
}

func TestMaintenance_CheckpointTruncatesWAL(t *testing.T) {
	d := openFile(t, 0)
	ctx := context.Background()
	for i := range 50 {
		if err := d.Unsafe(ctx).Create(&item{Code: fmt.Sprint(i)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	m := &maintenance{d: d}
	m.checkpoint(ctx)
	var busy, frames, done int
	if err := d.gdb.Raw("PRAGMA wal_checkpoint(PASSIVE)").Row().Scan(&busy, &frames, &done); err != nil {
		t.Fatal(err)
	}
	if frames != 0 {
		t.Fatalf("a TRUNCATE checkpoint leaves an empty WAL, got %d frames", frames)
	}
}

func TestMaintenance_DisabledStartsNothing(t *testing.T) {
	if m := startMaintenance(nil, SQLiteConfig{}); m != nil {
		t.Fatal("both intervals 0 start no loop")
	}
}

func TestMaintenance_CloseStopsLoopAndRunsPartingOptimize(t *testing.T) {
	d, err := Open(Config{Driver: "sqlite", SQLite: SQLiteConfig{Path: filepath.Join(t.TempDir(), "d.db"), CheckpointInterval: time.Millisecond, OptimizeInterval: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if d.maint == nil {
		t.Fatal("a file database with intervals runs maintenance")
	}
	time.Sleep(20 * time.Millisecond) // let a few ticks run
	if err := d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.maint.done:
	default:
		t.Fatal("Close stops the loop")
	}
}

func TestMaintenance_CloseWithSpentBudgetReturns(t *testing.T) {
	d, err := Open(Config{Driver: "sqlite", SQLite: SQLiteConfig{Path: filepath.Join(t.TempDir(), "d.db"), OptimizeInterval: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	begin := time.Now()
	_ = d.Close(ctx)
	if time.Since(begin) > time.Second {
		t.Fatal("a spent budget skips the parting optimize")
	}
}

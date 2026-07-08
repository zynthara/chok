package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// nopKernelLogger satisfies kernel.Logger for maintenance tests
// without dragging the log module into the db test graph.
type nopKernelLogger struct{}

func (nopKernelLogger) Debug(string, ...any)                         {}
func (nopKernelLogger) Info(string, ...any)                          {}
func (nopKernelLogger) Warn(string, ...any)                          {}
func (nopKernelLogger) Error(string, ...any)                         {}
func (nopKernelLogger) DebugContext(context.Context, string, ...any) {}
func (nopKernelLogger) InfoContext(context.Context, string, ...any)  {}
func (nopKernelLogger) WarnContext(context.Context, string, ...any)  {}
func (nopKernelLogger) ErrorContext(context.Context, string, ...any) {}

// tickRecorder is a race-safe onTick sink.
type tickRecorder struct {
	mu   sync.Mutex
	runs map[string]int
	errs map[string]error
}

func newTickRecorder() *tickRecorder {
	return &tickRecorder{runs: map[string]int{}, errs: map[string]error{}}
}

func (r *tickRecorder) record(job string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[job]++
	if err != nil {
		r.errs[job] = err
	}
}

func (r *tickRecorder) snapshot(job string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[job], r.errs[job]
}

// TestSQLiteMaintenance_CheckpointTruncatesWAL: the periodic
// wal_checkpoint(TRUNCATE) folds committed writes back into the main
// file and resets the -wal to zero bytes — the observable contract of
// the maintenance loop's checkpoint job.
func TestSQLiteMaintenance_CheckpointTruncatesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ctx := context.Background()
	if err := h.Migrate(ctx, Table(&TestItem{})); err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		if err := h.Unsafe(ctx).Create(&TestItem{Code: fmt.Sprintf("row-%d", i)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("precondition: writes must have grown the WAL (size=%v err=%v)", fi, err)
	}

	o := &SQLiteOptions{CheckpointInterval: 10 * time.Millisecond}
	rec := newTickRecorder()
	m := newSQLiteMaintenance(h, o, nopKernelLogger{}, "default")
	if m == nil {
		t.Fatal("maintenance must run when an interval is enabled")
	}
	m.onTick = rec.record
	m.start(o)
	defer m.close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if n, err := rec.snapshot("checkpoint"); n >= 2 {
			if err != nil {
				t.Fatalf("checkpoint reported an error: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("checkpoint never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fi, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("stat -wal: %v", err)
	} else if fi.Size() != 0 {
		t.Fatalf("-wal is %d bytes after checkpoint(TRUNCATE), want 0", fi.Size())
	}
}

// TestSQLiteMaintenance_CloseStopsLoopAndRunsPartingOptimize: close
// is synchronous (the loop is gone when it returns), runs the final
// PRAGMA optimize SQLite recommends, and is idempotent.
func TestSQLiteMaintenance_CloseStopsLoopAndRunsPartingOptimize(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	o := &SQLiteOptions{OptimizeInterval: time.Hour} // ticks never fire; only close's parting run
	rec := newTickRecorder()
	m := newSQLiteMaintenance(h, o, nopKernelLogger{}, "default")
	m.onTick = rec.record
	m.start(o)

	m.close()
	select {
	case <-m.done:
	default:
		t.Fatal("close must not return before the loop exited")
	}
	if n, err := rec.snapshot("optimize"); n != 1 || err != nil {
		t.Fatalf("parting optimize: runs=%d err=%v, want exactly one clean run", n, err)
	}
	m.close() // second close must be a no-op, not a panic
	if n, _ := rec.snapshot("optimize"); n != 1 {
		t.Fatalf("idempotent close reran optimize (%d runs)", n)
	}
}

// TestStartSQLiteMaintenance_DisabledReturnsNil: both intervals off
// means no goroutine at all.
func TestStartSQLiteMaintenance_DisabledReturnsNil(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if m := startSQLiteMaintenance(h, &SQLiteOptions{}, nopKernelLogger{}, "default"); m != nil {
		t.Fatal("disabled maintenance must not start")
	}
}

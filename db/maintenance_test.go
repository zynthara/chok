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
	last map[string]string
}

func newTickRecorder() *tickRecorder {
	return &tickRecorder{runs: map[string]int{}, last: map[string]string{}}
}

func (r *tickRecorder) record(job, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[job]++
	r.last[job] = result
}

func (r *tickRecorder) snapshot(job string) (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[job], r.last[job]
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
	m := newSQLiteMaintenance(ctx, h, o, nopKernelLogger{}, "default")
	if m == nil {
		t.Fatal("maintenance must run when an interval is enabled")
	}
	m.onTick = rec.record
	m.start(o)
	defer m.close(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if n, result := rec.snapshot("checkpoint"); n >= 2 {
			if result != maintenanceOK {
				t.Fatalf("checkpoint result = %q, want %q", result, maintenanceOK)
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
	ctx := context.Background()
	m := newSQLiteMaintenance(ctx, h, o, nopKernelLogger{}, "default")
	m.onTick = rec.record
	m.start(o)

	m.close(ctx)
	select {
	case <-m.done:
	default:
		t.Fatal("close must not return before the loop exited")
	}
	if n, result := rec.snapshot("optimize"); n != 1 || result != maintenanceOK {
		t.Fatalf("parting optimize: runs=%d result=%q, want exactly one clean run", n, result)
	}
	m.close(ctx) // second close must be a no-op, not a panic
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
	if m := startSQLiteMaintenance(context.Background(), h, &SQLiteOptions{}, nopKernelLogger{}, "default", nil); m != nil {
		t.Fatal("disabled maintenance must not start")
	}
}

// TestSQLiteMaintenance_CloseAbandonsOverrunningJob (db-layer review
// #6): a job that overruns the shutdown budget gets its SQL
// interrupted and, failing that, the goroutine abandoned — close
// returns within budget+grace instead of hanging registry teardown.
// The job is pinned inside the onTick seam, which cancellation cannot
// unblock, so this exercises the abandon path deterministically.
func TestSQLiteMaintenance_CloseAbandonsOverrunningJob(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	o := &SQLiteOptions{OptimizeInterval: 5 * time.Millisecond}
	m := newSQLiteMaintenance(context.Background(), h, o, nopKernelLogger{}, "default")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m.onTick = func(string, string) {
		once.Do(func() { close(entered) })
		<-release
	}
	m.start(o)
	<-entered // a job is now in flight and will not finish on its own

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	m.close(ctx)
	if elapsed := time.Since(start); elapsed > maintCloseGrace+2*time.Second {
		t.Fatalf("close took %v — did not honor its budget", elapsed)
	}
	select {
	case <-m.done:
		t.Fatal("loop cannot have exited while the job is pinned")
	default:
	}

	close(release) // let the pinned job return; the loop must then exit
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop never exited after the abandoned job returned")
	}
	m.close(ctx) // still idempotent after the abandon path
}

// TestSQLiteMaintenance_CloseSpentBudgetSkipsPartingOptimize
// (db-layer review #6): when the budget is already gone the loop
// still stops synchronously, but the parting optimize is skipped
// rather than run outside any deadline.
func TestSQLiteMaintenance_CloseSpentBudgetSkipsPartingOptimize(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	o := &SQLiteOptions{OptimizeInterval: time.Hour}
	rec := newTickRecorder()
	m := newSQLiteMaintenance(context.Background(), h, o, nopKernelLogger{}, "default")
	m.onTick = rec.record
	m.start(o)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.close(ctx)
	select {
	case <-m.done:
	default:
		t.Fatal("close must not return before the loop exited")
	}
	if n, _ := rec.snapshot("optimize"); n != 0 {
		t.Fatalf("parting optimize ran %d times under a spent budget, want 0", n)
	}
}

// TestSQLiteMaintenance_JobsHonorContext (db-layer review #6): the
// job SQL actually rides the context it is given — a cancelled one
// must surface as an error instead of running unbounded.
func TestSQLiteMaintenance_JobsHonorContext(t *testing.T) {
	h, err := Open(Options{Driver: "sqlite", SQLite: SQLiteOptions{Path: filepath.Join(t.TempDir(), "d.db")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	o := &SQLiteOptions{OptimizeInterval: time.Hour}
	rec := newTickRecorder()
	m := newSQLiteMaintenance(context.Background(), h, o, nopKernelLogger{}, "default")
	m.onTick = rec.record
	m.start(o)
	defer m.close(context.Background())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	m.checkpoint(cancelled)
	m.optimize(cancelled)
	if _, result := rec.snapshot("checkpoint"); result != maintenanceError {
		t.Fatal("checkpoint ignored its cancelled context")
	}
	if _, result := rec.snapshot("optimize"); result != maintenanceError {
		t.Fatal("optimize ignored its cancelled context")
	}
}

func TestMaintenanceResult_DeferredIsNotOK(t *testing.T) {
	if got := maintenanceResult(nil, true); got != maintenanceDeferred {
		t.Fatalf("deferred checkpoint classified as %q", got)
	}
	if got := maintenanceResult(context.Canceled, true); got != maintenanceError {
		t.Fatalf("errors must win over deferred, got %q", got)
	}
}

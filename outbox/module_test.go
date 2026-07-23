package outbox_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zynthara/chok/v2/choktest"
	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/internal/testschema"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/outbox"
	"github.com/zynthara/chok/v2/scheduler"
)

// The component is the battery's enqueue face — drift breaks the
// role-interface consumers at compile time.
var _ outbox.Enqueuer = (*outbox.Component)(nil)

const outboxYAML = `
db:
  driver: sqlite
  sqlite:
    path: ":memory:"
outbox:
  poll_interval: 1h
`

func TestOptions_Validate(t *testing.T) {
	base := func() outbox.Options {
		return outbox.Options{
			Enabled:      true,
			PollInterval: time.Second, BatchSize: 100,
			SettleWindow: 30 * time.Second, CleanupInterval: time.Hour,
		}
	}
	if o := base(); o.Validate() != nil {
		t.Fatalf("valid options rejected: %v", o.Validate())
	}
	disabled := outbox.Options{Enabled: false}
	if err := disabled.Validate(); err != nil {
		t.Fatalf("disabled section must not validate fields: %v", err)
	}
	cases := map[string]func(*outbox.Options){
		"poll_interval":    func(o *outbox.Options) { o.PollInterval = 0 },
		"batch_size low":   func(o *outbox.Options) { o.BatchSize = 0 },
		"batch_size high":  func(o *outbox.Options) { o.BatchSize = 10_001 },
		"settle_window":    func(o *outbox.Options) { o.SettleWindow = 0 },
		"retention":        func(o *outbox.Options) { o.Retention = -time.Second },
		"cleanup_interval": func(o *outbox.Options) { o.CleanupInterval = 0 },
	}
	for name, mutate := range cases {
		o := base()
		mutate(&o)
		if o.Validate() == nil {
			t.Errorf("%s: invalid options accepted", name)
		}
	}
}

func TestModule_DescriptorShape(t *testing.T) {
	d := outbox.Module().Describe()
	if d.Kind != "outbox" || d.ConfigKey != "outbox" {
		t.Fatalf("descriptor = %+v", d)
	}
	wantTables := []string{"outbox_messages", "outbox_relay_state", "schema_migrations_chok_outbox"}
	if len(d.Schema.Tables) != len(wantTables) {
		t.Fatalf("schema tables = %v, want %v", d.Schema.Tables, wantTables)
	}
	for i, w := range wantTables {
		if d.Schema.Tables[i] != w {
			t.Fatalf("schema tables = %v, want %v", d.Schema.Tables, wantTables)
		}
	}
	needDB := false
	for _, n := range d.Needs {
		if n.Kind == "db" && !n.Optional {
			needDB = true
		}
		if n.Kind == "scheduler" && !n.Optional {
			t.Fatal("scheduler must stay a soft dependency")
		}
	}
	if !needDB {
		t.Fatal("db must be a hard dependency")
	}
}

func TestModule_EnqueueRoundTripAndDelivery(t *testing.T) {
	var mu sync.Mutex
	var got []string
	component := outbox.Module(outbox.WithRelay("test", func(_ context.Context, rec outbox.Record) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, rec.Topic+":"+string(rec.Payload))
		return nil
	}))
	tk := choktest.NewTestKernel(t, outboxYAML, db.Module(), scheduler.Module(), component)
	testschema.AssertOwnershipForMode(t, db.From(tk), component, db.MigrateAuto)

	ctx := context.Background()
	ob := outbox.From(tk)
	h := db.From(tk)
	err := h.RunInTx(ctx, func(txCtx context.Context) error {
		return ob.Enqueue(txCtx, "orders", []byte("o1"))
	})
	if err != nil {
		t.Fatal(err)
	}

	// poll_interval is 1h — drive the registered job by hand.
	sc, ok := kernel.Get[*scheduler.Component](tk, "scheduler")
	if !ok {
		t.Fatal("scheduler component not visible")
	}
	if err := sc.Scheduler().RunNow("outbox-relay-test"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "orders:o1" {
		t.Fatalf("delivered = %v", got)
	}
}

func TestModule_VersionedUsesOwnedLedger(t *testing.T) {
	component := outbox.Module()
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  migrate: versioned
  sqlite: {path: ":memory:"}
outbox: {}
`, db.Module(db.WithMigrations(fstest.MapFS{
		"README.txt": &fstest.MapFile{Data: []byte("no application migrations")},
	})), component)
	testschema.AssertOwnershipForMode(t, db.From(tk), component, db.MigrateVersioned)
	st, err := db.SequenceStatus(t.Context(), db.From(tk), outbox.MigrationSequence())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Applied) != 1 || len(st.Pending) != 0 || st.Ledger != "schema_migrations_chok_outbox" {
		t.Fatalf("outbox owned status = %+v", st)
	}
}

func TestModule_MigrateOff_NoDDL_EnqueueFails(t *testing.T) {
	tk := choktest.NewTestKernel(t, `
db:
  driver: sqlite
  migrate: off
  sqlite:
    path: ":memory:"
outbox: {}
`, db.Module(), outbox.Module())

	ctx := context.Background()
	if db.From(tk).Unsafe(ctx).Migrator().HasTable("outbox_messages") {
		t.Fatal("migrate off must not create outbox_messages")
	}
	ob := outbox.From(tk)
	err := db.From(tk).RunInTx(ctx, func(txCtx context.Context) error {
		return ob.Enqueue(txCtx, "t", nil)
	})
	if err == nil {
		t.Fatal("enqueue against a missing table should error")
	}
}

func TestModule_RelaysRequireScheduler(t *testing.T) {
	_, err := choktest.StartKernel(t, outboxYAML,
		db.Module(),
		outbox.Module(outbox.WithRelay("r", func(context.Context, outbox.Record) error { return nil })),
	)
	if err == nil || !strings.Contains(err.Error(), "scheduler module is absent") {
		t.Fatalf("want relay-without-scheduler fail-fast, got %v", err)
	}
	// Enqueue-only (zero relays) boots fine without a scheduler.
	if _, err := choktest.StartKernel(t, outboxYAML, db.Module(), outbox.Module()); err != nil {
		t.Fatalf("enqueue-only assembly must boot without a scheduler: %v", err)
	}
}

func TestModule_DuplicateRelayNameFailsInit(t *testing.T) {
	noop := func(context.Context, outbox.Record) error { return nil }
	_, err := choktest.StartKernel(t, outboxYAML,
		db.Module(), scheduler.Module(),
		outbox.Module(outbox.WithRelay("dup", noop), outbox.WithRelay("dup", noop)),
	)
	if err == nil || !strings.Contains(err.Error(), `duplicate relay name "dup"`) {
		t.Fatalf("want duplicate-name fail-fast, got %v", err)
	}
}

func TestModule_ReadOnlyDBFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox-readonly.db")
	h, err := db.Open(db.Options{Driver: "sqlite", SQLite: db.SQLiteOptions{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	_, err = choktest.StartKernel(t, fmt.Sprintf(`
db:
  driver: sqlite
  read_only: true
  sqlite: {path: %q}
outbox: {}
`, path), db.Module(), outbox.Module())
	if err == nil || !strings.Contains(err.Error(), "outbox requires a writable database") {
		t.Fatalf("want outbox read-only fail-fast, got %v", err)
	}
}

func TestModule_CleanupJobSweeps(t *testing.T) {
	component := outbox.Module(outbox.WithRelay("clean", func(context.Context, outbox.Record) error { return nil }))
	tk := choktest.NewTestKernel(t, outboxYAML+`
  settle_window: 1ms
  retention: 1ms
  cleanup_interval: 1h
`, db.Module(), scheduler.Module(), component)

	ctx := context.Background()
	ob := outbox.From(tk)
	h := db.From(tk)
	for i := 0; i < 3; i++ {
		if err := h.RunInTx(ctx, func(txCtx context.Context) error {
			return ob.Enqueue(txCtx, "t", []byte{byte('a' + i)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	sc, _ := kernel.Get[*scheduler.Component](tk, "scheduler")
	time.Sleep(5 * time.Millisecond) // let the rows pass the tiny settle window
	if err := sc.Scheduler().RunNow("outbox-relay-clean"); err != nil {
		t.Fatal(err)
	}
	if err := sc.Scheduler().RunNow("outbox-cleanup"); err != nil {
		t.Fatal(err)
	}
	var left int64
	if err := h.Unsafe(ctx).Table("outbox_messages").Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	// Strict less-than keeps the watermark row itself; everything
	// before it is swept.
	if left != 1 {
		t.Fatalf("rows left after cleanup = %d, want 1", left)
	}
}

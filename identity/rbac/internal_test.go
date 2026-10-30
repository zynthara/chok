package rbac

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
)

func newTestEngine(t *testing.T) *engine {
	t.Helper()
	adapter, err := newGormAdapter(newAdapterDB(t))
	if err != nil {
		t.Fatal(err)
	}
	e, err := newEngine(rbacWithDomainsModel, adapter, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.close)
	return e
}

func TestEngine_RedisWatcherStatsCountFailures(t *testing.T) {
	mr := miniredis.RunT(t)
	e := newTestEngine(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	if err := e.attachRedisWatcher(context.Background(), client, watcherChannel); err != nil {
		t.Fatal(err)
	}
	if got := e.watcherStats(); got.PublishFailures != 0 || got.ReloadFailures != 0 {
		t.Fatalf("a fresh watcher: %+v", got)
	}
	mr.Close()
	// The change is stored even when its announcement fails.
	if err := (&PolicyService{e: e}).GrantRole(context.Background(), "admin", "task", "read"); err != nil {
		t.Fatalf("a grant with Redis down: %v", err)
	}
	if got := e.watcherStats(); got.PublishFailures < 1 {
		t.Fatalf("the failed announcement is counted: %+v", got)
	}
}

func TestEngine_NoStatsWithoutWatcher(t *testing.T) {
	if got := newTestEngine(t).watcherStats(); got != (watcherStats{}) {
		t.Fatalf("no watcher, no counters: %+v", got)
	}
}

// countingAdapter counts the full policy loads of the adapter it wraps.
type countingAdapter struct {
	persist.Adapter
	loads atomic.Int32
}

func (a *countingAdapter) LoadPolicy(m model.Model) error {
	a.loads.Add(1)
	return a.Adapter.LoadPolicy(m)
}

// TestNewEngine_LoadsThePoliciesOnce: building the engine reads the
// policy table once — casbin's constructor already loads it — and the
// policies stored before are in force.
func TestNewEngine_LoadsThePoliciesOnce(t *testing.T) {
	gdb := newAdapterDB(t)
	adapter, err := newGormAdapter(gdb)
	if err != nil {
		t.Fatal(err)
	}
	first, err := newEngine(rbacWithDomainsModel, adapter, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := (&PolicyService{e: first}).GrantUser(ctx, "usr_1", "task", "read"); err != nil {
		t.Fatal(err)
	}
	first.close()

	counting := &countingAdapter{Adapter: adapter}
	e, err := newEngine(rbacWithDomainsModel, counting, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.close)
	if n := counting.loads.Load(); n != 1 {
		t.Fatalf("newEngine loaded the policies %d times, want 1", n)
	}
	if ok, err := e.decide("usr_1", nil, "", "task", "read"); err != nil || !ok {
		t.Fatalf("the stored grant is in force after one load: %v, %v", ok, err)
	}
}

func TestNewEngine_MissingTableFailsAtLoad(t *testing.T) {
	d := dbtest.Open(t)
	adapter, err := newGormAdapter(func() *gorm.DB { return d.Unsafe(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newEngine(rbacWithDomainsModel, adapter, nil); err == nil || !strings.Contains(err.Error(), "load the policies") {
		t.Fatalf("no casbin_rule table: %v", err)
	}
	if _, err := newEngine("not a model", adapter, nil); err == nil {
		t.Fatal("a bad model fails")
	}
}

// describe renders casbin_rule as the database reports it.
func describe(t *testing.T, d *db.DB) string {
	t.Helper()
	m := d.Unsafe(t.Context()).Migrator()
	cols, err := m.ColumnTypes("casbin_rule")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range cols {
		length, _ := c.Length()
		nullable, _ := c.Nullable()
		pk, _ := c.PrimaryKey()
		lines = append(lines, fmt.Sprintf("%s %s(%d) null=%v pk=%v", c.Name(), strings.ToLower(c.DatabaseTypeName()), length, nullable, pk))
	}
	idx, err := m.GetIndexes("casbin_rule")
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range idx {
		unique, _ := i.Unique()
		lines = append(lines, fmt.Sprintf("index %s %v unique=%v", i.Name(), slices.Sorted(slices.Values(i.Columns())), unique))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func TestMigrations_VersionedMatchesAuto(t *testing.T) {
	sub, err := migrationsFS()
	if err != nil {
		t.Fatal(err)
	}
	spec := []db.TableSpec{casbinTable()}
	auto := dbtest.Open(t)
	if err := auto.RegisterSchema("rbac", spec, sub); err != nil {
		t.Fatal(err)
	}
	cfg := dbtest.Config(t)
	cfg.Migrate = "versioned"
	versioned := dbtest.OpenConfig(t, cfg)
	if err := versioned.RegisterSchema("rbac", spec, sub); err != nil {
		t.Fatal(err)
	}
	if a, v := describe(t, auto), describe(t, versioned); a != v {
		t.Fatalf("versioned differs from auto on %s\nauto:\n%s\nversioned:\n%s", dbtest.Driver(), a, v)
	}
	sts, err := versioned.MigrationStatus(t.Context())
	if err != nil || len(sts) != 1 || sts[0].Owner != "rbac" || sts[0].State != "applied" || sts[0].Name != "create_casbin_rule" {
		t.Fatalf("the ledger: %+v %v", sts, err)
	}
	if !strings.Contains(describe(t, auto), "index unique_index [ptype v0 v1 v2 v3 v4 v5] unique=true") {
		t.Fatalf("the unique index:\n%s", describe(t, auto))
	}
}

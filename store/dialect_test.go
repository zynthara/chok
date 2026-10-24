package store_test

// The dialect differences of plan §5.6 and the MySQL time baseline of
// §5.2, one test each. A test that pins one dialect's behaviour skips the
// others; a test about converging behaviour runs on every lane.

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/zynthara/chok/v3/db"
	"github.com/zynthara/chok/v3/internal/dbtest"
	"github.com/zynthara/chok/v3/store"
	"github.com/zynthara/chok/v3/store/where"
)

func onlyMySQL(t *testing.T) {
	t.Helper()
	if dbtest.Driver() != "mysql" {
		t.Skip("pins MySQL's behaviour; runs on the MySQL lane")
	}
}

// sale is the aggregate fixture: every scalar kind a dialect returns
// differently.
type sale struct {
	db.SoftDeleteModel
	Status string    `json:"status" store:"query" gorm:"size:16;not null"`
	Qty    int64     `json:"qty" store:"query" gorm:"not null"`
	Price  float64   `json:"price" store:"query" gorm:"not null"`
	Flag   bool      `json:"flag" store:"query" gorm:"not null"`
	At     time.Time `json:"at" store:"query" gorm:"not null"`
}

var t0 = time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)

func sales(t *testing.T) (*store.Store[sale], *db.DB) {
	t.Helper()
	d := dbtest.Open(t, db.Tables(db.Table(&sale{})))
	return store.New[sale](d), d
}

// seedSales: paid 10 × 1.50 at t0, paid 20 × 2.25 at t0+1h, draft 3 ×
// 0.25 at t0+2h.
func seedSales(t *testing.T, s *store.Store[sale]) {
	t.Helper()
	mustCreate(t, t.Context(), s,
		&sale{Status: "paid", Qty: 10, Price: 1.50, Flag: true, At: t0},
		&sale{Status: "paid", Qty: 20, Price: 2.25, Flag: true, At: t0.Add(time.Hour)},
		&sale{Status: "draft", Qty: 3, Price: 0.25, Flag: false, At: t0.Add(2 * time.Hour)},
	)
}

// TestAggregate_TypeMappingConverges: the drivers hand aggregates back as
// different types — MySQL SUM and AVG as DECIMAL bytes, PostgreSQL SUM
// of a bigint as numeric, SQLite times as text — and every one converges
// on the Go type asked for.
func TestAggregate_TypeMappingConverges(t *testing.T) {
	s, d := sales(t)
	seedSales(t, s)
	ctx := t.Context()
	if sum, ok, err := s.Sum[int64](ctx, "qty"); err != nil || !ok || sum != 33 {
		t.Fatalf("Sum[int64] = %d %v %v", sum, ok, err)
	}
	if sum, ok, err := s.Sum[float64](ctx, "price"); err != nil || !ok || math.Abs(sum-4) > 1e-9 {
		t.Fatalf("Sum[float64] = %v %v %v", sum, ok, err)
	}
	if avg, ok, err := s.Avg(ctx, "qty"); err != nil || !ok || math.Abs(avg-11) > 1e-9 {
		t.Fatalf("Avg = %v %v %v", avg, ok, err)
	}
	if at, ok, err := s.Max[time.Time](ctx, "at"); err != nil || !ok || !at.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("Max[time.Time] = %v %v %v", at, ok, err)
	}
	if lo, ok, err := s.Min[float64](ctx, "qty"); err != nil || !ok || lo != 3 {
		t.Fatalf("Min[float64] of an integer = %v %v %v", lo, ok, err)
	}
	groups, err := s.GroupBy[string](ctx, "status", []store.Aggregate{
		{Func: "count"}, {Func: "sum", Field: "qty"}, {Func: "avg", Field: "price"}, {Func: "max", Field: "at"}, {Func: "count_distinct", Field: "qty"},
	})
	if err != nil || len(groups) != 2 || groups[0].Key != "draft" {
		t.Fatalf("groups %v %v", groups, err)
	}
	paid := groups[1].Values
	if n, ok := paid[1].Int64(); !ok || n != 30 {
		t.Fatalf("paid sum %v", paid[1])
	}
	if avg, ok := paid[2].Float64(); !ok || math.Abs(avg-1.875) > 1e-9 {
		t.Fatalf("paid avg %v", paid[2])
	}
	if at, ok := paid[3].Time(); !ok || !at.Equal(t0.Add(time.Hour)) {
		t.Fatalf("paid max %v", paid[3])
	}
	if n, ok := paid[4].Int64(); !ok || n != 2 {
		t.Fatalf("paid count distinct %v", paid[4])
	}
	if flags, err := s.GroupBy[bool](ctx, "flag", []store.Aggregate{{Func: "count"}}); err != nil || len(flags) != 2 || flags[0].Key || !flags[1].Key {
		t.Fatalf("bool keys %v %v", flags, err)
	}
	if qtys, err := s.GroupBy[int64](ctx, "qty", []store.Aggregate{{Func: "count"}}); err != nil || len(qtys) != 3 || qtys[0].Key != 3 {
		t.Fatalf("int64 keys %v %v", qtys, err)
	}
	if prices, err := s.GroupBy[float64](ctx, "price", []store.Aggregate{{Func: "count"}}); err != nil || len(prices) != 3 || prices[0].Key != 0.25 {
		t.Fatalf("float64 keys %v %v", prices, err)
	}
	if ats, err := s.GroupBy[time.Time](ctx, "at", []store.Aggregate{{Func: "sum", Field: "qty"}}); err != nil || len(ats) != 3 || !ats[2].Key.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("time keys %v %v", ats, err)
	}
	if dbtest.Driver() == "mysql" {
		// What the conversion is for: MySQL answers SUM and AVG of an
		// integer column in DECIMAL, which the driver hands over as text.
		var sum, avg any
		if err := d.Unsafe(ctx).Raw("SELECT SUM(qty), AVG(qty) FROM sales").Row().Scan(&sum, &avg); err != nil {
			t.Fatal(err)
		}
		if _, ok := sum.([]byte); !ok {
			t.Fatalf("MySQL SUM came back as %T", sum)
		}
		if _, ok := avg.([]byte); !ok {
			t.Fatalf("MySQL AVG came back as %T", avg)
		}
	}
}

type tiered struct {
	db.Model
	Tier string `json:"tier" store:"query" gorm:"type:enum('free','pro');not null"`
}

// TestAggregate_MySQLEnumColumn: an ENUM is a bounded string — it groups
// and counts like one.
func TestAggregate_MySQLEnumColumn(t *testing.T) {
	onlyMySQL(t)
	s := store.New[tiered](dbtest.Open(t, db.Tables(db.Table(&tiered{}))))
	ctx := t.Context()
	mustCreate(t, ctx, s, &tiered{Tier: "free"}, &tiered{Tier: "pro"}, &tiered{Tier: "free"})
	groups, err := s.GroupBy[string](ctx, "tier", []store.Aggregate{{Func: "count"}})
	if err != nil || len(groups) != 2 || groups[0].Key != "free" {
		t.Fatalf("enum groups %v %v", groups, err)
	}
	if n, _ := groups[0].Values[0].Int64(); n != 2 {
		t.Fatalf("free counts %d", n)
	}
	if n, err := s.CountDistinct(ctx, "tier"); err != nil || n != 2 {
		t.Fatalf("CountDistinct = %d %v", n, err)
	}
	if _, _, err := s.Max[int64](ctx, "tier"); err == nil {
		t.Fatal("an enum is not a number")
	}
}

// TestBatchUpsert_MySQLIgnoresTheConflictTarget: ON DUPLICATE KEY UPDATE
// fires on any unique key, whatever conflict names: a collision on the
// alt key updates that row instead of inserting. The other dialects keep
// to the target (TestBatchUpsert_ConflictTargetIsStrict).
func TestBatchUpsert_MySQLIgnoresTheConflictTarget(t *testing.T) {
	onlyMySQL(t)
	s := batchItems(t)
	ctx := t.Context()
	mustCreate(t, ctx, s, &batchItem{Code: "old", Alt: "shared"})
	if err := s.BatchUpsert(ctx, []*batchItem{{Code: "new", Alt: "shared", N: 5}}, "code"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("rows %d: the upsert inserted", n)
	}
	got, _ := s.Get(ctx, store.Where(where.Eq("alt", "shared")))
	if got.Code != "new" || got.N != 5 || got.Version != 2 {
		t.Fatalf("the row the alt key found was updated: %+v", got)
	}
}

// ciCode is matched case-insensitively: SQLite through the column's
// NOCASE collation (MySQL's default collation needs no spelling).
type ciCode struct {
	db.Model
	Code string `json:"code" store:"query" gorm:"type:varchar(20) COLLATE NOCASE;not null"`
}

type mysqlCode struct {
	db.Model
	Code string `json:"code" store:"query" gorm:"size:20;not null"`
}

// fillers puts first and last 501 values apart: different IN chunks.
func fillers(first, last string) []string {
	values := []string{first}
	for i := range 500 {
		values = append(values, fmt.Sprintf("filler%03d", i))
	}
	return append(values, last)
}

// TestListIn_CaseInsensitiveCollationDedupes: under a case-insensitive
// collation a row matches "a" in one chunk and "A" in the next; ListIn
// returns it once, as one IN over the whole set would.
func TestListIn_CaseInsensitiveCollationDedupes(t *testing.T) {
	ctx := t.Context()
	var items int
	var err error
	switch dbtest.Driver() {
	case "sqlite":
		s := store.New[ciCode](dbtest.Open(t, db.Tables(db.Table(&ciCode{}))))
		mustCreate(t, ctx, s, &ciCode{Code: "a"})
		var got []ciCode
		got, err = s.ListIn(ctx, "code", fillers("a", "A"))
		items = len(got)
	case "mysql":
		s := store.New[mysqlCode](dbtest.Open(t, db.Tables(db.Table(&mysqlCode{}))))
		mustCreate(t, ctx, s, &mysqlCode{Code: "a"})
		var got []mysqlCode
		got, err = s.ListIn(ctx, "code", fillers("a", "A"))
		items = len(got)
	default:
		t.Skip("PostgreSQL compares case")
	}
	if err != nil || items != 1 {
		t.Fatalf("a row matching two chunks: %d rows, %v", items, err)
	}
}

// TestSoftUnique_ReleasedByDelete: UNIQUE(title, owner_id, delete_token)
// on every dialect — live rows are unique per owner, a delete frees the
// value, deleted copies coexist, and a restore into a taken slot fails.
func TestSoftUnique_ReleasedByDelete(t *testing.T) {
	docs := store.New[doc](openDB(t))
	alice, bob := as(t.Context(), "alice"), as(t.Context(), "bob")
	first := &doc{Title: "t"}
	mustCreate(t, alice, docs, first)
	var dup *store.DuplicateError
	if err := docs.Create(alice, &doc{Title: "t"}); !errors.As(err, &dup) || dup.Field != "title" {
		t.Fatalf("a live duplicate: %v", err)
	}
	mustCreate(t, bob, docs, &doc{Title: "t"})
	if err := docs.Delete(alice, store.RID(first.RID)); err != nil {
		t.Fatal(err)
	}
	second := &doc{Title: "t"}
	mustCreate(t, alice, docs, second)
	if err := docs.Delete(alice, store.RID(second.RID)); err != nil {
		t.Fatal(err)
	}
	third := &doc{Title: "t"}
	mustCreate(t, alice, docs, third)
	if n, _ := docs.Count(alice, where.WithTrashed()); n != 3 {
		t.Fatalf("alice holds %d rows, want two deleted and one live", n)
	}
	if err := docs.Restore(alice, store.RID(first.RID)); !errors.As(err, &dup) {
		t.Fatalf("restore while the slot is taken: %v", err)
	}
	if err := docs.Delete(alice, store.RID(third.RID)); err != nil {
		t.Fatal(err)
	}
	if err := docs.Restore(alice, store.RID(first.RID)); err != nil {
		t.Fatal(err)
	}
	if err := docs.Restore(alice, store.RID(second.RID)); !errors.As(err, &dup) {
		t.Fatalf("a second restore of the value: %v", err)
	}
}

// timed has a DATETIME and a DATE column for the MySQL time baseline.
type timed struct {
	db.SoftDeleteModel
	Label string    `json:"label" store:"query" gorm:"size:20;not null"`
	At    time.Time `json:"at" store:"query" gorm:"not null"`
	Day   time.Time `json:"day" store:"query" gorm:"type:date;not null"`
}

func timedStore(t *testing.T) (*store.Store[timed], *db.DB) {
	t.Helper()
	d := dbtest.Open(t, db.Tables(db.Table(&timed{})))
	return store.New[timed](d), d
}

func storedText(t *testing.T, d *db.DB, expr, label string) string {
	t.Helper()
	var s string
	if err := d.Unsafe(t.Context()).Raw("SELECT "+expr+" FROM timeds WHERE label = ?", label).Row().Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMySQLUTCBaseline_RoundTripAnyZone: whatever zone a time.Time
// carries, MySQL stores the UTC wall clock and reads back the instant in
// time.UTC. The stored text is checked against the column, since a
// symmetric conversion bug would survive a round trip; the location is
// checked by identity, which still tells UTC from Local on a UTC host.
func TestMySQLUTCBaseline_RoundTripAnyZone(t *testing.T) {
	onlyMySQL(t)
	s, d := timedStore(t)
	ctx := t.Context()
	inst := time.Date(2026, 7, 4, 6, 0, 0, 0, time.UTC)
	for name, zone := range map[string]*time.Location{
		"utc": time.UTC, "local": time.Local,
		"east3": time.FixedZone("east3", 3*3600), "kathmandu": time.FixedZone("kathmandu", 5*3600+45*60),
	} {
		mustCreate(t, ctx, s, &timed{Label: name, At: inst.In(zone), Day: inst})
		if got := storedText(t, d, "DATE_FORMAT(at, '%Y-%m-%d %H:%i:%s')", name); got != "2026-07-04 06:00:00" {
			t.Fatalf("%s: stored %q, want the UTC wall clock", name, got)
		}
		got, err := s.Get(ctx, store.Where(where.Eq("label", name)))
		if err != nil || !got.At.Equal(inst) || got.At.Location() != time.UTC {
			t.Fatalf("%s: read back %v (%v), %v", name, got.At, got.At.Location(), err)
		}
	}
}

// TestMySQLUTCBaseline_SoftDeleteSharesTheBaseline: the session runs at
// +00:00 and the deleted_at a soft delete writes sits on the same UTC
// baseline as the created_at gorm stamps.
func TestMySQLUTCBaseline_SoftDeleteSharesTheBaseline(t *testing.T) {
	onlyMySQL(t)
	s, d := timedStore(t)
	ctx := t.Context()
	var zone string
	if err := d.Unsafe(ctx).Raw("SELECT @@session.time_zone").Row().Scan(&zone); err != nil || zone != "+00:00" {
		t.Fatalf("session time_zone %q, %v", zone, err)
	}
	mustCreate(t, ctx, s, &timed{Label: "sd", At: time.Now(), Day: time.Now()})
	if err := s.Delete(ctx, store.Where(where.Eq("label", "sd"))); err != nil {
		t.Fatal(err)
	}
	parse := func(text string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04:05", text, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	created := parse(storedText(t, d, "DATE_FORMAT(created_at, '%Y-%m-%d %H:%i:%s')", "sd"))
	deleted := parse(storedText(t, d, "DATE_FORMAT(deleted_at, '%Y-%m-%d %H:%i:%s')", "sd"))
	if gap := deleted.Sub(created); gap < -time.Minute || gap > time.Minute {
		t.Fatalf("created %v, deleted %v: two baselines", created, deleted)
	}
	if off := time.Since(created); off < -time.Minute || off > time.Minute {
		t.Fatalf("created_at read as UTC is %v from now", off)
	}
}

// TestMySQLUTCBaseline_DateColumnCivilContract: a DATE stores the UTC
// calendar date of the instant: midnight in UTC+8 is the previous day.
func TestMySQLUTCBaseline_DateColumnCivilContract(t *testing.T) {
	onlyMySQL(t)
	s, d := timedStore(t)
	ctx := t.Context()
	utcMidnight := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	east8Midnight := time.Date(2026, 7, 4, 0, 0, 0, 0, time.FixedZone("east8", 8*3600))
	mustCreate(t, ctx, s, &timed{Label: "utc", At: utcMidnight, Day: utcMidnight}, &timed{Label: "east", At: east8Midnight, Day: east8Midnight})
	for label, want := range map[string]string{"utc": "2026-07-04", "east": "2026-07-03"} {
		if got := storedText(t, d, "CAST(day AS CHAR)", label); got != want {
			t.Fatalf("%s: stored %q, want %q", label, got, want)
		}
	}
	got, _ := s.Get(ctx, store.Where(where.Eq("label", "utc")))
	if !got.Day.Equal(utcMidnight) || got.Day.Location() != time.UTC {
		t.Fatalf("read back %v (%v)", got.Day, got.Day.Location())
	}
}

// foreignWriter is a connection chok does not own, in the zone loc and
// without the pinned session.
func foreignWriter(t *testing.T, d *db.DB, loc *time.Location) *sql.DB {
	t.Helper()
	cfg, err := gomysql.ParseDSN(os.Getenv(dbtest.MySQLDSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Unsafe(t.Context()).Raw("SELECT DATABASE()").Row().Scan(&cfg.DBName); err != nil {
		t.Fatal(err)
	}
	cfg.ParseTime, cfg.Loc, cfg.Params = true, loc, nil
	connector, err := gomysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn := sql.OpenDB(connector)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestMySQL_DatetimeStoresTheUTCWallClock: DATETIME keeps a wall clock
// and no zone. chok writes the UTC one, so one instant is one value
// across zones; a writer in another zone stores another wall clock for
// the same instant, which no reader can repair — the invariant the docs
// put on writers chok does not own.
func TestMySQL_DatetimeStoresTheUTCWallClock(t *testing.T) {
	onlyMySQL(t)
	s, d := timedStore(t)
	ctx := t.Context()
	inst := time.Date(2026, 7, 4, 6, 0, 0, 0, time.UTC)
	mustCreate(t, ctx, s, &timed{Label: "w1", At: inst, Day: inst}, &timed{Label: "w2", At: inst.In(time.FixedZone("kathmandu", 5*3600+45*60)), Day: inst})
	if n, err := s.CountDistinct(ctx, "at"); err != nil || n != 1 {
		t.Fatalf("one instant through chok: %d distinct, %v", n, err)
	}
	east := foreignWriter(t, d, time.FixedZone("east3", 3*3600))
	if _, err := east.ExecContext(ctx, "UPDATE timeds SET at = ? WHERE label = 'w2'", inst); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountDistinct(ctx, "at"); err != nil || n != 2 {
		t.Fatalf("a foreign writer three hours east: %d distinct, %v", n, err)
	}
}

// TestMySQL_DSTFold: at the 2026 New York fall-back, 05:30Z and 06:30Z
// are both 01:30 local. chok's UTC baseline keeps them apart even when
// the values carry the New York zone; a writer whose connection runs in
// New York stores them as one wall clock.
func TestMySQL_DSTFold(t *testing.T) {
	onlyMySQL(t)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	s, d := timedStore(t)
	ctx := t.Context()
	before := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC) // 01:30 EDT
	after := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)  // 01:30 EST
	mustCreate(t, ctx, s, &timed{Label: "w1", At: before.In(ny), Day: before}, &timed{Label: "w2", At: after.In(ny), Day: after})
	if n, err := s.CountDistinct(ctx, "at"); err != nil || n != 2 {
		t.Fatalf("through chok: %d distinct, %v", n, err)
	}
	nyWriter := foreignWriter(t, d, ny)
	for label, at := range map[string]time.Time{"w1": before, "w2": after} {
		if _, err := nyWriter.ExecContext(ctx, "UPDATE timeds SET at = ? WHERE label = ?", at, label); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.CountDistinct(ctx, "at"); err != nil || n != 1 {
		t.Fatalf("through a New York connection: %d distinct, %v", n, err)
	}
	if groups, err := s.GroupBy[time.Time](ctx, "at", []store.Aggregate{{Func: "count"}}); err != nil || len(groups) != 1 {
		t.Fatalf("folded groups %v %v", groups, err)
	}
}

package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/log"
	"github.com/zynthara/chok/v2/store/where"
)

// Arch-backlog #17 regression tests: MySQL writes on a UTC baseline,
// pinned on both halves. The driver half (Config.Loc = time.UTC)
// governs the wall clock DATETIME stores and how it parses back; the
// session half (time_zone pinned to +00:00 on every pooled connection)
// governs what the driver cannot reach — SQL-evaluated timestamps like
// the CURRENT_TIMESTAMP soft delete writes into deleted_at, and
// TIMESTAMP-column conversion. Before #17 the driver half rode
// time.Local and the session half rode the server's zone, so
// correctness hung on the deployment environment and the two halves
// forked whenever process and server zones differed.

// TestMySQLUTCBaseline_RoundTripAnyZone pins the driver half: a
// time.Time carrying ANY Go-side zone stores the UTC wall clock and
// reads back as the same instant, in UTC. The stored text is asserted
// against the column itself — a symmetric conversion bug (write +X,
// parse +X) survives a round trip, not this.
func TestMySQLUTCBaseline_RoundTripAnyZone(t *testing.T) {
	s := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	ctx := context.Background()

	inst := time.Date(2026, 7, 4, 6, 0, 0, 0, time.UTC)
	zones := map[string]*time.Location{
		"utc":       time.UTC,
		"local":     time.Local,
		"east3":     time.FixedZone("east3", 3*3600),
		"kathmandu": time.FixedZone("kathmandu", 5*3600+45*60),
	}
	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, zone := range zones {
		if err := s.Create(ctx, &AggSale{Status: name, Qty: 1, Price: 1, Flag: true, At: inst.In(zone)}); err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := gdb.Raw("SELECT DATE_FORMAT(at, '%Y-%m-%d %H:%i:%s') FROM agg_sales WHERE status = ?", name).Row().Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if want := "2026-07-04 06:00:00"; stored != want {
			t.Fatalf("zone %s: stored wall clock = %q, want the UTC wall clock %q", name, stored, want)
		}
		got, err := s.Get(ctx, Where(where.WithFilter("status", name)))
		if err != nil {
			t.Fatal(err)
		}
		if !got.At.Equal(inst) {
			t.Fatalf("zone %s: read back %v, want the instant %v", name, got.At, inst)
		}
		// Location identity, not offset: on a UTC host every wall-clock
		// assertion above degrades to no-signal under a Loc revert
		// (Local and UTC coincide there), and this is then the ONE
		// check that still goes red — the driver reconstructs times
		// with the exact *Location it was configured with, and
		// time.Local is never the time.UTC singleton. Do not soften it
		// to an offset comparison.
		if got.At.Location() != time.UTC {
			t.Fatalf("zone %s: read back in %v, want time.UTC", name, got.At.Location())
		}
	}
}

// TestMySQLUTCBaseline_SoftDeleteSharesDriverBaseline pins the session
// half: time_zone is +00:00 on every pooled connection, so the
// CURRENT_TIMESTAMP that soft delete writes into deleted_at lands on
// the same baseline as the driver-written created_at instead of forking
// onto the server's zone. The wall-clock comparison tolerates clock
// skew (the DB server stamps deleted_at, the app host stamps
// created_at) but not a zone offset.
func TestMySQLUTCBaseline_SoftDeleteSharesDriverBaseline(t *testing.T) {
	s := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	ctx := context.Background()

	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sessionTZ string
	if err := gdb.Raw("SELECT @@session.time_zone").Row().Scan(&sessionTZ); err != nil {
		t.Fatal(err)
	}
	// Load-bearing for the dropped-param regression on a UTC host:
	// stock MySQL reports the literal SYSTEM when no session zone was
	// set (never a resolved offset), so the comparison fires wherever
	// the pin is missing. Known blind spot, accepted: a server booted
	// with default_time_zone=+00:00 would report the pinned value with
	// no pin in place — stock images default to SYSTEM.
	if sessionTZ != "+00:00" {
		t.Fatalf("@@session.time_zone = %q, want the pinned +00:00 (the params SET must reach every pooled connection)", sessionTZ)
	}

	if err := s.Create(ctx, &AggSale{Status: "sd", Qty: 1, Price: 1, Flag: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, Where(where.WithFilter("status", "sd"))); err != nil {
		t.Fatal(err)
	}

	var createdText, deletedText string
	row := gdb.Raw("SELECT DATE_FORMAT(created_at, '%Y-%m-%d %H:%i:%s'), DATE_FORMAT(deleted_at, '%Y-%m-%d %H:%i:%s') FROM agg_sales WHERE status = 'sd'").Row()
	if err := row.Scan(&createdText, &deletedText); err != nil {
		t.Fatal(err)
	}
	parse := func(text string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04:05", text, time.UTC)
		if err != nil {
			t.Fatalf("parse %q: %v", text, err)
		}
		return v
	}
	created, deleted := parse(createdText), parse(deletedText)
	if d := deleted.Sub(created); d < -5*time.Minute || d > 5*time.Minute {
		t.Fatalf("deleted_at %s vs created_at %s: %v apart — the SQL-evaluated and driver baselines have forked", deletedText, createdText, d)
	}
	// And the driver half sits on UTC in absolute terms: read back as
	// UTC, the stored clock must be near now. Under the old Local
	// baseline on a non-UTC host this is off by the whole zone offset.
	if d := time.Since(created); d < -5*time.Minute || d > 5*time.Minute {
		t.Fatalf("created_at %s parsed as UTC is %v from now — not on the UTC baseline", createdText, d)
	}
}

// openMySQLLegacyWriter opens a raw driver connection emulating a
// pre-#17 chok process with BOTH legacy zones pinned explicitly: the
// driver Loc (the old process zone, which formatted every parameter)
// and the session time_zone (the old session/server zone, which
// evaluated CURRENT_TIMESTAMP and interpreted TIMESTAMP-column
// parameters). Pinning both keeps the legacy split deterministic
// regardless of the host or server configuration.
func openMySQLLegacyWriter(t *testing.T, dbName string, loc *time.Location, sessionTZ string) *sql.DB {
	t.Helper()
	cfg, err := gomysql.ParseDSN(os.Getenv(dbtest.MySQLDSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = dbName
	cfg.ParseTime = true
	cfg.Loc = loc
	cfg.Params = map[string]string{"time_zone": "'" + sessionTZ + "'"}
	connector, err := gomysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	conn := sql.OpenDB(connector)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

type legacyRebaseRow struct {
	db.SoftDeleteModel
	Status string     `json:"status" gorm:"size:16;not null"`
	At     time.Time  `json:"at" gorm:"not null"`
	Ts     *time.Time `json:"ts" gorm:"type:timestamp null"`
}

func (legacyRebaseRow) RIDPrefix() string { return "lgr" }
func (legacyRebaseRow) TableName() string { return "legacy_rebase_rows" }

// TestMySQLUTCBaseline_LegacyRebaseRecipe executes the CHANGELOG
// migration recipe end to end against data written the way a pre-#17
// deployment wrote it, with the two legacy zones deliberately split so
// every provenance needs a DIFFERENT rebase: driver-written DATETIME
// carries the old process zone's wall clock (+08:00 here),
// SQL-evaluated DATETIME (deleted_at = CURRENT_TIMESTAMP) carries the
// old session zone's (+05:00), and a parameter-written TIMESTAMP is
// skewed by exactly the difference between the two — the old driver
// formatted the +08 wall clock and the old session interpreted it at
// +05, storing an internal instant 3h late, which the old asymmetric
// read canceled and the new symmetric UTC read exposes.
//
// One TIMESTAMP column deliberately holds THREE provenances: row x
// parameter-written by the legacy process and skewed; row d
// SQL-evaluated the way DEFAULT CURRENT_TIMESTAMP fills it — internal
// instant already correct (the mix chok's own ledger acquired between
// beta.4's DEFAULT-filled applied_at and beta.5's parameter writes);
// and row n parameter-written on the NEW UTC baseline, standing in for
// everything the upgraded version writes once it boots (applied
// migration rows, the manifest updated_at refresh). The recipe's
// TIMESTAMP conversion must select exactly the legacy parameter rows:
// a blanket per-column UPDATE (the pre-round-4 wording) corrupts row d
// by the same 3h it repairs row x with, and a conversion that runs
// after the new version started writing (the pre-round-7 boot-first
// ordering) drags row n off baseline the same way — both fail here.
// Row d also pins the read-side disclosure for correct-instant
// TIMESTAMP values: the OLD asymmetric read showed them skewed by
// (session - process), so the upgrade CORRECTS what the API returns
// for them by that difference — no data migration, visible values
// move.
func TestMySQLUTCBaseline_LegacyRebaseRecipe(t *testing.T) {
	h := dbtest.OpenMySQL(t)
	ctx := context.Background()
	if err := h.Migrate(ctx, db.Table(&legacyRebaseRow{})); err != nil {
		t.Fatal(err)
	}
	s := New[legacyRebaseRow](h, log.Empty(), WithQueryFields("status", "at"))
	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	inst := time.Date(2026, 7, 4, 6, 0, 0, 0, time.UTC)
	for _, status := range []string{"x", "d", "n"} {
		if err := s.Create(ctx, &legacyRebaseRow{Status: status, At: inst}); err != nil {
			t.Fatal(err)
		}
	}
	var dbName string
	if err := gdb.Raw("SELECT DATABASE()").Row().Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	legacy := openMySQLLegacyWriter(t, dbName, time.FixedZone("proc8", 8*3600), "+05:00")
	// Row x: every value the legacy process wrote through parameters,
	// plus the session-evaluated soft delete. Row d: the TIMESTAMP is
	// SQL-evaluated (the DEFAULT CURRENT_TIMESTAMP class) — its
	// internal instant is correct from day one.
	if _, err := legacy.Exec("UPDATE legacy_rebase_rows SET at = ?, ts = ? WHERE status = 'x'", inst, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec("UPDATE legacy_rebase_rows SET deleted_at = CURRENT_TIMESTAMP WHERE status = 'x'"); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec("UPDATE legacy_rebase_rows SET at = ?, ts = CURRENT_TIMESTAMP WHERE status = 'd'", inst); err != nil {
		t.Fatal(err)
	}
	// Row n is written through the chok handle AFTER the baseline
	// switch — a UTC-driver parameter write with the pinned session,
	// exactly what the first boot of the upgraded version produces.
	// Its internal instant is correct and must survive the recipe.
	if err := gdb.Exec("UPDATE legacy_rebase_rows SET ts = ? WHERE status = 'n'", inst).Error; err != nil {
		t.Fatal(err)
	}
	// Row d's DATETIME is then REWRITTEN post-boot on the new baseline
	// — an UPDATE to a pre-upgrade row, the shape of every refreshable
	// column (autoUpdateTime's updated_at, a late soft-delete's
	// deleted_at, a login's last_used_at). Its id/status did not
	// change, so no insert-frontier bound can exclude it: the recipe
	// below must leave the whole row-d DATETIME out, which is why the
	// docs route refreshable columns touched by post-boot traffic to
	// the restore-backup path instead of an id bound.
	if err := gdb.Exec("UPDATE legacy_rebase_rows SET at = ? WHERE status = 'd'", inst).Error; err != nil {
		t.Fatal(err)
	}

	// Pre-recipe pins. Row x: the parameter-written TIMESTAMP's
	// internal instant sits 3h late (process offset minus session
	// offset). Row d: the internal instant is correct — the NEW read
	// returns it, while the OLD asymmetric read (legacy connection)
	// shows it 3h early: session renders the +05 wall clock, Loc
	// parses it at +08. That difference is the API-visible correction
	// the upgrade applies to DEFAULT-written TIMESTAMP values.
	var preTs time.Time
	if err := gdb.Raw("SELECT ts FROM legacy_rebase_rows WHERE status = 'x'").Row().Scan(&preTs); err != nil {
		t.Fatal(err)
	}
	if want := inst.Add(3 * time.Hour); !preTs.Equal(want) {
		t.Fatalf("legacy TIMESTAMP = %v, want the documented %v skew (Loc-formatted wall clock interpreted by a different session zone)", preTs, want)
	}
	var newRead, oldRead time.Time
	if err := gdb.Raw("SELECT ts FROM legacy_rebase_rows WHERE status = 'd'").Row().Scan(&newRead); err != nil {
		t.Fatal(err)
	}
	if err := legacy.QueryRow("SELECT ts FROM legacy_rebase_rows WHERE status = 'd'").Scan(&oldRead); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(newRead); d < -5*time.Minute || d > 5*time.Minute {
		t.Fatalf("correct-instant TIMESTAMP reads %v from now on the new baseline; want the true instant", d)
	}
	if diff := newRead.Sub(oldRead); diff != 3*time.Hour {
		t.Fatalf("old read %v vs new read %v: difference %v, want the 3h correction that undoes the documented (session - process) skew", oldRead, newRead, diff)
	}

	// The recipe (CHANGELOG Breaking entry): driver-written DATETIME by
	// the old process zone; SQL-evaluated DATETIME by the old session
	// zone; parameter-written TIMESTAMP by the difference between the
	// two — selected BY ROW where a column mixes provenances or was
	// touched after the switch (the status predicates stand in for the
	// ledger's provenance marker, the insert frontier that excludes
	// row n, and the no-marker-exists reality of row d's rewritten
	// DATETIME). Converting row d's TIMESTAMP moves its correct
	// instant 3h; converting row n, or row d's rewritten DATETIME,
	// drags a new-baseline value off UTC the same way.
	for _, stmt := range []string{
		"UPDATE legacy_rebase_rows SET at = CONVERT_TZ(at, '+08:00', '+00:00') WHERE at IS NOT NULL AND status = 'x'",
		"UPDATE legacy_rebase_rows SET deleted_at = CONVERT_TZ(deleted_at, '+05:00', '+00:00') WHERE deleted_at IS NOT NULL",
		"UPDATE legacy_rebase_rows SET ts = CONVERT_TZ(ts, '+08:00', '+05:00') WHERE ts IS NOT NULL AND status = 'x'",
	} {
		if err := gdb.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	var at, ts, deletedAt time.Time
	if err := gdb.Raw("SELECT at, ts, deleted_at FROM legacy_rebase_rows WHERE status = 'x'").Row().Scan(&at, &ts, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if !at.Equal(inst) {
		t.Fatalf("rebased DATETIME = %v, want %v", at, inst)
	}
	if !ts.Equal(inst) {
		t.Fatalf("rebased TIMESTAMP = %v, want %v", ts, inst)
	}
	if d := time.Since(deletedAt); d < -5*time.Minute || d > 5*time.Minute {
		t.Fatalf("rebased deleted_at %v is %v from now — the session-zone half of the recipe missed", deletedAt, d)
	}
	// Row d survived the recipe untouched: the SQL-evaluated TIMESTAMP
	// kept its correct instant, and the post-boot REWRITTEN DATETIME —
	// which no insert-frontier bound could have excluded — kept its
	// new-baseline value.
	var dAt, dTs time.Time
	if err := gdb.Raw("SELECT at, ts FROM legacy_rebase_rows WHERE status = 'd'").Row().Scan(&dAt, &dTs); err != nil {
		t.Fatal(err)
	}
	if !dAt.Equal(inst) {
		t.Fatalf("row d rewritten DATETIME = %v, want %v — the recipe converted a value the new version had already corrected", dAt, inst)
	}
	if d := time.Since(dTs); d < -5*time.Minute || d > 5*time.Minute {
		t.Fatalf("row d TIMESTAMP is %v from now after the recipe — the per-row provenance selection failed and corrupted a correct instant", d)
	}
	// Row n — the new-baseline writes — survived both statements: a
	// recipe run after the new version started writing must bound its
	// conversions to pre-upgrade rows or it un-corrects them.
	var nAt, nTs time.Time
	if err := gdb.Raw("SELECT at, ts FROM legacy_rebase_rows WHERE status = 'n'").Row().Scan(&nAt, &nTs); err != nil {
		t.Fatal(err)
	}
	if !nAt.Equal(inst) {
		t.Fatalf("row n DATETIME = %v, want %v — the recipe converted a new-baseline value", nAt, inst)
	}
	if !nTs.Equal(inst) {
		t.Fatalf("row n TIMESTAMP = %v, want %v — the recipe converted a new-baseline value", nTs, inst)
	}
}

// TestMySQLUTCBaseline_NamedZoneNullHazard pins the failure mode the
// recipe's mandatory preflight exists to prevent: CONVERT_TZ does not
// error on an unknown zone or unloaded server tz tables — it returns
// NULL, and on a nullable column that NULL is written silently.
// deleted_at is exactly such a column, so a soft-deleted row whose
// deleted_at becomes NULL is RESURRECTED — business data corruption,
// not bookkeeping noise. An invalid zone name reproduces the
// unloaded-tz-tables behaviour deterministically regardless of the
// server's tz table state. The probe the recipe mandates before any
// named-zone UPDATE is asserted in both directions: NULL for a broken
// zone (abort), non-NULL for a numeric offset (proceed).
func TestMySQLUTCBaseline_NamedZoneNullHazard(t *testing.T) {
	s := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	ctx := context.Background()

	if err := s.Create(ctx, &AggSale{Status: "z", Qty: 1, Price: 1, Flag: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, Where(where.WithFilter("status", "z"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, Where(where.WithFilter("status", "z"))); err == nil {
		t.Fatal("sanity: the soft-deleted row must be invisible")
	}
	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var probe sql.NullString
	if err := gdb.Raw("SELECT CONVERT_TZ('2026-01-01 00:00:00', 'No/Such_Zone', '+00:00')").Row().Scan(&probe); err != nil {
		t.Fatal(err)
	}
	if probe.Valid {
		t.Fatalf("broken-zone probe = %q, want NULL — the abort criterion the recipe's preflight relies on", probe.String)
	}
	if err := gdb.Raw("SELECT CONVERT_TZ('2026-01-01 00:00:00', '+08:00', '+00:00')").Row().Scan(&probe); err != nil {
		t.Fatal(err)
	}
	if !probe.Valid {
		t.Fatal("numeric-offset probe must be non-NULL")
	}
	// Skipping the preflight and running the UPDATE anyway: MySQL
	// reports success, the NULL lands silently, and the soft-deleted
	// row comes back to life.
	if err := gdb.Exec("UPDATE agg_sales SET deleted_at = CONVERT_TZ(deleted_at, 'No/Such_Zone', '+00:00') WHERE deleted_at IS NOT NULL").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, Where(where.WithFilter("status", "z"))); err != nil {
		t.Fatalf("expected the documented resurrection — soft-deleted row visible after the silent NULL; got %v", err)
	}
}

// TestMySQLUTCBaseline_OutOfRangeUnconverted pins the second silent
// CONVERT_TZ failure mode the recipe's per-column scan exists to
// catch: values outside CONVERT_TZ's supported span (roughly the
// 64-bit TIMESTAMP range — pre-epoch history, far-future dates) are
// neither converted nor NULLed nor errored — they are returned
// UNCHANGED, so the UPDATE reports success while the row silently
// keeps its old wall clock. DATETIME itself spans years 1000-9999, so
// such rows are perfectly storable. The unconverted-row detector the
// recipe mandates (converted = original, valid whenever the two zones
// differ) and the fixed-offset fallback (interval arithmetic, exact
// across the whole DATETIME range) are both asserted here.
func TestMySQLUTCBaseline_OutOfRangeUnconverted(t *testing.T) {
	s := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	ctx := context.Background()

	old := time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Create(ctx, &AggSale{Status: "pre", Qty: 1, Price: 1, Flag: true, At: old}); err != nil {
		t.Fatal(err)
	}
	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The recipe's unconverted-row detector fires on this row before
	// any UPDATE runs.
	var stuck int64
	if err := gdb.Raw("SELECT COUNT(*) FROM agg_sales WHERE at IS NOT NULL AND CONVERT_TZ(at, '+08:00', '+00:00') = at").Row().Scan(&stuck); err != nil {
		t.Fatal(err)
	}
	if stuck != 1 {
		t.Fatalf("unconverted-row scan = %d, want 1 — the pre-epoch row must be flagged before the UPDATE", stuck)
	}
	// Running the blanket UPDATE anyway: success reported, value
	// untouched — the silent skip the scan exists to prevent.
	if err := gdb.Exec("UPDATE agg_sales SET at = CONVERT_TZ(at, '+08:00', '+00:00') WHERE at IS NOT NULL").Error; err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := gdb.Raw("SELECT at FROM agg_sales WHERE status = 'pre'").Row().Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(old) {
		t.Fatalf("out-of-range value = %v, want it returned unchanged (%v) — CONVERT_TZ must not have touched it", after, old)
	}
	// The fixed-offset fallback is exact across the whole DATETIME
	// range: interval arithmetic lands the true instant.
	if err := gdb.Exec("UPDATE agg_sales SET at = DATE_SUB(at, INTERVAL '8:00' HOUR_MINUTE) WHERE status = 'pre'").Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Raw("SELECT at FROM agg_sales WHERE status = 'pre'").Row().Scan(&after); err != nil {
		t.Fatal(err)
	}
	if want := old.Add(-8 * time.Hour); !after.Equal(want) {
		t.Fatalf("interval-rebased value = %v, want %v", after, want)
	}

	// West of UTC the direction inverts, and the SIGNED interval is
	// what keeps the template correct: DATE_SUB with a negative
	// interval ADDS — for an old zone of -05:00 the true instant is
	// the wall clock plus 5h, the direction CONVERT_TZ would have
	// encoded automatically. Copying the east template's positive
	// interval here would subtract instead and land 10h wrong.
	if err := s.Create(ctx, &AggSale{Status: "wst", Qty: 1, Price: 1, Flag: true, At: old}); err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec("UPDATE agg_sales SET at = DATE_SUB(at, INTERVAL '-5:00' HOUR_MINUTE) WHERE status = 'wst'").Error; err != nil {
		t.Fatal(err)
	}
	var west time.Time
	if err := gdb.Raw("SELECT at FROM agg_sales WHERE status = 'wst'").Row().Scan(&west); err != nil {
		t.Fatal(err)
	}
	if want := old.Add(5 * time.Hour); !west.Equal(want) {
		t.Fatalf("west-offset interval rebase = %v, want %v — the signed (negative) interval must ADD", west, want)
	}
}

// TestMySQLUTCBaseline_InvalidDateNullsBothPaths pins why the recipe
// splits the scan's two arms instead of routing every flagged row to
// the interval fallback: a zero or otherwise invalid stored date makes
// CONVERT_TZ return NULL even under two valid numeric offsets — and
// DATE_SUB returns NULL on the very same value, so the fallback cannot
// rescue an IS NULL row; on a nullable column it would swallow the
// NULL silently (the resurrection door again), on a NOT NULL column it
// fails mid-batch. IS NULL rows abort to manual classification.
func TestMySQLUTCBaseline_InvalidDateNullsBothPaths(t *testing.T) {
	s := setupAggStoreOn(t, dbtest.OpenMySQL(t))
	ctx := context.Background()

	if err := s.Create(ctx, &AggSale{Status: "zed", Qty: 1, Price: 1, Flag: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var dbName string
	if err := gdb.Raw("SELECT DATABASE()").Row().Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	// A legacy-era connection with strictness off: how zero dates got
	// into old databases in the first place.
	cfg, err := gomysql.ParseDSN(os.Getenv(dbtest.MySQLDSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = dbName
	cfg.Params = map[string]string{"sql_mode": "''"}
	connector, err := gomysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lax := sql.OpenDB(connector)
	t.Cleanup(func() { _ = lax.Close() })
	if _, err := lax.Exec("UPDATE agg_sales SET at = '0000-00-00 00:00:00' WHERE status = 'zed'"); err != nil {
		t.Fatal(err)
	}

	// Both paths NULL on the same value: the scan's IS NULL arm is the
	// only thing standing between this row and a silently swallowed
	// NULL.
	var convNull, subNull bool
	row := gdb.Raw("SELECT CONVERT_TZ(at, '+08:00', '+00:00') IS NULL, DATE_SUB(at, INTERVAL '8:00' HOUR_MINUTE) IS NULL FROM agg_sales WHERE status = 'zed'").Row()
	if err := row.Scan(&convNull, &subNull); err != nil {
		t.Fatal(err)
	}
	if !convNull {
		t.Fatal("CONVERT_TZ on a zero date must be NULL even under valid numeric offsets")
	}
	if !subNull {
		t.Fatal("DATE_SUB on the same zero date must be NULL too — the interval fallback cannot rescue IS NULL rows")
	}
}

// TestMySQLUTCBaseline_EpochEdgeTimestampFallback pins the step-3
// fallback's own bound: the target is a TIMESTAMP column whose
// storable range is 1970-2038 — narrower than DATETIME's. A near-epoch
// value can sit unmoved by CONVERT_TZ (its UTC intermediate leaves the
// supported span, the silent skip the scan detects) while the signed
// interval's result falls BELOW what the column can store: strict mode
// rejects the write-back loudly. Those rows go to manual handling, not
// the fallback.
func TestMySQLUTCBaseline_EpochEdgeTimestampFallback(t *testing.T) {
	h := dbtest.OpenMySQL(t)
	ctx := context.Background()
	if err := h.Migrate(ctx, db.Table(&legacyRebaseRow{})); err != nil {
		t.Fatal(err)
	}
	s := New[legacyRebaseRow](h, log.Empty(), WithQueryFields("status", "at"))
	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, &legacyRebaseRow{Status: "ep", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Internal instant 1970-01-01 02:00Z: storable in TIMESTAMP, but
	// CONVERT_TZ('+08:00','+05:00') routes through a pre-epoch UTC
	// intermediate and returns it unchanged — the detector fires.
	edge := time.Date(1970, 1, 1, 2, 0, 0, 0, time.UTC)
	if err := gdb.Exec("UPDATE legacy_rebase_rows SET ts = ? WHERE status = 'ep'", edge).Error; err != nil {
		t.Fatal(err)
	}
	var stuck int64
	if err := gdb.Raw("SELECT COUNT(*) FROM legacy_rebase_rows WHERE ts IS NOT NULL AND CONVERT_TZ(ts, '+08:00', '+05:00') = ts").Row().Scan(&stuck); err != nil {
		t.Fatal(err)
	}
	if stuck != 1 {
		t.Fatalf("near-epoch TIMESTAMP scan = %d, want 1 — CONVERT_TZ must return it unmoved", stuck)
	}
	// The signed-interval result (1969-12-31 23:00) is below the
	// column's own range: strict mode rejects the write-back — the
	// loud signal that this row belongs to manual handling.
	err = gdb.Exec("UPDATE legacy_rebase_rows SET ts = DATE_SUB(ts, INTERVAL '3:00' HOUR_MINUTE) WHERE status = 'ep'").Error
	if err == nil {
		t.Fatal("expected the strict-mode range rejection: the fallback result is below TIMESTAMP's storable range")
	}
}

type civilDateRow struct {
	db.Model
	Status string    `json:"status" gorm:"size:16;not null"`
	D      time.Time `json:"d" gorm:"type:date;not null"`
}

func (civilDateRow) RIDPrefix() string { return "cvd" }
func (civilDateRow) TableName() string { return "civil_date_rows" }

// TestMySQLUTCBaseline_DateColumnCivilContract pins the DATE-column
// side of the UTC baseline: the driver converts every time.Time
// parameter to UTC before the server truncates it to a date, so the
// stored civil date is the UTC calendar date of the instant. Construct
// date-only values at UTC midnight; a local midnight east of UTC lands
// on the PREVIOUS UTC day (2026-07-04 00:00 at +08:00 is 2026-07-03
// 16:00Z — the pre-#17 Local baseline stored 2026-07-04 for it). Reads
// come back as UTC midnight of the stored date.
func TestMySQLUTCBaseline_DateColumnCivilContract(t *testing.T) {
	h := dbtest.OpenMySQL(t)
	ctx := context.Background()
	if err := h.Migrate(ctx, db.Table(&civilDateRow{})); err != nil {
		t.Fatal(err)
	}
	s := New[civilDateRow](h, log.Empty(), WithQueryFields("status", "d"))
	gdb, err := s.Unsafe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	utcMidnight := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	east8Midnight := time.Date(2026, 7, 4, 0, 0, 0, 0, time.FixedZone("east8", 8*3600))
	for status, d := range map[string]time.Time{"utc": utcMidnight, "east": east8Midnight} {
		if err := s.Create(ctx, &civilDateRow{Status: status, D: d}); err != nil {
			t.Fatal(err)
		}
	}
	for status, want := range map[string]string{"utc": "2026-07-04", "east": "2026-07-03"} {
		var stored string
		if err := gdb.Raw("SELECT CAST(d AS CHAR) FROM civil_date_rows WHERE status = ?", status).Row().Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != want {
			t.Fatalf("status %s: stored date %q, want %q", status, stored, want)
		}
	}
	got, err := s.Get(ctx, Where(where.WithFilter("status", "utc")))
	if err != nil {
		t.Fatal(err)
	}
	if !got.D.Equal(utcMidnight) || got.D.Location() != time.UTC {
		t.Fatalf("read back %v (%v), want %v in UTC", got.D, got.D.Location(), utcMidnight)
	}
}

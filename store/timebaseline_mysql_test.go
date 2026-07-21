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
// One TIMESTAMP column deliberately holds BOTH provenances (row x
// parameter-written and skewed; row d SQL-evaluated the way DEFAULT
// CURRENT_TIMESTAMP fills it — internal instant already correct, the
// mix chok's own ledger acquired between beta.4's DEFAULT-filled
// applied_at and beta.5's parameter writes), so the recipe's TIMESTAMP
// conversion must select rows by provenance: a blanket per-column
// UPDATE (the pre-round-4 wording) corrupts row d by the same 3h it
// repairs row x with, and fails here. Row d also pins the read-side
// disclosure for correct-instant TIMESTAMP values: the OLD asymmetric
// read showed them skewed by (session - process), so the upgrade
// CORRECTS what the API returns for them by that difference — no data
// migration, visible values move.
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
	for _, status := range []string{"x", "d"} {
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
	// two — selected BY ROW where the column mixes provenances (the
	// status predicate here stands in for the ledger's provenance
	// marker). Converting row d too would move its correct instant 3h.
	for _, stmt := range []string{
		"UPDATE legacy_rebase_rows SET at = CONVERT_TZ(at, '+08:00', '+00:00') WHERE at IS NOT NULL",
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
	// Row d survived the recipe untouched: still the correct instant.
	var dAt, dTs time.Time
	if err := gdb.Raw("SELECT at, ts FROM legacy_rebase_rows WHERE status = 'd'").Row().Scan(&dAt, &dTs); err != nil {
		t.Fatal(err)
	}
	if !dAt.Equal(inst) {
		t.Fatalf("row d DATETIME = %v, want %v", dAt, inst)
	}
	if d := time.Since(dTs); d < -5*time.Minute || d > 5*time.Minute {
		t.Fatalf("row d TIMESTAMP is %v from now after the recipe — the per-row provenance selection failed and corrupted a correct instant", d)
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

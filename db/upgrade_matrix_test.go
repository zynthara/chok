package db_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/db"
	"github.com/zynthara/chok/v2/db/dbtest"
	"github.com/zynthara/chok/v2/internal/testseq"
)

const syntheticTable = "synthetic_battery"

type syntheticBattery struct {
	ID          uint           `gorm:"primaryKey;autoIncrement"`
	Code        string         `gorm:"size:64;not null"`
	DeletedAt   gorm.DeletedAt `gorm:"index"`
	DeleteToken string         `gorm:"size:24;not null;default:''"`
	Payload     datatypes.JSON `gorm:"type:json"`
	Migrated    string         `gorm:"size:32;not null;default:''"`
}

func (syntheticBattery) TableName() string { return syntheticTable }

func TestMigrationBehavior_SyntheticUpgradeMatrix(t *testing.T) {
	runSyntheticUpgradeMatrix(t, dbtest.Open)
}

func TestMigrationBehavior_MySQLSyntheticUpgradeMatrix(t *testing.T) {
	runSyntheticUpgradeMatrix(t, dbtest.OpenMySQL)
}

func runSyntheticUpgradeMatrix(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	assets := syntheticMigrationFS(false, "")
	prefix := syntheticSequence(t, "synthetic_upgrade", testseq.PrefixFS(t, assets, 1), db.Baseline{})

	baselineDB := open(t)
	if _, err := db.ApplySequence(t.Context(), baselineDB, prefix); err != nil {
		t.Fatalf("synthetic baseline apply: %v", err)
	}
	fingerprint, err := db.SchemaFingerprint(t.Context(), baselineDB, []string{syntheticTable})
	if err != nil {
		t.Fatalf("synthetic baseline fingerprint: %v", err)
	}
	full := syntheticSequence(t, "synthetic_upgrade", assets, db.Baseline{
		EquivalentVersion: 1,
		Tables:            []string{syntheticTable},
		Fingerprints: map[string]string{
			"sqlite": fingerprint, "mysql": fingerprint, "postgres": fingerprint,
		},
	})

	testseq.RunUpgradeMatrix(t, open, full, prefix, testseq.UpgradeSpec{
		Seed:          seedSyntheticLegacy,
		VerifyUpgrade: verifySyntheticLegacy,
		Trace: func(t testing.TB, h *db.DB) testseq.Trace {
			return syntheticBehaviorTrace(t, h, true)
		},
		PrepareAdoptable: func(t testing.TB, h *db.DB) {
			t.Helper()
			if _, err := db.ApplySequence(t.Context(), h, prefix); err != nil {
				t.Fatalf("synthetic prepare prefix: %v", err)
			}
			gdb := h.Unsafe(t.Context())
			if err := gdb.Exec("DELETE FROM schema_migrations_chok_manifest WHERE kind = ?", prefix.Kind()).Error; err != nil {
				t.Fatalf("synthetic remove test manifest claim: %v", err)
			}
			if err := gdb.Migrator().DropTable(prefix.Ledger()); err != nil {
				t.Fatalf("synthetic remove test ledger: %v", err)
			}
			seedSyntheticLegacy(t, h)
		},
	})
}

func TestMigrationBehavior_PostgresNegativeSoftUnique(t *testing.T) {
	if dbtest.Driver() != "postgres" {
		t.Skip("postgres lane only")
	}
	assertSyntheticNegativeSoftUnique(t, dbtest.Open, "postgres")
}

func TestMigrationBehavior_MySQLNegativeSoftUnique(t *testing.T) {
	assertSyntheticNegativeSoftUnique(t, dbtest.OpenMySQL, "mysql")
}

func assertSyntheticNegativeSoftUnique(t *testing.T, open func(testing.TB) *db.DB, badDialect string) {
	t.Helper()
	goodFS := testseq.PrefixFS(t, syntheticMigrationFS(false, ""), 1)
	badFS := testseq.PrefixFS(t, syntheticMigrationFS(true, badDialect), 1)
	good := syntheticSequence(t, "synthetic_good", goodFS, db.Baseline{})
	bad := syntheticSequence(t, "synthetic_bad", badFS, db.Baseline{})
	goodDB := open(t)
	badDB := open(t)
	if _, err := db.ApplySequence(t.Context(), goodDB, good); err != nil {
		t.Fatalf("synthetic good DDL: %v", err)
	}
	if _, err := db.ApplySequence(t.Context(), badDB, bad); err != nil {
		t.Fatalf("synthetic bad DDL: %v", err)
	}
	goodTrace := syntheticBehaviorTrace(t, goodDB, true)
	badTrace := syntheticBehaviorTrace(t, badDB, false)
	if testseq.EqualTrace(goodTrace, badTrace) {
		t.Fatal("synthetic bad SoftUnique DDL produced the good behavior trace")
	}
	goodReclaim, okGood := traceObservation(goodTrace, "soft_unique_reclaim")
	badReclaim, okBad := traceObservation(badTrace, "soft_unique_reclaim")
	if !okGood || !okBad || !goodReclaim.OK || badReclaim.OK || badReclaim.Error != "duplicate" {
		t.Fatalf("synthetic negative split is not the reclaim step: good=%+v bad=%+v", goodReclaim, badReclaim)
	}
}

func syntheticSequence(t testing.TB, kind string, migrations fs.FS, baseline db.Baseline) db.Sequence {
	t.Helper()
	seq, err := db.OwnedSequence(
		kind,
		migrations,
		baseline,
		db.SequenceOwner("github.com/zynthara/chok/v2/dbtest/"+kind),
	)
	if err != nil {
		t.Fatalf("construct synthetic sequence %s: %v", kind, err)
	}
	return seq
}

func syntheticMigrationFS(bad bool, badDialect string) fs.FS {
	sqliteIndex := "CREATE UNIQUE INDEX uk_synthetic_code ON synthetic_battery (code, delete_token);"
	postgresIndex := "CREATE UNIQUE INDEX uk_synthetic_code ON synthetic_battery (code) WHERE deleted_at IS NULL;"
	mysqlIndex := "UNIQUE KEY uk_synthetic_code (code, delete_token),"
	if bad && badDialect == "postgres" {
		postgresIndex = "CREATE UNIQUE INDEX uk_synthetic_code ON synthetic_battery (code);"
	}
	if bad && badDialect == "mysql" {
		mysqlIndex = "UNIQUE KEY uk_synthetic_code (code),"
	}
	return fstest.MapFS{
		"sqlite/0001_init.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE synthetic_battery (
  id integer PRIMARY KEY AUTOINCREMENT,
  code text NOT NULL,
  deleted_at datetime,
  delete_token text NOT NULL DEFAULT '',
  payload JSON
);
` + sqliteIndex)},
		"sqlite/0002_add_migrated.sql": &fstest.MapFile{Data: []byte(`
ALTER TABLE synthetic_battery ADD COLUMN migrated text NOT NULL DEFAULT '';
UPDATE synthetic_battery SET migrated = 'backfilled' WHERE migrated = '';
`)},
		"postgres/0001_init.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE synthetic_battery (
  id bigserial PRIMARY KEY,
  code varchar(64) NOT NULL,
  deleted_at timestamptz,
  delete_token varchar(24) NOT NULL DEFAULT '',
  payload jsonb
);
` + postgresIndex)},
		"postgres/0002_add_migrated.sql": &fstest.MapFile{Data: []byte(`
ALTER TABLE synthetic_battery ADD COLUMN migrated varchar(32) NOT NULL DEFAULT '';
UPDATE synthetic_battery SET migrated = 'backfilled' WHERE migrated = '';
`)},
		"mysql/0001_init.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE synthetic_battery (
  id bigint unsigned NOT NULL AUTO_INCREMENT,
  code varchar(64) NOT NULL,
  deleted_at datetime(3) DEFAULT NULL,
  delete_token varchar(24) NOT NULL DEFAULT '',
  payload json DEFAULT NULL,
  ` + mysqlIndex + `
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
`)},
		"mysql/0002_add_migrated.sql": &fstest.MapFile{Data: []byte(`
ALTER TABLE synthetic_battery ADD COLUMN migrated varchar(32) NOT NULL DEFAULT '';
UPDATE synthetic_battery SET migrated = 'backfilled' WHERE migrated = '';
`)},
	}
}

func seedSyntheticLegacy(t testing.TB, h *db.DB) {
	t.Helper()
	if err := h.Unsafe(t.Context()).Exec(
		"INSERT INTO synthetic_battery (code, delete_token, payload) VALUES (?, '', ?)",
		"legacy-row", `{"legacy":true}`,
	).Error; err != nil {
		t.Fatalf("seed synthetic legacy row: %v", err)
	}
}

func verifySyntheticLegacy(t testing.TB, h *db.DB) {
	t.Helper()
	var migrated string
	if err := h.Unsafe(t.Context()).Raw(
		"SELECT migrated FROM synthetic_battery WHERE code = ?", "legacy-row",
	).Scan(&migrated).Error; err != nil {
		t.Fatalf("read synthetic migrated value: %v", err)
	}
	if migrated != "backfilled" {
		t.Fatalf("synthetic legacy row migrated = %q, want backfilled", migrated)
	}
}

func syntheticBehaviorTrace(t testing.TB, h *db.DB, enforceContract bool) testseq.Trace {
	t.Helper()
	gdb := h.Unsafe(t.Context())
	trace := testseq.Trace{}
	var priorMax uint
	if err := gdb.Unscoped().Model(&syntheticBattery{}).Select("COALESCE(MAX(id), 0)").Scan(&priorMax).Error; err != nil {
		t.Fatalf("synthetic behavior read prior auto-increment frontier: %v", err)
	}
	first := &syntheticBattery{Code: "matrix-slot", Payload: datatypes.JSON(`{"emoji":"🔋","nested":{"nil":null}}`)}
	if err := gdb.Omit("migrated").Create(first).Error; err != nil {
		t.Fatalf("synthetic behavior create first row: %v", err)
	}
	continued := first.ID > priorMax
	if enforceContract && !continued {
		t.Fatal("synthetic first new ID did not continue past stored frontier")
	}
	trace = append(trace, testseq.Observation{Step: "auto_increment_continuation", OK: continued})
	duplicate := &syntheticBattery{Code: first.Code}
	err := gdb.Omit("migrated").Create(duplicate).Error
	trace = append(trace, syntheticWriteObservation(t, "soft_unique_live_duplicate", err))
	if enforceContract && err == nil {
		t.Fatal("synthetic live duplicate unexpectedly succeeded")
	}
	if err := gdb.Model(&syntheticBattery{}).Where("id = ?", first.ID).Updates(map[string]any{
		"deleted_at": gorm.Expr("CURRENT_TIMESTAMP"), "delete_token": "deletedSlot",
	}).Error; err != nil {
		t.Fatalf("synthetic behavior soft delete: %v", err)
	}
	reclaimed := &syntheticBattery{Code: first.Code}
	err = gdb.Omit("migrated").Create(reclaimed).Error
	trace = append(trace, syntheticWriteObservation(t, "soft_unique_reclaim", err))
	if enforceContract && err != nil {
		t.Fatalf("synthetic reclaimed slot failed: %v", err)
	}

	ids := make([]uint, 0, 3)
	for i := range 3 {
		row := &syntheticBattery{Code: "matrix-auto-" + string(rune('a'+i))}
		if err := gdb.Omit("migrated").Create(row).Error; err != nil {
			t.Fatalf("synthetic behavior auto row %d: %v", i, err)
		}
		ids = append(ids, row.ID)
	}
	strict := ids[0] < ids[1] && ids[1] < ids[2]
	if enforceContract && !strict {
		t.Fatal("synthetic auto IDs are not strictly increasing")
	}
	trace = append(trace, testseq.Observation{Step: "auto_increment", OK: strict, Rank: 3})

	var got syntheticBattery
	if err := gdb.Unscoped().Where("id = ?", first.ID).First(&got).Error; err != nil {
		t.Fatalf("synthetic behavior read JSON row: %v", err)
	}
	trace = append(trace, testseq.Observation{Step: "json_round_trip", OK: true, JSON: testseq.CanonicalJSON(t, got.Payload)})
	return trace
}

// syntheticWriteObservation classifies a raw-GORM write outcome for the
// trace. The synthetic battery deliberately bypasses the store layer, so it
// mirrors store's tiered duplicate detection here. A non-duplicate error is a
// fixture bug on every path — the negative DDL variants also fail as unique
// violations — so it fails the test instead of polluting the trace.
func syntheticWriteObservation(t testing.TB, step string, err error) testseq.Observation {
	t.Helper()
	if err == nil {
		return testseq.Observation{Step: step, OK: true}
	}
	var pgErr *pgconn.PgError
	msg := strings.ToLower(err.Error())
	duplicate := errors.Is(err, gorm.ErrDuplicatedKey) ||
		(errors.As(err, &pgErr) && pgErr.Code == "23505") ||
		strings.Contains(msg, "duplicate") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "constraint failed")
	if !duplicate {
		t.Fatalf("%s: non-duplicate write error: %v", step, err)
	}
	return testseq.Observation{Step: step, Error: "duplicate"}
}

func traceObservation(trace testseq.Trace, step string) (testseq.Observation, bool) {
	for _, observation := range trace {
		if observation.Step == step {
			return observation, true
		}
	}
	return testseq.Observation{}, false
}

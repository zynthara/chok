package db

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Versioned migrations (SPEC §5.3) are embedded, forward-only,
// sequence-numbered *.sql files. The schema_migrations ledger records
// checksums and a crash-persistent dirty marker before any migration SQL
// runs. A temporary version-zero fence makes old chok binaries fail closed
// while a migration is active or unresolved. PostgreSQL and SQLite keep a
// file's SQL plus clean transition atomic; MySQL retains the dirty marker
// across its auto-committing DDL so an operator can resolve partial effects.

// ledgerTable is the migration ledger. It is also a member of the
// framework-table whitelist below.
const ledgerTable = "schema_migrations"

const (
	migrationFenceVersion int64 = 0
	migrationFenceName          = "__chok_migration_fence__"
	sqliteLeasePrefix           = "__chok_migration_lease__:"
	// sqliteLeaseTTL only bounds unlocked gaps between ledger transactions;
	// transaction-end refresh makes migration-file runtime irrelevant.
	sqliteLeaseTTL          = 30 * time.Second
	sqliteLeasePoll         = 100 * time.Millisecond
	migrationCleanupTimeout = 5 * time.Second
	maxMigrationErrorRunes  = 4096
)

// FrameworkTables lists the tables chok batteries own and keep
// managing via AutoMigrate even under migrate: versioned — the
// documented whitelist exemption (SPEC §5.3, review H2). Under
// migrate: off nothing is created, these included. The forward-only
// audit guarantee of versioned mode therefore covers application
// tables; framework tables evolve with the framework version.
func FrameworkTables() []string {
	return []string{"users", "identities", "audit_logs", "casbin_rule", ledgerTable}
}

// Migration is one parsed, not-necessarily-applied migration file.
type Migration struct {
	Version  int64
	Name     string
	File     string
	SQL      string
	Checksum string
}

// AppliedMigration is one ledger row.
type AppliedMigration struct {
	Version    int64
	Name       string
	AppliedAt  time.Time
	Checksum   string
	StartedAt  time.Time
	FinishedAt time.Time
	Dirty      bool
	LastError  string
}

// ChecksumDrift is an applied or dirty ledger row whose current file bytes
// no longer match the checksum recorded when that attempt began.
type ChecksumDrift struct {
	Version int64
	File    string
	Ledger  string
	Current string
}

// MigrationNameDrift is a version reused under a different filename stem.
// Names are part of migration identity even when the SQL bytes are unchanged.
type MigrationNameDrift struct {
	Version    int64
	LedgerName string
	FileName   string
	File       string
}

// MigrationFenceStatus describes the internal version-zero compatibility
// fence. It is normally transient; a fence without dirty rows means an up or
// repair is active, or a crashed operation awaits lease takeover.
type MigrationFenceStatus struct {
	Owner      string
	AcquiredAt time.Time
}

// MigrationStatus is the read-only view chok migrate status renders.
type MigrationStatus struct {
	Applied    []AppliedMigration
	Pending    []Migration
	Dirty      []AppliedMigration
	Drift      []ChecksumDrift
	Missing    []AppliedMigration
	Unverified []AppliedMigration
	OutOfOrder []Migration
	NameDrift  []MigrationNameDrift
	Fence      *MigrationFenceStatus
	// FrameworkTables echoes the AutoMigrate-exempt whitelist so every
	// status surface presents it honestly next to the ledger.
	FrameworkTables []string
}

// Clean reports whether the database and migration set agree completely.
// Pending and unverified legacy rows are intentionally not clean so the same
// predicate can gate deployments.
func (s *MigrationStatus) Clean() bool {
	return len(s.Pending) == 0 && len(s.Dirty) == 0 && len(s.Drift) == 0 &&
		len(s.Missing) == 0 && len(s.Unverified) == 0 &&
		len(s.OutOfOrder) == 0 && len(s.NameDrift) == 0 && s.Fence == nil
}

// ApplyReport records both migrations executed now and legacy ledger rows
// whose trust-on-first-use checksum baseline was established by this run.
type ApplyReport struct {
	Applied []Migration
	Adopted []AppliedMigration
}

// RepairAction is one explicit resolution of a dirty or drifted migration.
type RepairAction string

const (
	// RepairRetry clears a dirty attempt after the operator restored the
	// database to its pre-migration state; the next up reruns the file.
	RepairRetry RepairAction = "retry"
	// RepairMarkApplied marks a dirty attempt complete after the operator
	// verified or manually completed every intended effect.
	RepairMarkApplied RepairAction = "mark-applied"
	// RepairAcceptDrift rebaselines a clean ledger row to the current file.
	RepairAcceptDrift RepairAction = "accept-drift"
)

// RepairOptions scopes a repair to one version and uses ExpectedChecksum as
// a compare-and-swap guard against resolving state other than what the
// operator inspected. Reason is mandatory and should be persisted by the
// caller's operational audit sink.
type RepairOptions struct {
	Action           RepairAction
	Version          int64
	ExpectedChecksum string
	Reason           string
}

// RepairReport is the structured evidence emitted for one repair action.
// Chok deliberately does not add a second framework-owned history table in
// v2; callers that require durable compliance history persist this report.
type RepairReport struct {
	Action          RepairAction
	Version         int64
	File            string
	LedgerChecksum  string
	CurrentChecksum string
	Reason          string
	ResolvedAt      time.Time
}

// migFileRe: <version>_<name>.sql — version is a positive decimal
// sequence number (padding optional), name is free-form.
var migFileRe = regexp.MustCompile(`^(\d+)_(.+)\.sql$`)

// LoadMigrations parses the root of fsys. Every *.sql file must match
// NNNN_name.sql (fail-fast on strays — a typo'd file silently skipped
// is a missing migration in production); versions must be unique.
// Subdirectories and non-.sql files are ignored. The result is sorted
// by version.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("db: read migrations dir: %w", err)
	}
	var out []Migration
	seen := make(map[int64]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 4 || name[len(name)-4:] != ".sql" {
			continue
		}
		m := migFileRe.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("db: migration file %q does not match <version>_<name>.sql", name)
		}
		ver, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || ver <= 0 {
			return nil, fmt.Errorf("db: migration file %q has invalid version %q", name, m[1])
		}
		if prev, dup := seen[ver]; dup {
			return nil, fmt.Errorf("db: duplicate migration version %d (%s and %s)", ver, prev, name)
		}
		seen[ver] = name
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("db: read migration %s: %w", name, err)
		}
		normalized := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		sum := sha256.Sum256(normalized)
		out = append(out, Migration{
			Version:  ver,
			Name:     m[2],
			File:     name,
			SQL:      string(raw),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// ensureLedgerBase creates the complete current ledger for a fresh database.
// Existing three-column ledgers are left untouched here so their additive
// upgrade can happen only after the migration lock is held.
func ensureLedgerBase(gdb *gorm.DB) error {
	return gdb.Exec(
		"CREATE TABLE IF NOT EXISTS " + ledgerTable + " (" +
			"version BIGINT PRIMARY KEY, " +
			"name VARCHAR(255) NOT NULL, " +
			"applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, " +
			"checksum VARCHAR(64) NOT NULL DEFAULT '', " +
			"started_at TIMESTAMP NULL, " +
			"finished_at TIMESTAMP NULL, " +
			"dirty BOOLEAN NOT NULL DEFAULT FALSE, " +
			"last_error TEXT)",
	).Error
}

// ensureLedgerColumns upgrades a legacy ledger additively. Callers must hold
// the migration lock: HasColumn plus ALTER is idempotent across retries, not
// an atomic concurrency primitive.
func ensureLedgerColumns(gdb *gorm.DB) error {
	columns := []struct {
		name string
		ddl  string
	}{
		{"checksum", "VARCHAR(64) NOT NULL DEFAULT ''"},
		{"started_at", "TIMESTAMP NULL"},
		{"finished_at", "TIMESTAMP NULL"},
		{"dirty", "BOOLEAN NOT NULL DEFAULT FALSE"},
		{"last_error", "TEXT"},
	}
	for _, col := range columns {
		if gdb.Migrator().HasColumn(ledgerTable, col.name) {
			continue
		}
		if err := gdb.Exec("ALTER TABLE " + ledgerTable + " ADD COLUMN " + col.name + " " + col.ddl).Error; err != nil {
			return fmt.Errorf("db: upgrade %s add %s: %w", ledgerTable, col.name, err)
		}
	}
	return nil
}

// appliedMigrations reads every real migration row while tolerating the old
// three-column ledger and partially-completed additive upgrades. Version zero
// is the internal compatibility fence and is deliberately invisible here.
func appliedMigrations(gdb *gorm.DB) ([]AppliedMigration, error) {
	expr := func(column, fallback string) string {
		if gdb.Migrator().HasColumn(ledgerTable, column) {
			return column
		}
		return fallback
	}
	query := "SELECT version, name, applied_at, " +
		expr("checksum", "''") + ", " +
		expr("started_at", "NULL") + ", " +
		expr("finished_at", "NULL") + ", " +
		expr("dirty", "FALSE") + ", " +
		expr("last_error", "NULL") +
		" FROM " + ledgerTable + " WHERE version > 0 ORDER BY version"
	rows, err := gdb.Raw(query).Rows()
	if err != nil {
		return nil, fmt.Errorf("db: read %s: %w", ledgerTable, err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var (
			a        AppliedMigration
			checksum sql.NullString
			started  sql.NullTime
			finished sql.NullTime
			dirty    sql.NullBool
			lastErr  sql.NullString
		)
		if err := rows.Scan(
			&a.Version, &a.Name, &a.AppliedAt, &checksum,
			&started, &finished, &dirty, &lastErr,
		); err != nil {
			return nil, fmt.Errorf("db: scan %s: %w", ledgerTable, err)
		}
		a.Checksum = checksum.String
		if started.Valid {
			a.StartedAt = started.Time
		}
		if finished.Valid {
			a.FinishedAt = finished.Time
		}
		a.Dirty = dirty.Valid && dirty.Bool
		a.LastError = lastErr.String
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate %s: %w", ledgerTable, err)
	}
	return out, nil
}

func diffMigrations(files []Migration, ledger []AppliedMigration) *MigrationStatus {
	st := &MigrationStatus{FrameworkTables: FrameworkTables()}
	byVersion := make(map[int64]Migration, len(files))
	for _, f := range files {
		byVersion[f.Version] = f
	}
	ledgerSet := make(map[int64]bool, len(ledger))
	var maxApplied int64
	for _, a := range ledger {
		ledgerSet[a.Version] = true
		if a.Dirty {
			st.Dirty = append(st.Dirty, a)
		} else {
			st.Applied = append(st.Applied, a)
			if a.Version > maxApplied {
				maxApplied = a.Version
			}
		}

		f, ok := byVersion[a.Version]
		if !ok {
			st.Missing = append(st.Missing, a)
			continue
		}
		if a.Name != f.Name {
			st.NameDrift = append(st.NameDrift, MigrationNameDrift{
				Version: a.Version, LedgerName: a.Name, FileName: f.Name, File: f.File,
			})
		}
		if a.Checksum == "" {
			st.Unverified = append(st.Unverified, a)
		} else if a.Checksum != f.Checksum {
			st.Drift = append(st.Drift, ChecksumDrift{
				Version: a.Version, File: f.File, Ledger: a.Checksum, Current: f.Checksum,
			})
		}
	}
	for _, f := range files {
		if ledgerSet[f.Version] {
			continue
		}
		if f.Version <= maxApplied {
			st.OutOfOrder = append(st.OutOfOrder, f)
		} else {
			st.Pending = append(st.Pending, f)
		}
	}
	return st
}

// ApplyMigrations brings the database up to the migration set and preserves
// the original compact return shape. Call ApplyMigrationsWithReport when the
// caller also needs to observe legacy checksum adoption.
func ApplyMigrations(ctx context.Context, h *DB, fsys fs.FS) ([]Migration, error) {
	report, err := ApplyMigrationsWithReport(ctx, h, fsys)
	return report.Applied, err
}

// ApplyMigrationsWithReport serializes ledger upgrade and migration execution,
// rejects every inconsistent state, establishes legacy checksum baselines,
// and returns both adopted and newly-applied rows.
func ApplyMigrationsWithReport(ctx context.Context, h *DB, fsys fs.FS) (*ApplyReport, error) {
	report := &ApplyReport{}
	files, err := LoadMigrations(fsys)
	if err != nil {
		return report, err
	}
	gdb := h.gdb.WithContext(ctx)
	if err := ensureLedgerBase(gdb); err != nil {
		return report, fmt.Errorf("db: ensure %s base: %w", ledgerTable, err)
	}

	lock, err := acquireMigrationLock(ctx, gdb)
	if err != nil {
		return report, err
	}
	defer lock.release()
	if err := ensureLedgerColumns(gdb); err != nil {
		return report, err
	}

	ledger, err := appliedMigrations(gdb)
	if err != nil {
		return report, err
	}
	st := diffMigrations(files, ledger)
	if len(st.Dirty) > 0 {
		if err := ensureMigrationFence(gdb, lock.owner); err != nil {
			return report, err
		}
		return report, dirtyMigrationsError(st.Dirty)
	}
	if lock.owner == "" {
		if err := cleanupMigrationFenceIfClean(gdb, ""); err != nil {
			return report, err
		}
	}
	if err := structuralMigrationError(st); err != nil {
		return report, err
	}

	if len(st.Unverified) > 0 {
		adopted, err := adoptLegacyChecksums(gdb, files, st.Unverified, lock.owner)
		if err != nil {
			return report, err
		}
		report.Adopted = adopted
		ledger, err = appliedMigrations(gdb)
		if err != nil {
			return report, err
		}
		st = diffMigrations(files, ledger)
	}
	if len(st.Drift) > 0 {
		return report, fmt.Errorf(
			"db: checksum drift at migration version(s) %s; inspect with chok migrate status and resolve explicitly with chok migrate repair accept-drift",
			checksumDriftVersions(st.Drift))
	}

	for _, m := range st.Pending {
		if err := applyOne(ctx, gdb, m, lock.owner); err != nil {
			return report, err
		}
		report.Applied = append(report.Applied, m)
	}
	return report, nil
}

func structuralMigrationError(st *MigrationStatus) error {
	if len(st.Missing) > 0 {
		return fmt.Errorf(
			"db: ledger has version(s) %s but no matching migration file — refusing to continue on drifted history",
			appliedVersions(st.Missing))
	}
	if len(st.NameDrift) > 0 {
		return fmt.Errorf("db: migration name drift at version(s) %s; restore the original filename", nameDriftVersions(st.NameDrift))
	}
	if len(st.OutOfOrder) > 0 {
		return fmt.Errorf("db: out-of-order pending migration version(s) %s are below the applied frontier; renumber them after the latest applied version", migrationVersions(st.OutOfOrder))
	}
	return nil
}

func dirtyMigrationsError(dirty []AppliedMigration) error {
	parts := make([]string, 0, len(dirty))
	for _, a := range dirty {
		part := fmt.Sprintf("%d_%s", a.Version, a.Name)
		if !a.StartedAt.IsZero() {
			part += " started=" + a.StartedAt.Format(time.RFC3339)
		}
		if a.LastError != "" {
			part += " error=" + strconv.Quote(a.LastError)
		}
		parts = append(parts, part)
	}
	return fmt.Errorf(
		"db: dirty migration attempt(s): %s; inspect with chok migrate status and resolve one version with repair retry or mark-applied",
		strings.Join(parts, ", "))
}

func adoptLegacyChecksums(gdb *gorm.DB, files []Migration, rows []AppliedMigration, owner string) ([]AppliedMigration, error) {
	byVersion := make(map[int64]Migration, len(files))
	for _, f := range files {
		byVersion[f.Version] = f
	}
	adopted := make([]AppliedMigration, 0, len(rows))
	adopt := func(exec *gorm.DB) error {
		for _, a := range rows {
			f, ok := byVersion[a.Version]
			if !ok || f.Name != a.Name {
				continue
			}
			res := exec.Exec(
				"UPDATE "+ledgerTable+" SET checksum = ?, finished_at = CASE WHEN finished_at IS NULL THEN applied_at ELSE finished_at END "+
					"WHERE version = ? AND dirty = FALSE AND checksum = ''",
				f.Checksum, a.Version,
			)
			if res.Error != nil {
				return fmt.Errorf("db: adopt checksum for migration %d: %w", a.Version, res.Error)
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("db: adopt checksum for migration %d: ledger changed concurrently", a.Version)
			}
			a.Checksum = f.Checksum
			if a.FinishedAt.IsZero() {
				a.FinishedAt = a.AppliedAt
			}
			adopted = append(adopted, a)
		}
		return nil
	}
	var err error
	if owner == "" {
		err = adopt(gdb)
	} else {
		err = withMigrationLeaseTransaction(gdb, owner, adopt)
	}
	if err != nil {
		return nil, err
	}
	return adopted, nil
}

// applyOne validates the file before recording an attempt, commits a dirty
// row and old-engine fence before any SQL, then clears dirty in the same
// transaction as the final statement where the dialect permits it.
func applyOne(ctx context.Context, gdb *gorm.DB, m Migration, owner string) error {
	stmts := splitSQLStatements(m.SQL)
	if len(stmts) == 0 {
		return fmt.Errorf("db: migration %s contains no statements", m.File)
	}
	if err := insertDirtyMarker(gdb, m, owner); err != nil {
		return fmt.Errorf("db: mark migration %s dirty: %w", m.File, err)
	}

	err := withMigrationLeaseTransaction(gdb, owner, func(tx *gorm.DB) error {
		for i, stmt := range stmts {
			if err := tx.Exec(stmt).Error; err != nil {
				return fmt.Errorf("statement %d: %w", i+1, err)
			}
		}
		now := time.Now().UTC()
		res := tx.Exec(
			"UPDATE "+ledgerTable+" SET dirty = FALSE, finished_at = ?, applied_at = ?, last_error = '' "+
				"WHERE version = ? AND dirty = TRUE AND checksum = ?",
			now, now, m.Version, m.Checksum,
		)
		if res.Error != nil {
			return fmt.Errorf("finalize ledger: %w", res.Error)
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("finalize ledger: dirty marker ownership lost (updated %d rows)", res.RowsAffected)
		}
		if owner == "" {
			if err := tx.Exec("DELETE FROM "+ledgerTable+" WHERE version = ?", migrationFenceVersion).Error; err != nil {
				return fmt.Errorf("remove compatibility fence: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		persistMigrationError(ctx, gdb, m, err)
		return fmt.Errorf("db: apply migration %s: %w", m.File, err)
	}
	return nil
}

func insertDirtyMarker(gdb *gorm.DB, m Migration, owner string) error {
	now := time.Now().UTC()
	return withMigrationLeaseTransaction(gdb, owner, func(tx *gorm.DB) error {
		if owner == "" {
			if err := ensureMigrationFence(tx, ""); err != nil {
				return err
			}
		}
		return tx.Exec(
			"INSERT INTO "+ledgerTable+" (version, name, applied_at, checksum, started_at, dirty, last_error) "+
				"VALUES (?, ?, ?, ?, ?, TRUE, '')",
			m.Version, m.Name, now, m.Checksum, now,
		).Error
	})
}

func persistMigrationError(ctx context.Context, gdb *gorm.DB, m Migration, cause error) {
	msg := []rune(cause.Error())
	if len(msg) > maxMigrationErrorRunes {
		msg = msg[:maxMigrationErrorRunes]
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrationCleanupTimeout)
	defer cancel()
	_ = gdb.WithContext(writeCtx).Exec(
		"UPDATE "+ledgerTable+" SET last_error = ? WHERE version = ? AND dirty = TRUE AND checksum = ?",
		string(msg), m.Version, m.Checksum,
	).Error
}

func ensureMigrationFence(gdb *gorm.DB, owner string) error {
	var count int64
	if err := gdb.Raw("SELECT COUNT(*) FROM "+ledgerTable+" WHERE version = ?", migrationFenceVersion).Scan(&count).Error; err != nil {
		return fmt.Errorf("db: inspect migration compatibility fence: %w", err)
	}
	if count > 0 {
		return nil
	}
	name := migrationFenceName
	if owner != "" {
		name = owner
	}
	if err := gdb.Exec(
		"INSERT INTO "+ledgerTable+" (version, name, applied_at) VALUES (?, ?, ?)",
		migrationFenceVersion, name, time.Now().UTC(),
	).Error; err != nil {
		return fmt.Errorf("db: create migration compatibility fence: %w", err)
	}
	return nil
}

func hasDirtyMigrations(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasColumn(ledgerTable, "dirty") {
		return false, nil
	}
	var count int64
	if err := gdb.Raw("SELECT COUNT(*) FROM " + ledgerTable + " WHERE version > 0 AND dirty = TRUE").Scan(&count).Error; err != nil {
		return false, fmt.Errorf("db: count dirty migrations: %w", err)
	}
	return count > 0, nil
}

func cleanupMigrationFenceIfClean(gdb *gorm.DB, owner string) error {
	dirty, err := hasDirtyMigrations(gdb)
	if err != nil || dirty {
		return err
	}
	query := "DELETE FROM " + ledgerTable + " WHERE version = ?"
	args := []any{migrationFenceVersion}
	if owner != "" {
		query += " AND name = ?"
		args = append(args, owner)
	}
	if err := gdb.Exec(query, args...).Error; err != nil {
		return fmt.Errorf("db: remove migration compatibility fence: %w", err)
	}
	return nil
}

func verifyMigrationLease(gdb *gorm.DB, owner string) error {
	if owner == "" {
		return nil
	}
	var current string
	if err := gdb.Raw("SELECT name FROM "+ledgerTable+" WHERE version = ?", migrationFenceVersion).Scan(&current).Error; err != nil {
		return fmt.Errorf("db: verify sqlite migration lease: %w", err)
	}
	if current != owner {
		return fmt.Errorf("db: sqlite migration lease ownership lost")
	}
	return nil
}

func refreshMigrationLease(gdb *gorm.DB, owner string) error {
	if owner == "" {
		return nil
	}
	res := gdb.Exec(
		"UPDATE "+ledgerTable+" SET applied_at = ? WHERE version = ? AND name = ?",
		time.Now().UTC(), migrationFenceVersion, owner,
	)
	if res.Error != nil {
		return fmt.Errorf("db: refresh sqlite migration lease: %w", res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("db: sqlite migration lease ownership lost")
	}
	return nil
}

func withMigrationLeaseTransaction(gdb *gorm.DB, owner string, work func(*gorm.DB) error) error {
	return gdb.Transaction(func(tx *gorm.DB) error {
		if err := verifyMigrationLease(tx, owner); err != nil {
			return err
		}
		// The first write obtains SQLite's write lock even if the DSN no
		// longer uses _txlock=immediate.
		if err := refreshMigrationLease(tx, owner); err != nil {
			return err
		}
		if err := work(tx); err != nil {
			return err
		}
		// Stamp at the transaction boundary: a long migration must not expose
		// an already-expired timestamp at the instant its commit becomes visible.
		return refreshMigrationLease(tx, owner)
	})
}

// MigrationsStatus is strictly read-only. It tolerates a missing, legacy, or
// partially-upgraded ledger and reports every mismatch instead of converting
// diagnostic state into an error.
func MigrationsStatus(ctx context.Context, h *DB, fsys fs.FS) (*MigrationStatus, error) {
	files, err := LoadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	gdb := h.gdb.WithContext(ctx)
	if !gdb.Migrator().HasTable(ledgerTable) {
		return diffMigrations(files, nil), nil
	}
	ledger, err := appliedMigrations(gdb)
	if err != nil {
		return nil, err
	}
	st := diffMigrations(files, ledger)
	fence, err := migrationFenceStatus(gdb)
	if err != nil {
		return nil, err
	}
	st.Fence = fence
	return st, nil
}

func migrationFenceStatus(gdb *gorm.DB) (*MigrationFenceStatus, error) {
	var row struct {
		Owner      string
		AcquiredAt time.Time
	}
	res := gdb.Raw(
		"SELECT name AS owner, applied_at AS acquired_at FROM "+ledgerTable+" WHERE version = ?",
		migrationFenceVersion,
	).Scan(&row)
	if res.Error != nil {
		return nil, fmt.Errorf("db: read migration compatibility fence: %w", res.Error)
	}
	if row.Owner == "" {
		return nil, nil
	}
	return &MigrationFenceStatus{Owner: row.Owner, AcquiredAt: row.AcquiredAt}, nil
}

// RepairMigration resolves exactly one inspected ledger row under the same
// cross-process lock as ApplyMigrations. It never guesses whether a dirty
// MySQL migration should be retried or accepted as complete.
func RepairMigration(ctx context.Context, h *DB, fsys fs.FS, opts RepairOptions) (*RepairReport, error) {
	if err := validateRepairOptions(opts); err != nil {
		return nil, err
	}
	files, err := LoadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	byVersion := make(map[int64]Migration, len(files))
	for _, f := range files {
		byVersion[f.Version] = f
	}
	gdb := h.gdb.WithContext(ctx)
	if err := ensureLedgerBase(gdb); err != nil {
		return nil, fmt.Errorf("db: ensure %s base: %w", ledgerTable, err)
	}
	lock, err := acquireMigrationLock(ctx, gdb)
	if err != nil {
		return nil, err
	}
	defer lock.release()
	if err := ensureLedgerColumns(gdb); err != nil {
		return nil, err
	}

	ledger, err := appliedMigrations(gdb)
	if err != nil {
		return nil, err
	}
	if diff := diffMigrations(files, ledger); len(diff.Dirty) > 0 {
		if err := ensureMigrationFence(gdb, lock.owner); err != nil {
			return nil, err
		}
	}
	var row *AppliedMigration
	for i := range ledger {
		if ledger[i].Version == opts.Version {
			copy := ledger[i]
			row = &copy
			break
		}
	}
	if row == nil {
		return nil, fmt.Errorf("db: repair migration %d: no ledger row", opts.Version)
	}
	if row.Checksum != opts.ExpectedChecksum {
		return nil, fmt.Errorf("db: repair migration %d: checksum changed since inspection (expected %s, ledger %s)", opts.Version, opts.ExpectedChecksum, row.Checksum)
	}
	file, ok := byVersion[opts.Version]
	if !ok {
		return nil, fmt.Errorf("db: repair migration %d: migration file is missing; restore the exact file before repair", opts.Version)
	}
	if file.Name != row.Name {
		return nil, fmt.Errorf("db: repair migration %d: filename identity changed from %q to %q; restore it before repair", opts.Version, row.Name, file.Name)
	}

	now := time.Now().UTC()
	err = withMigrationLeaseTransaction(gdb, lock.owner, func(tx *gorm.DB) error {
		var res *gorm.DB
		switch opts.Action {
		case RepairRetry:
			if !row.Dirty {
				return fmt.Errorf("migration %d is not dirty", row.Version)
			}
			res = tx.Exec(
				"DELETE FROM "+ledgerTable+" WHERE version = ? AND dirty = TRUE AND checksum = ?",
				row.Version, opts.ExpectedChecksum,
			)
		case RepairMarkApplied:
			if !row.Dirty {
				return fmt.Errorf("migration %d is not dirty", row.Version)
			}
			res = tx.Exec(
				"UPDATE "+ledgerTable+" SET dirty = FALSE, finished_at = ?, applied_at = ? "+
					"WHERE version = ? AND dirty = TRUE AND checksum = ?",
				now, now, row.Version, opts.ExpectedChecksum,
			)
		case RepairAcceptDrift:
			if row.Dirty {
				return fmt.Errorf("migration %d is dirty; resolve it with retry or mark-applied first", row.Version)
			}
			if row.Checksum == file.Checksum {
				return fmt.Errorf("migration %d has no checksum drift", row.Version)
			}
			res = tx.Exec(
				"UPDATE "+ledgerTable+" SET checksum = ? WHERE version = ? AND dirty = FALSE AND checksum = ?",
				file.Checksum, row.Version, opts.ExpectedChecksum,
			)
		}
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("repair compare-and-swap lost (updated %d rows)", res.RowsAffected)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("db: repair migration %d (%s): %w", opts.Version, opts.Action, err)
	}
	if lock.owner == "" {
		if err := cleanupMigrationFenceIfClean(gdb, ""); err != nil {
			return nil, err
		}
	}
	return &RepairReport{
		Action: opts.Action, Version: opts.Version, File: file.File,
		LedgerChecksum: row.Checksum, CurrentChecksum: file.Checksum,
		Reason: strings.TrimSpace(opts.Reason), ResolvedAt: now,
	}, nil
}

var checksumHexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validateRepairOptions(opts RepairOptions) error {
	if opts.Version <= 0 {
		return fmt.Errorf("db: repair version must be positive")
	}
	switch opts.Action {
	case RepairRetry, RepairMarkApplied, RepairAcceptDrift:
	default:
		return fmt.Errorf("db: repair action must be retry|mark-applied|accept-drift, got %q", opts.Action)
	}
	if !checksumHexRe.MatchString(opts.ExpectedChecksum) {
		return fmt.Errorf("db: repair expected checksum must be 64 lowercase hex characters")
	}
	reason := strings.TrimSpace(opts.Reason)
	if reason == "" {
		return fmt.Errorf("db: repair reason must not be empty")
	}
	if len([]rune(reason)) > 1024 {
		return fmt.Errorf("db: repair reason must be at most 1024 characters")
	}
	return nil
}

func migrationVersions(ms []Migration) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = strconv.FormatInt(m.Version, 10)
	}
	return strings.Join(parts, ",")
}

func appliedVersions(ms []AppliedMigration) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = strconv.FormatInt(m.Version, 10)
	}
	return strings.Join(parts, ",")
}

func checksumDriftVersions(ms []ChecksumDrift) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = strconv.FormatInt(m.Version, 10)
	}
	return strings.Join(parts, ",")
}

func nameDriftVersions(ms []MigrationNameDrift) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = strconv.FormatInt(m.Version, 10)
	}
	return strings.Join(parts, ",")
}

// --- migration lock (three dialect branches, SPEC §5.3) --------------

// pgAdvisoryLockKey is fnv64a("chok:schema_migrations") interpreted as
// a signed 64-bit advisory-lock key — stable across runs and
// platforms, vanishingly unlikely to collide with an application's
// own advisory locks.
var pgAdvisoryLockKey = func() int64 {
	const offset64, prime64 = 14695981039346656037, 1099511628211
	h := uint64(offset64)
	for _, c := range []byte("chok:schema_migrations") {
		h ^= uint64(c)
		h *= prime64
	}
	return int64(h)
}()

// mysqlLockName is GET_LOCK's namespace-global name (64-char limit).
const mysqlLockName = "chok:schema_migrations"

type migrationLock struct {
	owner   string // non-empty for the SQLite ledger lease
	release func()
}

// acquireMigrationLock takes the cross-process migration lock for the
// handle's dialect:
//
//   - postgres: pg_advisory_lock on a dedicated *sql.Conn (advisory
//     locks are per-session — the conn is pinned until release)
//   - mysql: GET_LOCK on a dedicated conn, timeout from ctx deadline
//   - sqlite: a version-zero ledger lease. SQLite's transaction lock cannot
//     span the committed dirty marker and the following migration transaction,
//     so the old no-op branch is insufficient once repair can run concurrently.
func acquireMigrationLock(ctx context.Context, gdb *gorm.DB) (migrationLock, error) {
	switch gdb.Dialector.Name() {
	case "postgres":
		release, err := acquireConnLock(ctx, gdb,
			func(ctx context.Context, conn *sql.Conn) error {
				_, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", pgAdvisoryLockKey)
				return err
			},
			func(conn *sql.Conn) {
				// Unlock on the same session; ignore errors — the
				// conn.Close below drops the session and with it the lock.
				_, _ = conn.ExecContext(context.WithoutCancel(ctx),
					"SELECT pg_advisory_unlock($1)", pgAdvisoryLockKey)
			})
		return migrationLock{release: release}, err
	case "mysql":
		timeout := int64(60)
		if dl, ok := ctx.Deadline(); ok {
			if remaining := int64(time.Until(dl).Seconds()); remaining > 0 {
				timeout = remaining
			} else {
				timeout = 1
			}
		}
		release, err := acquireConnLock(ctx, gdb,
			func(ctx context.Context, conn *sql.Conn) error {
				var got sql.NullInt64
				row := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", mysqlLockName, timeout)
				if err := row.Scan(&got); err != nil {
					return err
				}
				if !got.Valid || got.Int64 != 1 {
					return fmt.Errorf("db: GET_LOCK(%q) timed out after %ds", mysqlLockName, timeout)
				}
				return nil
			},
			func(conn *sql.Conn) {
				_, _ = conn.ExecContext(context.WithoutCancel(ctx),
					"SELECT RELEASE_LOCK(?)", mysqlLockName)
			})
		return migrationLock{release: release}, err
	case "sqlite":
		if err := ensureLedgerBase(gdb); err != nil {
			return migrationLock{}, fmt.Errorf("db: ensure %s for sqlite migration lease: %w", ledgerTable, err)
		}
		return acquireSQLiteMigrationLease(ctx, gdb)
	default:
		return migrationLock{release: func() {}}, nil
	}
}

func acquireSQLiteMigrationLease(ctx context.Context, gdb *gorm.DB) (migrationLock, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return migrationLock{}, fmt.Errorf("db: generate sqlite migration lease owner: %w", err)
	}
	owner := sqliteLeasePrefix + hex.EncodeToString(random)

	for {
		now := time.Now().UTC()
		res := gdb.WithContext(ctx).Exec(
			"INSERT OR IGNORE INTO "+ledgerTable+" (version, name, applied_at) VALUES (?, ?, ?)",
			migrationFenceVersion, owner, now,
		)
		if res.Error == nil && res.RowsAffected == 1 {
			return migrationLock{
				owner: owner,
				release: func() {
					cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrationCleanupTimeout)
					defer cancel()
					_ = releaseSQLiteMigrationLease(gdb.WithContext(cleanupCtx), owner)
				},
			}, nil
		}
		if res.Error != nil && !sqliteLockContention(res.Error) {
			return migrationLock{}, fmt.Errorf("db: acquire sqlite migration lease: %w", res.Error)
		}

		var existing struct {
			Name      string
			AppliedAt time.Time
		}
		readErr := gdb.WithContext(ctx).Raw(
			"SELECT name, applied_at FROM "+ledgerTable+" WHERE version = ?",
			migrationFenceVersion,
		).Scan(&existing).Error
		if readErr == nil && existing.Name != "" && now.Sub(existing.AppliedAt) >= sqliteLeaseTTL {
			takeover := gdb.WithContext(ctx).Exec(
				"UPDATE "+ledgerTable+" SET name = ?, applied_at = ? WHERE version = ? AND name = ? AND applied_at = ?",
				owner, now, migrationFenceVersion, existing.Name, existing.AppliedAt,
			)
			if takeover.Error == nil && takeover.RowsAffected == 1 {
				return migrationLock{
					owner: owner,
					release: func() {
						cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrationCleanupTimeout)
						defer cancel()
						_ = releaseSQLiteMigrationLease(gdb.WithContext(cleanupCtx), owner)
					},
				}, nil
			}
			if takeover.Error != nil && !sqliteLockContention(takeover.Error) {
				return migrationLock{}, fmt.Errorf("db: take over stale sqlite migration lease: %w", takeover.Error)
			}
		} else if readErr != nil && !sqliteLockContention(readErr) {
			return migrationLock{}, fmt.Errorf("db: inspect sqlite migration lease: %w", readErr)
		}

		timer := time.NewTimer(sqliteLeasePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return migrationLock{}, fmt.Errorf("db: wait for sqlite migration lease: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func releaseSQLiteMigrationLease(gdb *gorm.DB, owner string) error {
	dirty, err := hasDirtyMigrations(gdb)
	if err != nil {
		return err
	}
	if dirty {
		// The active call has returned, so make the retained old-engine fence
		// immediately reclaimable by a new repair/up process. A hard process
		// crash cannot run this release path and instead relies on lease TTL.
		if err := gdb.Exec(
			"UPDATE "+ledgerTable+" SET name = ?, applied_at = ? WHERE version = ? AND name = ?",
			migrationFenceName, time.Unix(0, 0).UTC(), migrationFenceVersion, owner,
		).Error; err != nil {
			return fmt.Errorf("db: release sqlite migration lease as dirty fence: %w", err)
		}
		return nil
	}
	return cleanupMigrationFenceIfClean(gdb, owner)
}

func sqliteLockContention(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "locked") || strings.Contains(msg, "busy")
}

// acquireConnLock pins one pool connection, runs the dialect's lock
// statement on it, and returns a release that unlocks and returns the
// connection. The lock lives exactly as long as the pinned session.
func acquireConnLock(
	ctx context.Context,
	gdb *gorm.DB,
	lock func(context.Context, *sql.Conn) error,
	unlock func(*sql.Conn),
) (func(), error) {
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("db: get underlying sql.DB: %w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: pin migration-lock conn: %w", err)
	}
	if err := lock(ctx, conn); err != nil {
		_ = conn.Close()
		if errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("db: cancelled while waiting for migration lock: %w", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("db: migration lock wait exceeded the phase budget: %w", err)
		}
		return nil, fmt.Errorf("db: acquire migration lock: %w", err)
	}
	return func() {
		unlock(conn)
		_ = conn.Close()
	}, nil
}

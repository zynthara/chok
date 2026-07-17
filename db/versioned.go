package db

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"
)

// Versioned migrations (SPEC §5.3) are embedded, forward-only,
// sequence-numbered *.sql files. The schema_migrations ledger records
// checksums and a crash-persistent dirty marker before any migration SQL
// runs. A temporary version-zero fence makes old chok binaries fail closed
// while a migration is active or unresolved. PostgreSQL and SQLite keep a
// file's SQL plus clean transition atomic; MySQL retains the dirty marker
// across its auto-committing DDL so an operator can resolve partial effects.

// ledgerTable is the application migration ledger. Owned component sequences
// use schema_migrations_chok_<kind>; keeping this constant preserves the
// existing package surface and in-package tests for the application sequence.
const ledgerTable = "schema_migrations"

type migrationSequence struct {
	kind             string
	ledger           string
	dialect          string
	fsys             fs.FS
	baseline         *Baseline
	owner            string
	componentVersion string
}

func applicationMigrationSequence(fsys fs.FS, dialect string) migrationSequence {
	return migrationSequence{kind: "app", ledger: ledgerTable, dialect: dialect, fsys: fsys}
}

type migrationEngine struct {
	seq migrationSequence
}

func applicationMigrationEngine(dialect string, fsys fs.FS) migrationEngine {
	return migrationEngine{seq: applicationMigrationSequence(fsys, dialect)}
}

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

// FrameworkTables returns the alphabetically sorted catalog of tables owned
// by chok's built-in components and evolved outside the application's
// versioned migration history. The catalog is generated from
// Descriptor.Schema declarations and is independent of which modules are
// assembled or which named database instance is being inspected; it does not
// assert that every listed table exists in that database. The returned slice
// is a copy and may be modified by the caller.
func FrameworkTables() []string {
	return append([]string(nil), frameworkTables...)
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
	Dialect    string
	Provenance string
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
	Sequence   string
	Ledger     string
	Dialect    string
	Applied    []AppliedMigration
	Pending    []Migration
	Dirty      []AppliedMigration
	Drift      []ChecksumDrift
	Missing    []AppliedMigration
	Unverified []AppliedMigration
	OutOfOrder []Migration
	NameDrift  []MigrationNameDrift
	Fence      *MigrationFenceStatus
	// FrameworkTables echoes the built-in framework-owned table catalog next
	// to the application migration ledger.
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
	Sequence string
	Ledger   string
	Dialect  string
	Applied  []Migration
	Adopted  []AppliedMigration
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
// operator inspected. Reason is mandatory. Operator is the optional identity
// persisted to repair history — an explicitly supplied value must pass
// validation, while an empty value derives user@host best-effort.
type RepairOptions struct {
	Action           RepairAction
	Version          int64
	ExpectedChecksum string
	Reason           string
	Operator         string
}

// RepairReport is the structured evidence emitted for one repair action. The
// same evidence is persisted to the append-only repair history table in the
// repair's own transaction — a history row means the business-state CAS (and,
// on PostgreSQL/MySQL, the fence cleanup) committed; SQLite's post-commit
// lease release stays best-effort and a caller crash after commit can still
// leave a row whose success response was never observed. Operator and
// ChokVersion echo the values actually persisted.
type RepairReport struct {
	Sequence        string
	Ledger          string
	Dialect         string
	Action          RepairAction
	Version         int64
	File            string
	LedgerChecksum  string
	CurrentChecksum string
	Reason          string
	Operator        string
	ChokVersion     string
	ResolvedAt      time.Time
}

// migFileRe: <version>_<name>.sql — version is a positive decimal
// sequence number (padding optional), name is free-form.
var migFileRe = regexp.MustCompile(`^(\d+)_(.+)\.sql$`)

// validateMigrationFileNameChars keeps migration filenames renderable and
// history-safe: valid UTF-8, no control characters, no path separators.
// LoadMigrations enforces it at load time and the repair-history read path
// re-checks persisted rows with the same rule, so a migration that can be
// repaired can never persist history its own reader rejects.
func validateMigrationFileNameChars(name string) error {
	if !utf8.ValidString(name) {
		return fmt.Errorf("filename is not valid UTF-8")
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return fmt.Errorf("filename contains a control character or path separator")
		}
	}
	return nil
}

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
		if err := validateMigrationFileNameChars(name); err != nil {
			return nil, fmt.Errorf("db: migration file %q: %v", name, err)
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

// ensureLedgerBase creates the complete current ledger for a fresh database
// and leaves existing three-column ledgers untouched (ensureLedgerColumns
// upgrades those additively). Callers must hold the migration lock:
// PostgreSQL's CREATE TABLE IF NOT EXISTS is not concurrency-safe, so two
// sessions racing on first creation collide on the pg_type/pg_class catalog
// uniques (SQLSTATE 23505). The SQLite lock branch is the one sanctioned
// pre-lock caller — its lease row lives inside the ledger it ensures, and
// SQLite's single-writer file lock serializes the creation anyway.
func (e migrationEngine) ensureLedgerBase(gdb *gorm.DB) error {
	return gdb.Exec(
		"CREATE TABLE IF NOT EXISTS " + e.seq.ledger + " (" +
			"version BIGINT PRIMARY KEY, " +
			"name VARCHAR(255) NOT NULL, " +
			"applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, " +
			"checksum VARCHAR(64) NOT NULL DEFAULT '', " +
			"started_at TIMESTAMP NULL, " +
			"finished_at TIMESTAMP NULL, " +
			"dirty BOOLEAN NOT NULL DEFAULT FALSE, " +
			"last_error TEXT, " +
			"dialect VARCHAR(32) NOT NULL DEFAULT '', " +
			"provenance VARCHAR(32) NOT NULL DEFAULT '')",
	).Error
}

type ledgerColumns map[string]bool

func (e migrationEngine) inspectLedgerColumns(gdb *gorm.DB) (ledgerColumns, error) {
	types, err := gdb.Migrator().ColumnTypes(e.seq.ledger)
	if err != nil {
		return nil, fmt.Errorf("db: inspect %s columns: %w", e.seq.ledger, err)
	}
	columns := make(ledgerColumns, len(types))
	for _, column := range types {
		columns[strings.ToLower(column.Name())] = true
	}
	return columns, nil
}

// has treats nil as the complete current schema. Callers may use nil only
// after ensureLedgerColumns succeeds under the migration lock.
func (columns ledgerColumns) has(name string) bool {
	return columns == nil || columns[strings.ToLower(name)]
}

// ensureLedgerColumns upgrades a legacy ledger additively. Callers must hold
// the migration lock: introspection plus ALTER is idempotent across retries,
// not an atomic concurrency primitive.
func (e migrationEngine) ensureLedgerColumns(gdb *gorm.DB) error {
	existing, err := e.inspectLedgerColumns(gdb)
	if err != nil {
		return err
	}
	columns := []struct {
		name string
		ddl  string
	}{
		{"checksum", "VARCHAR(64) NOT NULL DEFAULT ''"},
		{"started_at", "TIMESTAMP NULL"},
		{"finished_at", "TIMESTAMP NULL"},
		{"dirty", "BOOLEAN NOT NULL DEFAULT FALSE"},
		{"last_error", "TEXT"},
		{"dialect", "VARCHAR(32) NOT NULL DEFAULT ''"},
		{"provenance", "VARCHAR(32) NOT NULL DEFAULT ''"},
	}
	for _, col := range columns {
		if existing.has(col.name) {
			continue
		}
		if err := gdb.Exec("ALTER TABLE " + e.seq.ledger + " ADD COLUMN " + col.name + " " + col.ddl).Error; err != nil {
			return fmt.Errorf("db: upgrade %s add %s: %w", e.seq.ledger, col.name, err)
		}
		existing[col.name] = true
	}
	return nil
}

// appliedMigrations reads every real migration row while tolerating the old
// three-column ledger and partially-completed additive upgrades. Passing nil
// columns means the current schema is known to be complete. Version zero is
// the internal compatibility fence and is deliberately invisible here.
func (e migrationEngine) appliedMigrations(gdb *gorm.DB, columns ledgerColumns) ([]AppliedMigration, error) {
	expr := func(column, fallback string) string {
		if columns.has(column) {
			return column
		}
		return fallback
	}
	query := "SELECT version, name, applied_at, " +
		expr("checksum", "''") + ", " +
		expr("started_at", "NULL") + ", " +
		expr("finished_at", "NULL") + ", " +
		expr("dirty", "FALSE") + ", " +
		expr("last_error", "NULL") + ", " +
		expr("dialect", "''") + ", " +
		expr("provenance", "''") +
		" FROM " + e.seq.ledger + " WHERE version > 0 ORDER BY version"
	rows, err := gdb.Raw(query).Rows()
	if err != nil {
		return nil, fmt.Errorf("db: read %s: %w", e.seq.ledger, err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var (
			a          AppliedMigration
			checksum   sql.NullString
			started    sql.NullTime
			finished   sql.NullTime
			dirty      sql.NullBool
			lastErr    sql.NullString
			dialect    sql.NullString
			provenance sql.NullString
		)
		if err := rows.Scan(
			&a.Version, &a.Name, &a.AppliedAt, &checksum,
			&started, &finished, &dirty, &lastErr, &dialect, &provenance,
		); err != nil {
			return nil, fmt.Errorf("db: scan %s: %w", e.seq.ledger, err)
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
		a.Dialect = dialect.String
		a.Provenance = provenance.String
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate %s: %w", e.seq.ledger, err)
	}
	return out, nil
}

func (e migrationEngine) diffMigrations(files []Migration, ledger []AppliedMigration) *MigrationStatus {
	st := &MigrationStatus{
		Sequence: e.seq.kind, Ledger: e.seq.ledger, Dialect: e.seq.dialect,
		FrameworkTables: FrameworkTables(),
	}
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
	e := applicationMigrationEngine(h.gdb.Dialector.Name(), fsys)
	return e.apply(ctx, h)
}

func (e migrationEngine) apply(ctx context.Context, h *DB) (*ApplyReport, error) {
	report := &ApplyReport{Sequence: e.seq.kind, Ledger: e.seq.ledger, Dialect: e.seq.dialect}
	files, err := LoadMigrations(e.seq.fsys)
	if err != nil {
		return report, err
	}
	gdb := h.gdb.WithContext(ctx)
	owned := e.seq.owner != ""
	ledgerExisted, err := e.preflightSequenceClaim(gdb)
	if err != nil {
		return report, err
	}
	lock, err := e.acquireMigrationLock(ctx, gdb)
	if err != nil {
		return report, err
	}
	defer lock.release()
	if err := e.ensureLedgerBase(gdb); err != nil {
		return report, fmt.Errorf("db: ensure %s base: %w", e.seq.ledger, err)
	}
	if owned {
		if err := ensureManifestBase(gdb); err != nil {
			return report, fmt.Errorf("db: ensure %s base: %w", sequenceManifestTable, err)
		}
	}
	if err := e.ensureLedgerColumns(gdb); err != nil {
		return report, err
	}
	if owned {
		if err := ensureManifestColumns(gdb); err != nil {
			return report, err
		}
	}

	ledger, err := e.appliedMigrations(gdb, nil)
	if err != nil {
		return report, err
	}
	if owned {
		needsAdoption, err := e.authorizeSequenceWrite(gdb, ledgerExisted)
		if err != nil {
			return report, err
		}
		if needsAdoption {
			// The legacy ledger must prove that its existing history belongs to
			// this sequence before trust-on-first-use persists an owner. Once
			// proven, persist the claim before any dialect/checksum/baseline or
			// schema write so a crash cannot return the ledger to unclaimed state.
			if err := e.preflightSequenceAdoption(ctx, h, gdb, files, ledger); err != nil {
				return report, err
			}
			if err := e.adoptSequenceManifest(gdb); err != nil {
				return report, err
			}
		}
	}
	if err := e.adoptLegacyDialect(gdb, ledger, lock.owner); err != nil {
		return report, err
	}
	if err := e.validateLedgerDialect(ledger); err != nil {
		return report, err
	}
	if len(ledger) == 0 && e.seq.baseline != nil && e.seq.baseline.EquivalentVersion > 0 {
		adopted, err := e.adoptBaseline(ctx, h, gdb, files, lock.owner)
		if err != nil {
			return report, err
		}
		if len(adopted) > 0 {
			report.Adopted = append(report.Adopted, adopted...)
			ledger = append(ledger, adopted...)
		}
	}
	st := e.diffMigrations(files, ledger)
	if len(st.Dirty) > 0 {
		if err := e.ensureMigrationFence(gdb, lock.owner); err != nil {
			return report, err
		}
		return report, dirtyMigrationsError(st.Dirty)
	}
	if lock.owner == "" {
		if err := e.cleanupMigrationFenceIfClean(gdb, ""); err != nil {
			return report, err
		}
	}
	if err := structuralMigrationError(st); err != nil {
		return report, err
	}

	if len(st.Unverified) > 0 {
		adopted, err := e.adoptLegacyChecksums(gdb, files, ledger, st.Unverified, lock.owner)
		if err != nil {
			return report, err
		}
		report.Adopted = append(report.Adopted, adopted...)
		st = e.diffMigrations(files, ledger)
	}
	if len(st.Drift) > 0 {
		return report, fmt.Errorf(
			"db: checksum drift at migration version(s) %s; inspect with chok migrate status and resolve explicitly with chok migrate repair accept-drift",
			versionList(st.Drift, func(d ChecksumDrift) int64 { return d.Version }))
	}

	for _, m := range st.Pending {
		if err := e.applyOne(ctx, gdb, m, lock.owner); err != nil {
			return report, err
		}
		report.Applied = append(report.Applied, m)
	}
	if owned {
		if err := e.refreshSequenceManifest(gdb); err != nil {
			return report, err
		}
	}
	return report, nil
}

func (e migrationEngine) preflightSequenceAdoption(
	ctx context.Context,
	h *DB,
	gdb *gorm.DB,
	files []Migration,
	ledger []AppliedMigration,
) error {
	if err := e.validateLedgerDialect(ledger); err != nil {
		return err
	}
	st := e.diffMigrations(files, ledger)
	if len(st.Dirty) > 0 {
		return dirtyMigrationsError(st.Dirty)
	}
	if len(ledger) == 0 && e.seq.baseline != nil && e.seq.baseline.EquivalentVersion > 0 {
		adopted, err := e.planBaselineAdoption(ctx, h, gdb, files)
		if err != nil {
			return err
		}
		if len(adopted) > 0 {
			st = e.diffMigrations(files, adopted)
		}
	}
	if err := structuralMigrationError(st); err != nil {
		return err
	}
	if len(st.Drift) > 0 {
		return fmt.Errorf(
			"db: checksum drift at migration version(s) %s; inspect with chok migrate status and resolve explicitly with chok migrate repair accept-drift",
			versionList(st.Drift, func(d ChecksumDrift) int64 { return d.Version }))
	}
	return nil
}

func structuralMigrationError(st *MigrationStatus) error {
	if len(st.Missing) > 0 {
		return fmt.Errorf(
			"db: ledger has version(s) %s but no matching migration file — refusing to continue on drifted history",
			versionList(st.Missing, func(m AppliedMigration) int64 { return m.Version }))
	}
	if len(st.NameDrift) > 0 {
		return fmt.Errorf("db: migration name drift at version(s) %s; restore the original filename", versionList(st.NameDrift, func(d MigrationNameDrift) int64 { return d.Version }))
	}
	if len(st.OutOfOrder) > 0 {
		return fmt.Errorf("db: out-of-order pending migration version(s) %s are below the applied frontier; renumber them after the latest applied version", versionList(st.OutOfOrder, func(m Migration) int64 { return m.Version }))
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

func (e migrationEngine) adoptLegacyDialect(gdb *gorm.DB, ledger []AppliedMigration, owner string) error {
	needsAdoption := false
	for _, row := range ledger {
		if row.Dialect == "" {
			needsAdoption = true
			break
		}
	}
	if !needsAdoption {
		return nil
	}
	work := func(tx *gorm.DB) error {
		return tx.Exec(
			"UPDATE "+e.seq.ledger+" SET dialect = ?, provenance = CASE WHEN provenance = '' THEN 'legacy' ELSE provenance END "+
				"WHERE version > 0 AND dialect = ''",
			e.seq.dialect,
		).Error
	}
	if err := e.withMigrationLeaseTransaction(gdb, owner, work); err != nil {
		return fmt.Errorf("db: sequence %s ledger %s adopt legacy dialect %s: %w", e.seq.kind, e.seq.ledger, e.seq.dialect, err)
	}
	for i := range ledger {
		if ledger[i].Dialect == "" {
			ledger[i].Dialect = e.seq.dialect
			if ledger[i].Provenance == "" {
				ledger[i].Provenance = "legacy"
			}
		}
	}
	return nil
}

func (e migrationEngine) validateLedgerDialect(ledger []AppliedMigration) error {
	for _, row := range ledger {
		if row.Dialect != "" && row.Dialect != e.seq.dialect {
			return fmt.Errorf(
				"db: sequence %s ledger %s dialect mismatch at version %d: ledger=%s connection=%s; cross-dialect ledgers require an explicit migration runbook",
				e.seq.kind, e.seq.ledger, row.Version, row.Dialect, e.seq.dialect,
			)
		}
	}
	return nil
}

func (e migrationEngine) adoptBaseline(
	ctx context.Context,
	h *DB,
	gdb *gorm.DB,
	files []Migration,
	owner string,
) ([]AppliedMigration, error) {
	adopted, err := e.planBaselineAdoption(ctx, h, gdb, files)
	if err != nil || len(adopted) == 0 {
		return adopted, err
	}
	work := func(tx *gorm.DB) error {
		for _, row := range adopted {
			if err := tx.Exec(
				"INSERT INTO "+e.seq.ledger+" (version, name, applied_at, checksum, finished_at, dirty, last_error, dialect, provenance) "+
					"VALUES (?, ?, ?, ?, ?, FALSE, '', ?, 'baseline')",
				row.Version, row.Name, row.AppliedAt, row.Checksum, row.FinishedAt, row.Dialect,
			).Error; err != nil {
				return err
			}
		}
		return nil
	}
	if err := e.withMigrationLeaseTransaction(gdb, owner, work); err != nil {
		return nil, fmt.Errorf("db: sequence %s ledger %s adopt baseline: %w", e.seq.kind, e.seq.ledger, err)
	}
	return adopted, nil
}

func (e migrationEngine) planBaselineAdoption(
	ctx context.Context,
	h *DB,
	gdb *gorm.DB,
	files []Migration,
) ([]AppliedMigration, error) {
	baseline := e.seq.baseline
	if baseline == nil || baseline.EquivalentVersion == 0 {
		return nil, nil
	}
	present := 0
	var missing []string
	for _, table := range baseline.Tables {
		tablePresent, err := tableExists(gdb, table)
		if err != nil {
			return nil, err
		}
		if tablePresent {
			present++
		} else {
			missing = append(missing, table)
		}
	}
	if present == 0 {
		return nil, nil
	}
	if present != len(baseline.Tables) {
		return nil, fmt.Errorf(
			"db: sequence %s ledger %s baseline refused: owned tables are partially present (missing=%s)",
			e.seq.kind, e.seq.ledger, strings.Join(missing, ","),
		)
	}
	expected := baseline.Fingerprints[e.seq.dialect]
	actual, err := SchemaFingerprint(ctx, h, baseline.Tables)
	if err != nil {
		return nil, fmt.Errorf("db: sequence %s ledger %s inspect baseline: %w", e.seq.kind, e.seq.ledger, err)
	}
	if actual != expected {
		return nil, fmt.Errorf(
			"db: sequence %s ledger %s baseline fingerprint mismatch: %s",
			e.seq.kind, e.seq.ledger, schemaFingerprintDifference(expected, actual),
		)
	}
	byVersion := make(map[int64]Migration, len(files))
	for _, file := range files {
		byVersion[file.Version] = file
	}
	now := time.Now().UTC()
	adopted := make([]AppliedMigration, 0, baseline.EquivalentVersion)
	for version := int64(1); version <= baseline.EquivalentVersion; version++ {
		file, ok := byVersion[version]
		if !ok {
			return nil, fmt.Errorf("db: sequence %s baseline equivalent version %d requires migration version %d", e.seq.kind, baseline.EquivalentVersion, version)
		}
		adopted = append(adopted, AppliedMigration{
			Version: file.Version, Name: file.Name, AppliedAt: now,
			Checksum: file.Checksum, FinishedAt: now,
			Dialect: e.seq.dialect, Provenance: "baseline",
		})
	}
	return adopted, nil
}

func (e migrationEngine) adoptLegacyChecksums(gdb *gorm.DB, files []Migration, ledger, rows []AppliedMigration, owner string) ([]AppliedMigration, error) {
	byVersion := make(map[int64]Migration, len(files))
	for _, f := range files {
		byVersion[f.Version] = f
	}
	ledgerIndex := make(map[int64]int, len(ledger))
	for i := range ledger {
		ledgerIndex[ledger[i].Version] = i
	}
	adopted := make([]AppliedMigration, 0, len(rows))
	adopt := func(exec *gorm.DB) error {
		for _, a := range rows {
			f, ok := byVersion[a.Version]
			if !ok || f.Name != a.Name {
				continue
			}
			res := exec.Exec(
				"UPDATE "+e.seq.ledger+" SET checksum = ?, dialect = CASE WHEN dialect = '' THEN ? ELSE dialect END, "+
					"provenance = CASE WHEN provenance = '' THEN 'checksum-tofu' ELSE provenance END, "+
					"finished_at = CASE WHEN finished_at IS NULL THEN applied_at ELSE finished_at END "+
					"WHERE version = ? AND dirty = FALSE AND checksum = ''",
				f.Checksum, e.seq.dialect, a.Version,
			)
			if res.Error != nil {
				return fmt.Errorf("db: adopt checksum for migration %d: %w", a.Version, res.Error)
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("db: adopt checksum for migration %d: ledger changed concurrently", a.Version)
			}
			a.Checksum = f.Checksum
			a.Dialect = e.seq.dialect
			a.Provenance = "checksum-tofu"
			if a.FinishedAt.IsZero() {
				a.FinishedAt = a.AppliedAt
			}
			if i, ok := ledgerIndex[a.Version]; ok {
				ledger[i] = a
			}
			adopted = append(adopted, a)
		}
		return nil
	}
	var err error
	if owner == "" {
		err = adopt(gdb)
	} else {
		err = e.withMigrationLeaseTransaction(gdb, owner, adopt)
	}
	if err != nil {
		return nil, err
	}
	return adopted, nil
}

// applyOne validates the file before recording an attempt, commits a dirty
// row and old-engine fence before any SQL, then clears dirty in the same
// transaction as the final statement where the dialect permits it.
func (e migrationEngine) applyOne(ctx context.Context, gdb *gorm.DB, m Migration, owner string) error {
	stmts := splitSQLStatements(m.SQL)
	if len(stmts) == 0 {
		return fmt.Errorf("db: migration %s contains no statements", m.File)
	}
	if err := e.insertDirtyMarker(gdb, m, owner); err != nil {
		return fmt.Errorf("db: mark migration %s dirty: %w", m.File, err)
	}

	err := e.withMigrationLeaseTransaction(gdb, owner, func(tx *gorm.DB) error {
		for i, stmt := range stmts {
			if err := tx.Exec(stmt).Error; err != nil {
				return fmt.Errorf("statement %d: %w", i+1, err)
			}
		}
		now := time.Now().UTC()
		res := tx.Exec(
			"UPDATE "+e.seq.ledger+" SET dirty = FALSE, finished_at = ?, applied_at = ?, last_error = '' "+
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
			if err := tx.Exec("DELETE FROM "+e.seq.ledger+" WHERE version = ?", migrationFenceVersion).Error; err != nil {
				return fmt.Errorf("remove compatibility fence: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		e.persistMigrationError(ctx, gdb, m, err)
		return fmt.Errorf("db: apply migration %s: %w", m.File, err)
	}
	return nil
}

func (e migrationEngine) insertDirtyMarker(gdb *gorm.DB, m Migration, owner string) error {
	now := time.Now().UTC()
	return e.withMigrationLeaseTransaction(gdb, owner, func(tx *gorm.DB) error {
		if owner == "" {
			if err := e.ensureMigrationFence(tx, ""); err != nil {
				return err
			}
		}
		return tx.Exec(
			"INSERT INTO "+e.seq.ledger+" (version, name, applied_at, checksum, started_at, dirty, last_error, dialect, provenance) "+
				"VALUES (?, ?, ?, ?, ?, TRUE, '', ?, 'applied')",
			m.Version, m.Name, now, m.Checksum, now, e.seq.dialect,
		).Error
	})
}

func (e migrationEngine) persistMigrationError(ctx context.Context, gdb *gorm.DB, m Migration, cause error) {
	msg := []rune(cause.Error())
	if len(msg) > maxMigrationErrorRunes {
		msg = msg[:maxMigrationErrorRunes]
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrationCleanupTimeout)
	defer cancel()
	_ = gdb.WithContext(writeCtx).Exec(
		"UPDATE "+e.seq.ledger+" SET last_error = ? WHERE version = ? AND dirty = TRUE AND checksum = ?",
		string(msg), m.Version, m.Checksum,
	).Error
}

func (e migrationEngine) ensureMigrationFence(gdb *gorm.DB, owner string) error {
	var count int64
	if err := gdb.Raw("SELECT COUNT(*) FROM "+e.seq.ledger+" WHERE version = ?", migrationFenceVersion).Scan(&count).Error; err != nil {
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
		"INSERT INTO "+e.seq.ledger+" (version, name, applied_at) VALUES (?, ?, ?)",
		migrationFenceVersion, name, time.Now().UTC(),
	).Error; err != nil {
		return fmt.Errorf("db: create migration compatibility fence: %w", err)
	}
	return nil
}

func (e migrationEngine) hasDirtyMigrations(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasColumn(e.seq.ledger, "dirty") {
		return false, nil
	}
	var count int64
	if err := gdb.Raw("SELECT COUNT(*) FROM " + e.seq.ledger + " WHERE version > 0 AND dirty = TRUE").Scan(&count).Error; err != nil {
		return false, fmt.Errorf("db: count dirty migrations: %w", err)
	}
	return count > 0, nil
}

func (e migrationEngine) cleanupMigrationFenceIfClean(gdb *gorm.DB, owner string) error {
	dirty, err := e.hasDirtyMigrations(gdb)
	if err != nil || dirty {
		return err
	}
	query := "DELETE FROM " + e.seq.ledger + " WHERE version = ?"
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

func (e migrationEngine) verifyMigrationLease(gdb *gorm.DB, owner string) error {
	if owner == "" {
		return nil
	}
	var current string
	if err := gdb.Raw("SELECT name FROM "+e.seq.ledger+" WHERE version = ?", migrationFenceVersion).Scan(&current).Error; err != nil {
		return fmt.Errorf("db: verify sqlite migration lease: %w", err)
	}
	if current != owner {
		return fmt.Errorf("db: sqlite migration lease ownership lost")
	}
	return nil
}

func (e migrationEngine) refreshMigrationLease(gdb *gorm.DB, owner string) error {
	if owner == "" {
		return nil
	}
	res := gdb.Exec(
		"UPDATE "+e.seq.ledger+" SET applied_at = ? WHERE version = ? AND name = ?",
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

func (e migrationEngine) withMigrationLeaseTransaction(gdb *gorm.DB, owner string, work func(*gorm.DB) error) error {
	return gdb.Transaction(func(tx *gorm.DB) error {
		if err := e.verifyMigrationLease(tx, owner); err != nil {
			return err
		}
		// The first write obtains SQLite's write lock even if the DSN no
		// longer uses _txlock=immediate.
		if err := e.refreshMigrationLease(tx, owner); err != nil {
			return err
		}
		if err := work(tx); err != nil {
			return err
		}
		// Stamp at the transaction boundary: a long migration must not expose
		// an already-expired timestamp at the instant its commit becomes visible.
		return e.refreshMigrationLease(tx, owner)
	})
}

// MigrationsStatus is strictly read-only. It tolerates a missing, legacy, or
// partially-upgraded ledger and reports every mismatch instead of converting
// diagnostic state into an error.
func MigrationsStatus(ctx context.Context, h *DB, fsys fs.FS) (*MigrationStatus, error) {
	e := applicationMigrationEngine(h.gdb.Dialector.Name(), fsys)
	return e.status(ctx, h)
}

func (e migrationEngine) status(ctx context.Context, h *DB) (*MigrationStatus, error) {
	files, err := LoadMigrations(e.seq.fsys)
	if err != nil {
		return nil, err
	}
	gdb := h.gdb.WithContext(ctx)
	ledgerPresent, err := tableExists(gdb, e.seq.ledger)
	if err != nil {
		return nil, err
	}
	if !ledgerPresent {
		return e.diffMigrations(files, nil), nil
	}
	columns, err := e.inspectLedgerColumns(gdb)
	if err != nil {
		return nil, err
	}
	ledger, err := e.appliedMigrations(gdb, columns)
	if err != nil {
		return nil, err
	}
	if err := e.validateLedgerDialect(ledger); err != nil {
		return nil, err
	}
	st := e.diffMigrations(files, ledger)
	fence, err := e.migrationFenceStatus(gdb)
	if err != nil {
		return nil, err
	}
	st.Fence = fence
	return st, nil
}

func (e migrationEngine) migrationFenceStatus(gdb *gorm.DB) (*MigrationFenceStatus, error) {
	var row struct {
		Owner      string
		AcquiredAt time.Time
	}
	res := gdb.Raw(
		"SELECT name AS owner, applied_at AS acquired_at FROM "+e.seq.ledger+" WHERE version = ?",
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
	e := applicationMigrationEngine(h.gdb.Dialector.Name(), fsys)
	return e.repair(ctx, h, opts)
}

func (e migrationEngine) repair(ctx context.Context, h *DB, opts RepairOptions) (*RepairReport, error) {
	if err := validateRepairOptions(opts); err != nil {
		return nil, err
	}
	operator, err := resolveRepairOperator(opts.Operator)
	if err != nil {
		return nil, err
	}
	files, err := LoadMigrations(e.seq.fsys)
	if err != nil {
		return nil, err
	}
	byVersion := make(map[int64]Migration, len(files))
	for _, f := range files {
		byVersion[f.Version] = f
	}
	gdb := h.gdb.WithContext(ctx)
	owned := e.seq.owner != ""
	ledgerExisted, err := e.preflightSequenceClaim(gdb)
	if err != nil {
		return nil, err
	}
	if owned && !ledgerExisted {
		return nil, fmt.Errorf("%w: migration kind %q has no existing ledger", ErrSequenceUnclaimed, e.seq.kind)
	}
	lock, err := e.acquireMigrationLock(ctx, gdb)
	if err != nil {
		return nil, err
	}
	defer lock.release()
	if err := e.ensureLedgerBase(gdb); err != nil {
		return nil, fmt.Errorf("db: ensure %s base: %w", e.seq.ledger, err)
	}
	if owned {
		if err := ensureManifestBase(gdb); err != nil {
			return nil, fmt.Errorf("db: ensure %s base: %w", sequenceManifestTable, err)
		}
	}
	if err := ensureRepairHistoryBase(gdb); err != nil {
		return nil, fmt.Errorf("db: ensure %s base: %w", sequenceRepairHistoryTable, err)
	}
	if err := e.ensureLedgerColumns(gdb); err != nil {
		return nil, err
	}
	if owned {
		if err := ensureManifestColumns(gdb); err != nil {
			return nil, err
		}
		if err := e.authorizeSequenceRepair(gdb, ledgerExisted); err != nil {
			return nil, err
		}
	}
	if err := ensureRepairHistoryColumns(gdb); err != nil {
		return nil, err
	}

	ledger, err := e.appliedMigrations(gdb, nil)
	if err != nil {
		return nil, err
	}
	if err := e.adoptLegacyDialect(gdb, ledger, lock.owner); err != nil {
		return nil, err
	}
	if err := e.validateLedgerDialect(ledger); err != nil {
		return nil, err
	}
	if diff := e.diffMigrations(files, ledger); len(diff.Dirty) > 0 {
		if err := e.ensureMigrationFence(gdb, lock.owner); err != nil {
			return nil, err
		}
	}
	ledgerByVersion := make(map[int64]AppliedMigration, len(ledger))
	for _, applied := range ledger {
		ledgerByVersion[applied.Version] = applied
	}
	row, ok := ledgerByVersion[opts.Version]
	if !ok {
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
	err = e.withMigrationLeaseTransaction(gdb, lock.owner, func(tx *gorm.DB) error {
		var res *gorm.DB
		switch opts.Action {
		case RepairRetry:
			if !row.Dirty {
				return fmt.Errorf("migration %d is not dirty", row.Version)
			}
			res = tx.Exec(
				"DELETE FROM "+e.seq.ledger+" WHERE version = ? AND dirty = TRUE AND checksum = ?",
				row.Version, opts.ExpectedChecksum,
			)
		case RepairMarkApplied:
			if !row.Dirty {
				return fmt.Errorf("migration %d is not dirty", row.Version)
			}
			res = tx.Exec(
				"UPDATE "+e.seq.ledger+" SET dirty = FALSE, finished_at = ?, applied_at = ? "+
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
				"UPDATE "+e.seq.ledger+" SET checksum = ? WHERE version = ? AND dirty = FALSE AND checksum = ?",
				file.Checksum, row.Version, opts.ExpectedChecksum,
			)
		}
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("repair compare-and-swap lost (updated %d rows)", res.RowsAffected)
		}
		// PostgreSQL/MySQL hold the global lock without a ledger lease, so
		// the fence transition joins the repair's own transaction: a history
		// row then proves the CAS and the fence state committed together.
		// SQLite (lock.owner != "") keeps its fence inside the lease-release
		// path, best-effort after commit.
		if lock.owner == "" {
			if err := e.cleanupMigrationFenceIfClean(tx, ""); err != nil {
				return err
			}
		}
		return insertRepairHistory(tx, e.newLedgerRepairRecord(opts, file, row.Checksum, operator, now))
	})
	if err != nil {
		return nil, fmt.Errorf("db: repair migration %d (%s): %w", opts.Version, opts.Action, err)
	}
	return &RepairReport{
		Sequence: e.seq.kind, Ledger: e.seq.ledger, Dialect: e.seq.dialect,
		Action: opts.Action, Version: opts.Version, File: file.File,
		LedgerChecksum: row.Checksum, CurrentChecksum: file.Checksum,
		Reason: strings.TrimSpace(opts.Reason), Operator: operator,
		ChokVersion: currentChokVersion(), ResolvedAt: now,
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
	if err := validateRepairReason(opts.Reason); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	return nil
}

func versionList[T any](items []T, version func(T) int64) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = strconv.FormatInt(version(item), 10)
	}
	return strings.Join(parts, ",")
}

// The following application-sequence adapters keep the existing in-package
// characterization tests focused on the historical schema_migrations path.
// Owned-sequence tests exercise migrationEngine directly.
func ensureLedgerBase(gdb *gorm.DB) error {
	return applicationMigrationEngine(gdb.Dialector.Name(), nil).ensureLedgerBase(gdb)
}

func ensureLedgerColumns(gdb *gorm.DB) error {
	return applicationMigrationEngine(gdb.Dialector.Name(), nil).ensureLedgerColumns(gdb)
}

func applyOne(ctx context.Context, gdb *gorm.DB, m Migration, owner string) error {
	return applicationMigrationEngine(gdb.Dialector.Name(), nil).applyOne(ctx, gdb, m, owner)
}

func verifyMigrationLease(gdb *gorm.DB, owner string) error {
	return applicationMigrationEngine(gdb.Dialector.Name(), nil).verifyMigrationLease(gdb, owner)
}

func acquireMigrationLock(ctx context.Context, gdb *gorm.DB) (migrationLock, error) {
	return applicationMigrationEngine(gdb.Dialector.Name(), nil).acquireMigrationLock(ctx, gdb)
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
func (e migrationEngine) acquireMigrationLock(ctx context.Context, gdb *gorm.DB) (migrationLock, error) {
	switch gdb.Dialector.Name() {
	case "postgres":
		release, err := acquireConnLock(ctx, gdb,
			func(ctx context.Context, conn *sql.Conn) error {
				_, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", pgAdvisoryLockKey)
				return err
			},
			func(ctx context.Context, conn *sql.Conn) error {
				var unlocked bool
				if err := conn.QueryRowContext(ctx,
					"SELECT pg_advisory_unlock($1)", pgAdvisoryLockKey).Scan(&unlocked); err != nil {
					return err
				}
				if !unlocked {
					return fmt.Errorf("db: PostgreSQL advisory lock was not held by the pinned session")
				}
				return nil
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
			func(ctx context.Context, conn *sql.Conn) error {
				var released sql.NullInt64
				if err := conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", mysqlLockName).Scan(&released); err != nil {
					return err
				}
				if !released.Valid || released.Int64 != 1 {
					return fmt.Errorf("db: MySQL migration lock %q was not held by the pinned session", mysqlLockName)
				}
				return nil
			})
		return migrationLock{release: release}, err
	case "sqlite":
		// The lease lives in the ledger; self-ensure keeps direct internal callers safe.
		if err := e.ensureLedgerBase(gdb); err != nil {
			return migrationLock{}, fmt.Errorf("db: ensure %s for sqlite migration lease: %w", e.seq.ledger, err)
		}
		return e.acquireSQLiteMigrationLease(ctx, gdb)
	default:
		return migrationLock{release: func() {}}, nil
	}
}

func (e migrationEngine) sqliteLeaseLock(ctx context.Context, gdb *gorm.DB, owner string) migrationLock {
	return migrationLock{
		owner: owner,
		release: func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrationCleanupTimeout)
			defer cancel()
			_ = e.releaseSQLiteMigrationLease(gdb.WithContext(cleanupCtx), owner)
		},
	}
}

func (e migrationEngine) acquireSQLiteMigrationLease(ctx context.Context, gdb *gorm.DB) (migrationLock, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return migrationLock{}, fmt.Errorf("db: generate sqlite migration lease owner: %w", err)
	}
	owner := sqliteLeasePrefix + hex.EncodeToString(random)

	for {
		now := time.Now().UTC()
		res := gdb.WithContext(ctx).Exec(
			"INSERT OR IGNORE INTO "+e.seq.ledger+" (version, name, applied_at) VALUES (?, ?, ?)",
			migrationFenceVersion, owner, now,
		)
		if res.Error == nil && res.RowsAffected == 1 {
			return e.sqliteLeaseLock(ctx, gdb, owner), nil
		}
		if res.Error != nil && !sqliteLockContention(res.Error) {
			return migrationLock{}, fmt.Errorf("db: acquire sqlite migration lease: %w", res.Error)
		}

		var existing struct {
			Name      string
			AppliedAt time.Time
		}
		readErr := gdb.WithContext(ctx).Raw(
			"SELECT name, applied_at FROM "+e.seq.ledger+" WHERE version = ?",
			migrationFenceVersion,
		).Scan(&existing).Error
		if readErr == nil && existing.Name != "" && now.Sub(existing.AppliedAt) >= sqliteLeaseTTL {
			takeover := gdb.WithContext(ctx).Exec(
				"UPDATE "+e.seq.ledger+" SET name = ?, applied_at = ? WHERE version = ? AND name = ? AND applied_at = ?",
				owner, now, migrationFenceVersion, existing.Name, existing.AppliedAt,
			)
			if takeover.Error == nil && takeover.RowsAffected == 1 {
				return e.sqliteLeaseLock(ctx, gdb, owner), nil
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

func (e migrationEngine) releaseSQLiteMigrationLease(gdb *gorm.DB, owner string) error {
	dirty, err := e.hasDirtyMigrations(gdb)
	if err != nil {
		return err
	}
	if dirty {
		// The active call has returned, so make the retained old-engine fence
		// immediately reclaimable by a new repair/up process. A hard process
		// crash cannot run this release path and instead relies on lease TTL.
		if err := gdb.Exec(
			"UPDATE "+e.seq.ledger+" SET name = ?, applied_at = ? WHERE version = ? AND name = ?",
			migrationFenceName, time.Unix(0, 0).UTC(), migrationFenceVersion, owner,
		).Error; err != nil {
			return fmt.Errorf("db: release sqlite migration lease as dirty fence: %w", err)
		}
		return nil
	}
	return e.cleanupMigrationFenceIfClean(gdb, owner)
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
	unlock func(context.Context, *sql.Conn) error,
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
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), migrationCleanupTimeout)
		defer cancel()
		if err := unlock(cleanupCtx, conn); err != nil {
			// sql.Conn.Close returns a healthy physical connection to the
			// pool; it does not end the server session. Force database/sql to
			// discard this connection when unlock cannot be confirmed.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, nil
}

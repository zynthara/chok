package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v2/version"
)

const (
	sequenceManifestTable = "schema_migrations_chok_manifest"

	// MigrationEngineGeneration is the manifest protocol generation understood
	// by this build. A sequence whose persisted engine floor is higher is
	// observable through status APIs but cannot be mutated by this engine.
	MigrationEngineGeneration = 1
)

var (
	// ErrSequenceClaimConflict means a migration kind is owned by a different
	// component identity in the database manifest.
	ErrSequenceClaimConflict = errors.New("db: migration sequence claim conflict")
	// ErrSequenceManifestCorrupt means persisted manifest identity metadata is
	// internally inconsistent or unsafe to use.
	ErrSequenceManifestCorrupt = errors.New("db: migration sequence manifest is corrupt")
	// ErrSequenceUnclaimed means an operation that requires an existing claim
	// was asked to act on a pre-manifest ledger.
	ErrSequenceUnclaimed = errors.New("db: migration sequence is unclaimed")
	// ErrMigrationEngineTooOld means the manifest requires a newer protocol
	// generation than this build implements.
	ErrMigrationEngineTooOld = errors.New("db: migration engine generation is too old")
)

// ManifestEntry is the persisted owner and compatibility metadata for one
// component-owned migration sequence. ComponentVersion and ChokVersion are
// informational; EngineFloor is the compatibility decision boundary.
type ManifestEntry struct {
	Kind             string
	Ledger           string
	Owner            string
	EngineFloor      int
	ComponentVersion string
	ChokVersion      string
	Provenance       string
	ClaimedAt        time.Time
	UpdatedAt        time.Time
}

// EngineCompatible reports whether this build may mutate the sequence.
func (e ManifestEntry) EngineCompatible() bool {
	return e.EngineFloor <= MigrationEngineGeneration
}

// SequenceLedgerSnapshot is the file-independent health view of one owned
// migration ledger. It deliberately cannot report pending or drifted files.
type SequenceLedgerSnapshot struct {
	Kind       string
	Ledger     string
	Dialect    string
	Exists     bool
	Frontier   int64
	Rows       int
	Dirty      int
	Unverified int
	Fence      *MigrationFenceStatus
}

// RepairClaimOptions transfers an existing manifest claim with an exact-owner
// compare-and-swap guard. It cannot create the first claim for an unclaimed
// ledger; ApplySequence owns trust-on-first-use adoption. Reason is mandatory
// — an ownership transfer is the audit action that most needs one. Operator
// follows the same contract as RepairOptions.Operator.
type RepairClaimOptions struct {
	ExpectedOwner string
	NewOwner      string
	Reason        string
	Operator      string
}

// RepairClaimReport records one successful sequence-claim transfer. The same
// evidence lands in the append-only repair history table in the transfer's
// own transaction; Operator and ChokVersion echo the persisted values.
type RepairClaimReport struct {
	Kind          string
	Ledger        string
	Dialect       string
	PreviousOwner string
	NewOwner      string
	Reason        string
	Operator      string
	ChokVersion   string
	RepairedAt    time.Time
}

type tableColumns map[string]bool

func (columns tableColumns) has(name string) bool {
	return columns == nil || columns[strings.ToLower(name)]
}

// ensureManifestBase creates the shared manifest table on first use. Callers
// must hold the migration lock — PostgreSQL's CREATE TABLE IF NOT EXISTS
// races on the catalog uniques when two sessions create the table at once.
func ensureManifestBase(gdb *gorm.DB) error {
	return gdb.Exec(
		"CREATE TABLE IF NOT EXISTS " + sequenceManifestTable + " (" +
			"kind VARCHAR(31) PRIMARY KEY, " +
			"ledger VARCHAR(64) NOT NULL, " +
			"owner VARCHAR(190) NOT NULL, " +
			"engine_floor INT NOT NULL DEFAULT 1, " +
			"component_version VARCHAR(64) NOT NULL DEFAULT '', " +
			"chok_version VARCHAR(64) NOT NULL DEFAULT '', " +
			"provenance VARCHAR(32) NOT NULL DEFAULT '', " +
			"claimed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, " +
			"updated_at TIMESTAMP NULL)",
	).Error
}

// inspectColumns snapshots one table's lowercase column set. Shared by the
// manifest and repair-history additive-upgrade and fallback-read paths.
func inspectColumns(gdb *gorm.DB, table string) (tableColumns, error) {
	types, err := gdb.Migrator().ColumnTypes(table)
	if err != nil {
		return nil, fmt.Errorf("db: inspect %s columns: %w", table, err)
	}
	columns := make(tableColumns, len(types))
	for _, column := range types {
		columns[strings.ToLower(column.Name())] = true
	}
	return columns, nil
}

func inspectManifestColumns(gdb *gorm.DB) (tableColumns, error) {
	return inspectColumns(gdb, sequenceManifestTable)
}

// ensureManifestColumns is called while the sequence migration lock is held.
// PostgreSQL and MySQL currently serialize every kind with one global lock.
// SQLite's locks are per-ledger, so its short BEGIN IMMEDIATE transaction is
// the independent database-wide guard for the shared manifest table.
func ensureManifestColumns(gdb *gorm.DB) error {
	if gdb.Dialector.Name() == "sqlite" {
		return gdb.Transaction(func(tx *gorm.DB) error {
			return ensureManifestColumnsLocked(tx)
		})
	}
	return ensureManifestColumnsLocked(gdb)
}

func ensureManifestColumnsLocked(gdb *gorm.DB) error {
	existing, err := inspectManifestColumns(gdb)
	if err != nil {
		return err
	}
	for _, core := range []string{"kind", "ledger", "owner"} {
		if !existing.has(core) {
			return fmt.Errorf("%w: %s is missing identity column %s", ErrSequenceManifestCorrupt, sequenceManifestTable, core)
		}
	}
	columns := []struct {
		name string
		ddl  string
	}{
		{"engine_floor", "INT NOT NULL DEFAULT 1"},
		{"component_version", "VARCHAR(64) NOT NULL DEFAULT ''"},
		{"chok_version", "VARCHAR(64) NOT NULL DEFAULT ''"},
		{"provenance", "VARCHAR(32) NOT NULL DEFAULT ''"},
		// SQLite cannot add a column with a non-constant CURRENT_TIMESTAMP
		// default. New writes always provide claimed_at explicitly, so a
		// nullable additive column is the portable legacy upgrade shape.
		{"claimed_at", "TIMESTAMP NULL"},
		{"updated_at", "TIMESTAMP NULL"},
	}
	for _, column := range columns {
		if existing.has(column.name) {
			continue
		}
		err := gdb.Exec("ALTER TABLE " + sequenceManifestTable + " ADD COLUMN " + column.name + " " + column.ddl).Error
		if err != nil {
			// MySQL duplicate-column is 1060. The global GET_LOCK makes the
			// branch defensive today; retaining it keeps a future finer lock
			// granularity idempotent without confusing it with index error 1061.
			msg := strings.ToLower(err.Error())
			if gdb.Dialector.Name() != "mysql" ||
				(!strings.Contains(msg, "error 1060") && !strings.Contains(msg, "duplicate column")) {
				return fmt.Errorf("db: upgrade %s add %s: %w", sequenceManifestTable, column.name, err)
			}
			refreshed, inspectErr := inspectManifestColumns(gdb)
			if inspectErr != nil || !refreshed.has(column.name) {
				return fmt.Errorf("db: upgrade %s add %s after duplicate-column race: %w", sequenceManifestTable, column.name, errors.Join(err, inspectErr))
			}
		}
		existing[column.name] = true
	}
	return nil
}

// ManifestEntries returns every claimed sequence in kind order. It is strictly
// read-only: a missing manifest returns an empty slice, and a legacy partial
// manifest is read through column fallbacks rather than upgraded.
func ManifestEntries(ctx context.Context, h *DB) ([]ManifestEntry, error) {
	if h == nil || h.gdb == nil {
		return nil, fmt.Errorf("db: read migration manifest: nil database handle")
	}
	gdb := h.gdb.WithContext(ctx)
	if !gdb.Migrator().HasTable(sequenceManifestTable) {
		return []ManifestEntry{}, nil
	}
	columns, err := inspectManifestColumns(gdb)
	if err != nil {
		return nil, err
	}
	return manifestEntries(gdb, columns)
}

func manifestEntries(gdb *gorm.DB, columns tableColumns) ([]ManifestEntry, error) {
	return manifestEntriesWithPolicy(gdb, columns, "")
}

func manifestEntriesWithPolicy(gdb *gorm.DB, columns tableColumns, allowedReservedOwnerMismatch string) ([]ManifestEntry, error) {
	for _, core := range []string{"kind", "ledger", "owner"} {
		if !columns.has(core) {
			return nil, fmt.Errorf("%w: %s is missing identity column %s", ErrSequenceManifestCorrupt, sequenceManifestTable, core)
		}
	}
	expr := func(column, fallback string) string {
		if columns.has(column) {
			return column
		}
		return fallback
	}
	query := "SELECT kind, ledger, owner, " +
		expr("engine_floor", "1") + ", " +
		expr("component_version", "''") + ", " +
		expr("chok_version", "''") + ", " +
		expr("provenance", "''") + ", " +
		expr("claimed_at", "NULL") + ", " +
		expr("updated_at", "NULL") +
		" FROM " + sequenceManifestTable + " ORDER BY kind"
	rows, err := gdb.Raw(query).Rows()
	if err != nil {
		return nil, fmt.Errorf("db: read %s: %w", sequenceManifestTable, err)
	}
	defer rows.Close()

	entries := make([]ManifestEntry, 0)
	for rows.Next() {
		var (
			entry            ManifestEntry
			componentVersion sql.NullString
			chokVersion      sql.NullString
			provenance       sql.NullString
			claimed, updated sql.NullTime
		)
		if err := rows.Scan(
			&entry.Kind, &entry.Ledger, &entry.Owner, &entry.EngineFloor,
			&componentVersion, &chokVersion, &provenance, &claimed, &updated,
		); err != nil {
			return nil, fmt.Errorf("db: scan %s: %w", sequenceManifestTable, err)
		}
		entry.ComponentVersion = componentVersion.String
		entry.ChokVersion = chokVersion.String
		entry.Provenance = provenance.String
		if claimed.Valid {
			entry.ClaimedAt = claimed.Time
		}
		if updated.Valid {
			entry.UpdatedAt = updated.Time
		}
		if err := validateStoredManifestEntry(entry, allowedReservedOwnerMismatch); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate %s: %w", sequenceManifestTable, err)
	}
	return entries, nil
}

func validateStoredManifestEntry(entry ManifestEntry, allowedReservedOwnerMismatch string) error {
	if err := ValidateSequenceKind(entry.Kind); err != nil {
		return fmt.Errorf("%w: %v", ErrSequenceManifestCorrupt, err)
	}
	if entry.Ledger != ledgerForSequenceKind(entry.Kind) {
		return fmt.Errorf("%w: kind %q records ledger %q, want %q", ErrSequenceManifestCorrupt, entry.Kind, entry.Ledger, ledgerForSequenceKind(entry.Kind))
	}
	if err := validateSequenceOwner(entry.Owner); err != nil {
		return fmt.Errorf("%w: kind %q owner: %v", ErrSequenceManifestCorrupt, entry.Kind, err)
	}
	if entry.EngineFloor < 1 {
		return fmt.Errorf("%w: kind %q has invalid engine floor %d", ErrSequenceManifestCorrupt, entry.Kind, entry.EngineFloor)
	}
	if err := validateSequenceVersion(entry.ComponentVersion); err != nil {
		return fmt.Errorf("%w: kind %q component version: %v", ErrSequenceManifestCorrupt, entry.Kind, err)
	}
	if err := validateSequenceVersion(entry.ChokVersion); err != nil {
		return fmt.Errorf("%w: kind %q chok version: %v", ErrSequenceManifestCorrupt, entry.Kind, err)
	}
	if entry.Provenance != "" && entry.Provenance != "claimed" && entry.Provenance != "adopted" {
		return fmt.Errorf("%w: kind %q has invalid provenance %q", ErrSequenceManifestCorrupt, entry.Kind, entry.Provenance)
	}
	if expected, reserved := reservedSequenceOwners[entry.Kind]; reserved && entry.Owner != expected && entry.Kind != allowedReservedOwnerMismatch {
		return fmt.Errorf("%w: reserved kind %q records owner %q, want %q", ErrSequenceManifestCorrupt, entry.Kind, entry.Owner, expected)
	}
	return nil
}

func manifestEntry(gdb *gorm.DB, kind string) (ManifestEntry, bool, error) {
	return manifestEntryWithPolicy(gdb, kind, "")
}

func manifestEntryWithPolicy(gdb *gorm.DB, kind, allowedReservedOwnerMismatch string) (ManifestEntry, bool, error) {
	entries, err := manifestEntriesWithPolicy(gdb, nil, allowedReservedOwnerMismatch)
	if err != nil {
		return ManifestEntry{}, false, err
	}
	i := sort.Search(len(entries), func(i int) bool { return entries[i].Kind >= kind })
	if i >= len(entries) || entries[i].Kind != kind {
		return ManifestEntry{}, false, nil
	}
	return entries[i], true, nil
}

// manifestClaimExists is a read-only pre-ensure probe. It preserves the
// distinction between a fresh sequence and a manifest claim whose ledger was
// deleted, so apply cannot recreate and replay the missing ledger by accident.
func manifestClaimExists(gdb *gorm.DB, kind string) (bool, error) {
	if !gdb.Migrator().HasTable(sequenceManifestTable) {
		return false, nil
	}
	columns, err := inspectManifestColumns(gdb)
	if err != nil {
		return false, err
	}
	entries, err := manifestEntries(gdb, columns)
	if err != nil {
		return false, err
	}
	i := sort.Search(len(entries), func(i int) bool { return entries[i].Kind >= kind })
	return i < len(entries) && entries[i].Kind == kind, nil
}

// preflightSequenceClaim reads the pre-ensure claim evidence for an owned
// sequence — whether its ledger already existed — and refuses the corrupt
// state where a manifest claim outlived its ledger, so apply can never
// recreate and silently replay a deleted ledger. It must stay before the
// migration lock: the SQLite lock creates the ledger as a lease side
// effect, which would destroy the missing-table evidence. Running unlocked
// means the catalog reads can interleave with a concurrent first claim, so
// the read order is what makes the verdict sound — claims are only ever
// inserted after their ledger exists, hence observe the claim first and
// re-read the ledger second. A ledger still missing after its claim was
// seen was dropped after claiming (the exact corruption guarded against),
// while one that appears on the re-read is a first claim that converged
// mid-probe and reads as existing.
func (e migrationEngine) preflightSequenceClaim(gdb *gorm.DB) (bool, error) {
	if e.seq.owner == "" {
		return false, nil
	}
	if gdb.Migrator().HasTable(e.seq.ledger) {
		return true, nil
	}
	claimExists, err := manifestClaimExists(gdb, e.seq.kind)
	if err != nil {
		return false, err
	}
	if !claimExists {
		return false, nil
	}
	if gdb.Migrator().HasTable(e.seq.ledger) {
		return true, nil
	}
	return false, fmt.Errorf("%w: migration kind %q claim exists without ledger %s", ErrSequenceManifestCorrupt, e.seq.kind, e.seq.ledger)
}

func currentChokVersion() string {
	return sanitizeSequenceVersion(version.Get().Version)
}

// sanitizeSequenceVersion coerces a build-supplied version string into a
// value validateSequenceVersion always accepts. The write side must never
// persist metadata the read side rejects: a byte-level truncation that split
// a UTF-8 rune would turn every later manifest read into a corruption error.
func sanitizeSequenceVersion(value string) string {
	if len(value) > maxSequenceVersionBytes {
		value = strings.ToValidUTF8(value[:maxSequenceVersionBytes], "")
	}
	if validateSequenceVersion(value) != nil {
		return ""
	}
	return value
}

func newManifestEntry(e migrationEngine, provenance string) ManifestEntry {
	now := time.Now().UTC()
	return ManifestEntry{
		Kind: e.seq.kind, Ledger: e.seq.ledger, Owner: e.seq.owner,
		EngineFloor: MigrationEngineGeneration, ComponentVersion: e.seq.componentVersion,
		ChokVersion: currentChokVersion(), Provenance: provenance,
		ClaimedAt: now, UpdatedAt: now,
	}
}

func insertManifestEntry(gdb *gorm.DB, e migrationEngine, provenance string) (ManifestEntry, error) {
	entry := newManifestEntry(e, provenance)
	err := gdb.Exec(
		"INSERT INTO "+sequenceManifestTable+" (kind, ledger, owner, engine_floor, component_version, chok_version, provenance, claimed_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		entry.Kind, entry.Ledger, entry.Owner, entry.EngineFloor, entry.ComponentVersion,
		entry.ChokVersion, entry.Provenance, entry.ClaimedAt, entry.UpdatedAt,
	).Error
	if err == nil {
		return entry, nil
	}
	// A concurrent first insert converges through the primary key. Re-read on
	// every error so driver-specific duplicate-key strings never become API.
	existing, ok, readErr := manifestEntry(gdb, e.seq.kind)
	if readErr != nil || !ok {
		return ManifestEntry{}, fmt.Errorf("db: claim migration sequence %s: %w", e.seq.kind, errors.Join(err, readErr))
	}
	if validateErr := validateManifestClaim(existing, e); validateErr != nil {
		return ManifestEntry{}, validateErr
	}
	return existing, nil
}

func validateManifestClaim(entry ManifestEntry, e migrationEngine) error {
	if entry.Ledger != e.seq.ledger {
		return fmt.Errorf("%w: migration kind %q records ledger %q, want %q", ErrSequenceManifestCorrupt, e.seq.kind, entry.Ledger, e.seq.ledger)
	}
	if entry.EngineFloor > MigrationEngineGeneration {
		return fmt.Errorf("%w: migration kind %q requires engine generation %d; this build supports %d", ErrMigrationEngineTooOld, e.seq.kind, entry.EngineFloor, MigrationEngineGeneration)
	}
	if entry.Owner != e.seq.owner {
		return fmt.Errorf("%w: migration kind %q is claimed by %q; this sequence declares %q — rename the kind or transfer the claim with chok migrate repair claim", ErrSequenceClaimConflict, e.seq.kind, entry.Owner, e.seq.owner)
	}
	return nil
}

func (e migrationEngine) authorizeSequenceWrite(gdb *gorm.DB, ledgerExisted bool) (bool, error) {
	entry, exists, err := manifestEntry(gdb, e.seq.kind)
	if err != nil {
		return false, err
	}
	if exists {
		return false, validateManifestClaim(entry, e)
	}
	if ledgerExisted {
		return true, nil
	}
	entry, err = insertManifestEntry(gdb, e, "claimed")
	if err != nil {
		return false, err
	}
	return false, validateManifestClaim(entry, e)
}

func (e migrationEngine) authorizeSequenceRepair(gdb *gorm.DB, ledgerExisted bool) error {
	entry, exists, err := manifestEntry(gdb, e.seq.kind)
	if err != nil {
		return err
	}
	if exists {
		return validateManifestClaim(entry, e)
	}
	if ledgerExisted {
		return nil // explicit repair of a pre-manifest ledger remains available
	}
	return fmt.Errorf("%w: migration kind %q has no existing ledger", ErrSequenceUnclaimed, e.seq.kind)
}

func (e migrationEngine) adoptSequenceManifest(gdb *gorm.DB) error {
	entry, err := insertManifestEntry(gdb, e, "adopted")
	if err != nil {
		return err
	}
	return validateManifestClaim(entry, e)
}

func (e migrationEngine) refreshSequenceManifest(gdb *gorm.DB) error {
	now := time.Now().UTC()
	res := gdb.Exec(
		"UPDATE "+sequenceManifestTable+" SET component_version = ?, chok_version = ?, updated_at = ? "+
			"WHERE kind = ? AND owner = ? AND engine_floor <= ?",
		e.seq.componentVersion, currentChokVersion(), now,
		e.seq.kind, e.seq.owner, MigrationEngineGeneration,
	)
	if res.Error != nil {
		return fmt.Errorf("db: refresh migration manifest for %s: %w", e.seq.kind, res.Error)
	}
	if res.RowsAffected != 1 {
		entry, exists, err := manifestEntry(gdb, e.seq.kind)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: migration kind %q claim disappeared", ErrSequenceManifestCorrupt, e.seq.kind)
		}
		return validateManifestClaim(entry, e)
	}
	return nil
}

// LedgerSnapshot returns file-independent health information for an owned
// ledger. It validates kind before deriving the SQL identifier and never uses
// a manifest ledger value as executable SQL.
func LedgerSnapshot(ctx context.Context, h *DB, kind string) (*SequenceLedgerSnapshot, error) {
	if h == nil || h.gdb == nil {
		return nil, fmt.Errorf("db: read owned migration ledger: nil database handle")
	}
	if err := ValidateSequenceKind(kind); err != nil {
		return nil, err
	}
	// Validate any persisted manifest identity before touching the derived
	// ledger. The ledger column remains display-only and is never interpolated
	// into SQL, but a mismatch still means the catalog is corrupt.
	if _, err := manifestClaimExists(h.gdb.WithContext(ctx), kind); err != nil {
		return nil, err
	}
	dialect := h.gdb.Dialector.Name()
	e := migrationEngine{seq: migrationSequence{kind: kind, ledger: ledgerForSequenceKind(kind), dialect: dialect}}
	gdb := h.gdb.WithContext(ctx)
	snapshot := &SequenceLedgerSnapshot{Kind: kind, Ledger: e.seq.ledger, Dialect: dialect}
	if !gdb.Migrator().HasTable(e.seq.ledger) {
		return snapshot, nil
	}
	snapshot.Exists = true
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
	for _, row := range ledger {
		snapshot.Rows++
		if row.Version > snapshot.Frontier {
			snapshot.Frontier = row.Version
		}
		if row.Dirty {
			snapshot.Dirty++
		}
		if row.Checksum == "" {
			snapshot.Unverified++
		}
	}
	snapshot.Fence, err = e.migrationFenceStatus(gdb)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

// RepairSequenceClaim transfers an existing claim under the same migration
// lock as apply and repair. It never creates or adopts a missing claim or
// ledger, and refuses to touch a sequence whose engine floor is too new.
func RepairSequenceClaim(ctx context.Context, h *DB, kind string, opts RepairClaimOptions) (*RepairClaimReport, error) {
	if h == nil || h.gdb == nil {
		return nil, fmt.Errorf("db: repair migration sequence claim: nil database handle")
	}
	if err := ValidateSequenceKind(kind); err != nil {
		return nil, err
	}
	if err := validateSequenceOwner(opts.ExpectedOwner); err != nil {
		return nil, fmt.Errorf("db: expected claim owner: %w", err)
	}
	if err := validateSequenceOwner(opts.NewOwner); err != nil {
		return nil, fmt.Errorf("db: new claim owner: %w", err)
	}
	if expected, reserved := reservedSequenceOwners[kind]; reserved && opts.NewOwner != expected {
		return nil, fmt.Errorf("db: reserved migration kind %q can only be restored to owner %q", kind, expected)
	}
	if err := validateRepairReason(opts.Reason); err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	operator, err := resolveRepairOperator(opts.Operator)
	if err != nil {
		return nil, err
	}

	gdb := h.gdb.WithContext(ctx)
	ledger := ledgerForSequenceKind(kind)
	if !gdb.Migrator().HasTable(sequenceManifestTable) {
		return nil, fmt.Errorf("%w: migration kind %q has no manifest claim", ErrSequenceUnclaimed, kind)
	}
	if !gdb.Migrator().HasTable(ledger) {
		return nil, fmt.Errorf("%w: migration kind %q claim exists without ledger %s", ErrSequenceManifestCorrupt, kind, ledger)
	}
	e := migrationEngine{seq: migrationSequence{kind: kind, ledger: ledger, dialect: gdb.Dialector.Name()}}
	lock, err := e.acquireMigrationLock(ctx, gdb)
	if err != nil {
		return nil, err
	}
	defer lock.release()
	if err := ensureRepairHistoryBase(gdb); err != nil {
		return nil, fmt.Errorf("db: ensure %s base: %w", sequenceRepairHistoryTable, err)
	}
	if err := ensureManifestColumns(gdb); err != nil {
		return nil, err
	}
	if err := ensureRepairHistoryColumns(gdb); err != nil {
		return nil, err
	}
	if !gdb.Migrator().HasTable(ledger) {
		return nil, fmt.Errorf("%w: migration kind %q claim exists without ledger %s", ErrSequenceManifestCorrupt, kind, ledger)
	}
	// A reserved claim may be repaired only back to its canonical owner, so
	// this recovery path must be able to read that one otherwise-invalid row.
	entry, exists, err := manifestEntryWithPolicy(gdb, kind, kind)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: migration kind %q must be adopted by ApplySequence before transfer", ErrSequenceUnclaimed, kind)
	}
	if entry.EngineFloor > MigrationEngineGeneration {
		return nil, fmt.Errorf("%w: migration kind %q requires engine generation %d; this build supports %d", ErrMigrationEngineTooOld, kind, entry.EngineFloor, MigrationEngineGeneration)
	}
	if entry.Owner != opts.ExpectedOwner {
		return nil, fmt.Errorf("db: repair migration kind %q claim: expected owner %q, manifest has %q", kind, opts.ExpectedOwner, entry.Owner)
	}
	now := time.Now().UTC()
	reason := strings.TrimSpace(opts.Reason)
	// The owner CAS and its history row commit together: a transfer that
	// cannot be recorded must not happen.
	err = e.withMigrationLeaseTransaction(gdb, lock.owner, func(tx *gorm.DB) error {
		res := tx.Exec(
			"UPDATE "+sequenceManifestTable+" SET owner = ?, component_version = '', chok_version = ?, updated_at = ? "+
				"WHERE kind = ? AND owner = ? AND engine_floor <= ?",
			opts.NewOwner, currentChokVersion(), now, kind, opts.ExpectedOwner, MigrationEngineGeneration,
		)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("claim changed concurrently")
		}
		return insertRepairHistory(tx, RepairRecord{
			Kind: kind, Ledger: ledger, Dialect: e.seq.dialect,
			Action: repairActionClaimTransfer, Version: 0,
			PreviousOwner: opts.ExpectedOwner, NewOwner: opts.NewOwner,
			Reason: reason, Operator: operator,
			ChokVersion: currentChokVersion(), RepairedAt: now,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("db: repair migration kind %q claim: %w", kind, err)
	}
	return &RepairClaimReport{
		Kind: kind, Ledger: ledger, Dialect: e.seq.dialect,
		PreviousOwner: opts.ExpectedOwner, NewOwner: opts.NewOwner,
		Reason: reason, Operator: operator, ChokVersion: currentChokVersion(),
		RepairedAt: now,
	}, nil
}

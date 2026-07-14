package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"
)

// sequenceRepairHistoryTable is the per-database append-only record of every
// repair action. Its name occupies the reserved sequence kind "repairs".
const sequenceRepairHistoryTable = "schema_migrations_chok_repairs"

// repairActionClaimTransfer is the history action recorded by
// RepairSequenceClaim. Ledger repairs record their RepairAction verbatim.
const repairActionClaimTransfer = "claim-transfer"

// repairHistoryKindApp is the history identity of the application ledger
// (schema_migrations). It is not a sequence kind: ValidateSequenceKind
// rejects it so no component can mint an ambiguous history row.
const repairHistoryKindApp = "app"

// ErrRepairHistoryCorrupt means persisted repair-history rows or the history
// table shape are internally inconsistent and cannot be trusted as evidence.
var ErrRepairHistoryCorrupt = errors.New("db: migration repair history is corrupt")

// Repair history read limits. A zero RepairHistoryFilter.Limit reads
// DefaultRepairHistoryLimit rows; larger requests are clamped to
// MaxRepairHistoryLimit (the table is append-only and never purged by the
// framework, so unbounded reads are a foot-gun, not a feature).
const (
	DefaultRepairHistoryLimit = 50
	MaxRepairHistoryLimit     = 1000
)

// RepairRecord is one persisted repair-history row. A row's existence means
// the repair's business-state compare-and-swap committed together with it in
// one transaction; it does not prove the caller observed a successful API
// return (the process can die after commit, and on SQLite the post-commit
// lease release is best-effort). The table's internal auto-increment key is
// deliberately not exposed.
type RepairRecord struct {
	Kind            string // sequence kind, or "app" for the application ledger
	Ledger          string
	Dialect         string
	Action          string // retry | mark-applied | accept-drift | claim-transfer
	Version         int64  // 0 for claim-transfer
	File            string
	LedgerChecksum  string
	CurrentChecksum string
	PreviousOwner   string // claim-transfer only
	NewOwner        string // claim-transfer only
	Reason          string
	Operator        string
	ChokVersion     string
	RepairedAt      time.Time
}

// RepairHistoryFilter scopes a RepairHistory read. Kind "" reads every kind;
// "app" reads the application ledger's rows; any other value must be a valid
// sequence kind. Limit 0 applies DefaultRepairHistoryLimit, negative values
// are rejected, and values above MaxRepairHistoryLimit are clamped.
type RepairHistoryFilter struct {
	Kind  string
	Limit int
}

// ensureRepairHistoryBase creates the append-only history table. The three
// blessed dialects need distinct auto-increment spellings; anything else
// fails closed rather than minting a table whose inserts cannot work.
//
// Column contract: the CREATE-time set below is frozen. reason and
// repaired_at carry no default, so they can never be added by ALTER — every
// future column must be expressible as an additive DEFAULT/nullable ALTER
// handled in ensureRepairHistoryColumnsLocked.
func ensureRepairHistoryBase(gdb *gorm.DB) error {
	var idColumn string
	switch gdb.Dialector.Name() {
	case "sqlite":
		idColumn = "id INTEGER PRIMARY KEY AUTOINCREMENT"
	case "mysql":
		idColumn = "id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY"
	case "postgres":
		idColumn = "id BIGSERIAL PRIMARY KEY"
	default:
		return fmt.Errorf("db: repair history requires sqlite, mysql or postgres, got %q", gdb.Dialector.Name())
	}
	return gdb.Exec(
		"CREATE TABLE IF NOT EXISTS " + sequenceRepairHistoryTable + " (" +
			idColumn + ", " +
			"kind VARCHAR(31) NOT NULL, " +
			"ledger VARCHAR(64) NOT NULL, " +
			"dialect VARCHAR(32) NOT NULL, " +
			"action VARCHAR(32) NOT NULL, " +
			"version BIGINT NOT NULL DEFAULT 0, " +
			"file VARCHAR(255) NOT NULL DEFAULT '', " +
			"ledger_checksum VARCHAR(64) NOT NULL DEFAULT '', " +
			"current_checksum VARCHAR(64) NOT NULL DEFAULT '', " +
			"previous_owner VARCHAR(190) NOT NULL DEFAULT '', " +
			"new_owner VARCHAR(190) NOT NULL DEFAULT '', " +
			"reason TEXT NOT NULL, " +
			"operator VARCHAR(190) NOT NULL DEFAULT '', " +
			"chok_version VARCHAR(64) NOT NULL DEFAULT '', " +
			"repaired_at TIMESTAMP NOT NULL)",
	).Error
}

// repairHistoryCoreColumns hold evidence whose legal emptiness depends on
// the action (file, checksums, owners) or that cannot carry a default at all
// (reason, repaired_at). An additive DEFAULT-backfill of such a column would
// make rows written before the upgrade indistinguishable from tampered ones,
// so the set is frozen at CREATE time and its absence means the table was
// not created by this engine.
var repairHistoryCoreColumns = []string{
	"id", "kind", "ledger", "dialect", "action", "version",
	"file", "ledger_checksum", "current_checksum", "previous_owner", "new_owner",
	"reason", "repaired_at",
}

// repairHistoryAdditiveColumns is the DEFAULT-able tail future versions may
// extend. Evolution contract: a column may only be additive when the empty
// string is a legal value for EVERY row regardless of action — the backfill
// an ALTER produces must be indistinguishable from an honest empty value.
// Readers fall back to the empty string when one is absent.
var repairHistoryAdditiveColumns = []struct {
	name string
	ddl  string
}{
	{"operator", "VARCHAR(190) NOT NULL DEFAULT ''"},
	{"chok_version", "VARCHAR(64) NOT NULL DEFAULT ''"},
}

// ensureRepairHistoryColumns upgrades a legacy history table additively while
// the caller holds the migration lock. PostgreSQL and MySQL serialize every
// kind under one global lock; SQLite's locks are per-ledger, so the short
// write transaction is the database-wide guard (same shape as the manifest).
func ensureRepairHistoryColumns(gdb *gorm.DB) error {
	if gdb.Dialector.Name() == "sqlite" {
		return gdb.Transaction(func(tx *gorm.DB) error {
			return ensureRepairHistoryColumnsLocked(tx)
		})
	}
	return ensureRepairHistoryColumnsLocked(gdb)
}

func ensureRepairHistoryColumnsLocked(gdb *gorm.DB) error {
	existing, err := inspectColumns(gdb, sequenceRepairHistoryTable)
	if err != nil {
		return err
	}
	for _, core := range repairHistoryCoreColumns {
		if !existing.has(core) {
			return fmt.Errorf("%w: %s is missing core column %s", ErrRepairHistoryCorrupt, sequenceRepairHistoryTable, core)
		}
	}
	for _, column := range repairHistoryAdditiveColumns {
		if existing.has(column.name) {
			continue
		}
		err := gdb.Exec("ALTER TABLE " + sequenceRepairHistoryTable + " ADD COLUMN " + column.name + " " + column.ddl).Error
		if err != nil {
			// Same duplicate-column race tolerance as the manifest upgrade:
			// MySQL 1060 under a hypothetically finer lock stays idempotent.
			msg := strings.ToLower(err.Error())
			if gdb.Dialector.Name() != "mysql" ||
				(!strings.Contains(msg, "error 1060") && !strings.Contains(msg, "duplicate column")) {
				return fmt.Errorf("db: upgrade %s add %s: %w", sequenceRepairHistoryTable, column.name, err)
			}
			refreshed, inspectErr := inspectColumns(gdb, sequenceRepairHistoryTable)
			if inspectErr != nil || !refreshed.has(column.name) {
				return fmt.Errorf("db: upgrade %s add %s after duplicate-column race: %w", sequenceRepairHistoryTable, column.name, errors.Join(err, inspectErr))
			}
		}
		existing[column.name] = true
	}
	return nil
}

// insertRepairHistory appends one row inside the caller's repair transaction.
// Failure rolls the whole repair back: an unrecorded repair must not commit.
func insertRepairHistory(tx *gorm.DB, record RepairRecord) error {
	if err := tx.Exec(
		"INSERT INTO "+sequenceRepairHistoryTable+
			" (kind, ledger, dialect, action, version, file, ledger_checksum, current_checksum,"+
			" previous_owner, new_owner, reason, operator, chok_version, repaired_at)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		record.Kind, record.Ledger, record.Dialect, record.Action, record.Version,
		record.File, record.LedgerChecksum, record.CurrentChecksum,
		record.PreviousOwner, record.NewOwner, record.Reason,
		record.Operator, record.ChokVersion, record.RepairedAt,
	).Error; err != nil {
		return fmt.Errorf("db: record repair history: %w", err)
	}
	return nil
}

// RepairHistory reads persisted repair evidence, most recent first. It is
// strictly read-only: a database that never repaired has no history table and
// reads as empty, and a legacy table shape is read through column fallbacks
// rather than upgraded. Any row that fails the audit validation matrix fails
// the whole read with ErrRepairHistoryCorrupt.
func RepairHistory(ctx context.Context, h *DB, filter RepairHistoryFilter) ([]RepairRecord, error) {
	if h == nil || h.gdb == nil {
		return nil, fmt.Errorf("db: read repair history: nil database handle")
	}
	if filter.Limit < 0 {
		return nil, fmt.Errorf("db: repair history limit must not be negative, got %d", filter.Limit)
	}
	limit := filter.Limit
	switch {
	case limit == 0:
		limit = DefaultRepairHistoryLimit
	case limit > MaxRepairHistoryLimit:
		limit = MaxRepairHistoryLimit
	}
	if filter.Kind != "" && filter.Kind != repairHistoryKindApp {
		if err := ValidateSequenceKind(filter.Kind); err != nil {
			return nil, err
		}
	}

	gdb := h.gdb.WithContext(ctx)
	if !gdb.Migrator().HasTable(sequenceRepairHistoryTable) {
		return []RepairRecord{}, nil
	}
	columns, err := inspectColumns(gdb, sequenceRepairHistoryTable)
	if err != nil {
		return nil, err
	}
	for _, core := range repairHistoryCoreColumns {
		if !columns.has(core) {
			return nil, fmt.Errorf("%w: %s is missing core column %s", ErrRepairHistoryCorrupt, sequenceRepairHistoryTable, core)
		}
	}
	expr := func(column, fallback string) string {
		if columns.has(column) {
			return column
		}
		return fallback
	}
	query := "SELECT kind, ledger, dialect, action, version, " +
		"file, ledger_checksum, current_checksum, previous_owner, new_owner, reason, " +
		expr("operator", "''") + ", " +
		expr("chok_version", "''") + ", " +
		"repaired_at FROM " + sequenceRepairHistoryTable
	args := make([]any, 0, 2)
	if filter.Kind != "" {
		query += " WHERE kind = ?"
		args = append(args, filter.Kind)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := gdb.Raw(query, args...).Rows()
	if err != nil {
		return nil, fmt.Errorf("db: read %s: %w", sequenceRepairHistoryTable, err)
	}
	defer rows.Close()

	records := make([]RepairRecord, 0)
	for rows.Next() {
		var record RepairRecord
		if err := rows.Scan(
			&record.Kind, &record.Ledger, &record.Dialect, &record.Action, &record.Version,
			&record.File, &record.LedgerChecksum, &record.CurrentChecksum,
			&record.PreviousOwner, &record.NewOwner, &record.Reason,
			&record.Operator, &record.ChokVersion, &record.RepairedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan %s: %w", sequenceRepairHistoryTable, err)
		}
		if err := validateStoredRepairRecord(record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate %s: %w", sequenceRepairHistoryTable, err)
	}
	return records, nil
}

// validateStoredRepairRecord is the audit trust boundary: every field of a
// history row must be internally consistent — and reachable by the write
// side — before it is shown as evidence. Core columns are unconditionally
// present (the read path fails closed on their absence), so an empty value
// in an action-required field is always tampering, never a legacy fallback.
func validateStoredRepairRecord(record RepairRecord) error {
	if record.Kind == repairHistoryKindApp {
		if record.Ledger != ledgerTable {
			return fmt.Errorf("%w: app row records ledger %q, want %s", ErrRepairHistoryCorrupt, record.Ledger, ledgerTable)
		}
	} else {
		if err := ValidateSequenceKind(record.Kind); err != nil {
			return fmt.Errorf("%w: %v", ErrRepairHistoryCorrupt, err)
		}
		if record.Ledger != ledgerForSequenceKind(record.Kind) {
			return fmt.Errorf("%w: kind %q records ledger %q, want %q", ErrRepairHistoryCorrupt, record.Kind, record.Ledger, ledgerForSequenceKind(record.Kind))
		}
	}
	switch record.Dialect {
	case "sqlite", "mysql", "postgres":
	default:
		return fmt.Errorf("%w: kind %q row has invalid dialect %q", ErrRepairHistoryCorrupt, record.Kind, record.Dialect)
	}
	if err := validateRepairReason(record.Reason); err != nil {
		return fmt.Errorf("%w: kind %q row reason: %v", ErrRepairHistoryCorrupt, record.Kind, err)
	}
	if strings.TrimSpace(record.Reason) != record.Reason {
		return fmt.Errorf("%w: kind %q row reason is not trimmed", ErrRepairHistoryCorrupt, record.Kind)
	}
	if err := validateRepairOperator(record.Operator); err != nil {
		return fmt.Errorf("%w: kind %q row operator: %v", ErrRepairHistoryCorrupt, record.Kind, err)
	}
	if err := validateSequenceVersion(record.ChokVersion); err != nil {
		return fmt.Errorf("%w: kind %q row chok version: %v", ErrRepairHistoryCorrupt, record.Kind, err)
	}
	if record.RepairedAt.IsZero() {
		return fmt.Errorf("%w: kind %q row has a zero repaired_at", ErrRepairHistoryCorrupt, record.Kind)
	}

	switch record.Action {
	case string(RepairRetry), string(RepairMarkApplied), string(RepairAcceptDrift):
		if record.Version <= 0 {
			return fmt.Errorf("%w: %s row for kind %q has version %d", ErrRepairHistoryCorrupt, record.Action, record.Kind, record.Version)
		}
		if record.PreviousOwner != "" || record.NewOwner != "" {
			return fmt.Errorf("%w: %s row for kind %q carries claim owners", ErrRepairHistoryCorrupt, record.Action, record.Kind)
		}
		if !checksumHexRe.MatchString(record.LedgerChecksum) {
			return fmt.Errorf("%w: %s row for kind %q has invalid ledger checksum", ErrRepairHistoryCorrupt, record.Action, record.Kind)
		}
		if !checksumHexRe.MatchString(record.CurrentChecksum) {
			return fmt.Errorf("%w: %s row for kind %q has invalid current checksum", ErrRepairHistoryCorrupt, record.Action, record.Kind)
		}
		if record.Action == string(RepairAcceptDrift) && record.LedgerChecksum == record.CurrentChecksum {
			return fmt.Errorf("%w: accept-drift row for kind %q records no checksum change", ErrRepairHistoryCorrupt, record.Kind)
		}
		if err := validateRepairHistoryFile(record.File, record.Version); err != nil {
			return fmt.Errorf("%w: %s row for kind %q file: %v", ErrRepairHistoryCorrupt, record.Action, record.Kind, err)
		}
	case repairActionClaimTransfer:
		if record.Version != 0 {
			return fmt.Errorf("%w: claim-transfer row for kind %q has version %d", ErrRepairHistoryCorrupt, record.Kind, record.Version)
		}
		if record.Kind == repairHistoryKindApp {
			return fmt.Errorf("%w: claim-transfer row targets the application ledger", ErrRepairHistoryCorrupt)
		}
		if err := validateSequenceOwner(record.PreviousOwner); err != nil {
			return fmt.Errorf("%w: claim-transfer row for kind %q previous owner: %v", ErrRepairHistoryCorrupt, record.Kind, err)
		}
		if err := validateSequenceOwner(record.NewOwner); err != nil {
			return fmt.Errorf("%w: claim-transfer row for kind %q new owner: %v", ErrRepairHistoryCorrupt, record.Kind, err)
		}
		if expected, reserved := reservedSequenceOwners[record.Kind]; reserved && record.NewOwner != expected {
			return fmt.Errorf("%w: claim-transfer row moves reserved kind %q to owner %q, only %q is writable", ErrRepairHistoryCorrupt, record.Kind, record.NewOwner, expected)
		}
		if record.LedgerChecksum != "" || record.CurrentChecksum != "" || record.File != "" {
			return fmt.Errorf("%w: claim-transfer row for kind %q carries ledger repair fields", ErrRepairHistoryCorrupt, record.Kind)
		}
	default:
		return fmt.Errorf("%w: kind %q row has invalid action %q", ErrRepairHistoryCorrupt, record.Kind, record.Action)
	}
	return nil
}

// validateRepairHistoryFile holds a persisted migration filename to the same
// rules LoadMigrations enforces at load time — character hygiene shared via
// validateMigrationFileNameChars plus the migration grammar — and pins the
// encoded version to the row's version. A tampered value can then neither
// impersonate another migration nor smuggle terminal escapes into rendered
// evidence, and a loadable migration can never persist a row this check
// rejects.
func validateRepairHistoryFile(file string, version int64) error {
	if err := validateMigrationFileNameChars(file); err != nil {
		return err
	}
	m := migFileRe.FindStringSubmatch(file)
	if m == nil {
		return fmt.Errorf("filename %q does not match the migration grammar", file)
	}
	encoded, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || encoded != version {
		return fmt.Errorf("filename %q encodes version %s, row records %d", file, m[1], version)
	}
	return nil
}

// validateRepairReason is the shared mandatory-reason rule for every repair
// path: ledger repairs, claim transfers, and the stored-row trust boundary.
func validateRepairReason(reason string) error {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return fmt.Errorf("repair reason must not be empty")
	}
	if len([]rune(trimmed)) > 1024 {
		return fmt.Errorf("repair reason must be at most 1024 characters")
	}
	return nil
}

// validateRepairOperator bounds the informational operator identity. Unlike
// a sequence owner it may be empty (derivation is best-effort) and may
// contain inner spaces (OS account names do); control characters and
// dialect-truncating lengths stay out so the write side never persists a
// value this read-side check rejects.
func validateRepairOperator(operator string) error {
	if operator == "" {
		return nil
	}
	if !utf8.ValidString(operator) {
		return fmt.Errorf("repair operator is not valid UTF-8")
	}
	if len(operator) > maxSequenceOwnerBytes {
		return fmt.Errorf("repair operator is %d bytes, maximum is %d", len(operator), maxSequenceOwnerBytes)
	}
	if strings.TrimSpace(operator) != operator {
		return fmt.Errorf("repair operator must not contain leading or trailing whitespace")
	}
	for _, r := range operator {
		if unicode.IsControl(r) {
			return fmt.Errorf("repair operator contains a control character")
		}
	}
	return nil
}

// resolveRepairOperator resolves the identity persisted with a repair. An
// explicitly supplied value must validate — silently dropping what the
// operator typed would corrupt the audit trail's credibility — while the
// zero-config path derives user@host best-effort and degrades to the empty
// string rather than failing an emergency repair.
func resolveRepairOperator(explicit string) (string, error) {
	if explicit != "" {
		if err := validateRepairOperator(explicit); err != nil {
			return "", fmt.Errorf("db: repair operator: %w", err)
		}
		return explicit, nil
	}
	derived := ""
	if current, err := user.Current(); err == nil {
		derived = current.Username
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		if derived != "" {
			derived += "@" + host
		} else {
			derived = host
		}
	}
	if validateRepairOperator(derived) != nil {
		return "", nil
	}
	return derived, nil
}

// newLedgerRepairRecord builds the history row for one ledger repair action.
// now is the same timestamp stamped on the RepairReport.
func (e migrationEngine) newLedgerRepairRecord(opts RepairOptions, file Migration, ledgerChecksum, operator string, now time.Time) RepairRecord {
	kind := e.seq.kind
	if kind == "" {
		kind = repairHistoryKindApp
	}
	return RepairRecord{
		Kind: kind, Ledger: e.seq.ledger, Dialect: e.seq.dialect,
		Action: string(opts.Action), Version: opts.Version, File: file.File,
		LedgerChecksum: ledgerChecksum, CurrentChecksum: file.Checksum,
		Reason: strings.TrimSpace(opts.Reason), Operator: operator,
		ChokVersion: currentChokVersion(), RepairedAt: now,
	}
}

package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"
)

var ownedSequenceKindRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

const (
	sequenceManifestKind     = "manifest"
	maxSequenceOwnerBytes    = 190
	maxSequenceVersionBytes  = 64
	chokAccountSequenceOwner = "github.com/zynthara/chok/v2/account"
	chokAuditSequenceOwner   = "github.com/zynthara/chok/v2/audit"
	chokAuthzSequenceOwner   = "github.com/zynthara/chok/v2/authz"
)

var reservedSequenceOwners = map[string]string{
	"account": chokAccountSequenceOwner,
	"audit":   chokAuditSequenceOwner,
	"authz":   chokAuthzSequenceOwner,
}

// Baseline describes the AutoMigrate-equivalent frontier of an owned
// component sequence. Fingerprints are canonical SchemaFingerprint results by
// dialect. Tables must contain the component's complete owned table set and
// are checked as a unit before any baseline rows are adopted.
type Baseline struct {
	EquivalentVersion int64
	Tables            []string
	Fingerprints      map[string]string
}

// Sequence is one framework-owned, dialect-resolved migration history.
// Construct it with OwnedSequence; its internals are intentionally opaque so
// callers cannot select arbitrary ledger identifiers.
type Sequence struct {
	kind             string
	ledger           string
	owner            string
	componentVersion string
	fsys             fs.FS
	baseline         Baseline
}

type sequenceOptions struct {
	owner            string
	componentVersion string
	ownerSet         bool
	versionSet       bool
}

// SequenceOption configures the stable identity and informational metadata of
// an owned migration sequence. Construct options with SequenceOwner and
// SequenceVersion.
type SequenceOption interface {
	applySequenceOption(*sequenceOptions) error
}

type sequenceOptionFunc func(*sequenceOptions) error

func (f sequenceOptionFunc) applySequenceOption(opts *sequenceOptions) error { return f(opts) }

// SequenceOwner declares the stable component identity that owns a migration
// sequence. Use the full import path of the package declaring the sequence.
// Every OwnedSequence must provide exactly one owner.
func SequenceOwner(owner string) SequenceOption {
	return sequenceOptionFunc(func(opts *sequenceOptions) error {
		if opts.ownerSet {
			return fmt.Errorf("db: migration sequence owner was declared more than once")
		}
		if err := validateSequenceOwner(owner); err != nil {
			return err
		}
		opts.owner = owner
		opts.ownerSet = true
		return nil
	})
}

// SequenceVersion declares an informational component version to record in
// the manifest. It does not participate in compatibility decisions.
func SequenceVersion(version string) SequenceOption {
	return sequenceOptionFunc(func(opts *sequenceOptions) error {
		if opts.versionSet {
			return fmt.Errorf("db: migration sequence version was declared more than once")
		}
		if err := validateSequenceVersion(version); err != nil {
			return err
		}
		opts.componentVersion = version
		opts.versionSet = true
		return nil
	})
}

// OwnedSequence constructs a framework-owned migration sequence. fsys must
// contain sqlite, mysql and postgres subdirectories with numbered SQL files.
// SequenceOwner is mandatory; SequenceVersion is optional and informational.
func OwnedSequence(kind string, fsys fs.FS, baseline Baseline, options ...SequenceOption) (Sequence, error) {
	if err := ValidateSequenceKind(kind); err != nil {
		return Sequence{}, err
	}
	var opts sequenceOptions
	for i, option := range options {
		if option == nil {
			return Sequence{}, fmt.Errorf("db: owned migration sequence %q option %d is nil", kind, i)
		}
		if err := option.applySequenceOption(&opts); err != nil {
			return Sequence{}, fmt.Errorf("db: owned migration sequence %q: %w", kind, err)
		}
	}
	if !opts.ownerSet {
		return Sequence{}, fmt.Errorf("db: owned migration sequence %q requires db.SequenceOwner", kind)
	}
	if expected, reserved := reservedSequenceOwners[kind]; reserved && opts.owner != expected {
		return Sequence{}, fmt.Errorf("db: owned migration kind %q is reserved for owner %q", kind, expected)
	}
	if fsys == nil {
		return Sequence{}, fmt.Errorf("db: owned migration sequence %q requires a migration filesystem", kind)
	}
	if baseline.EquivalentVersion < 0 {
		return Sequence{}, fmt.Errorf("db: owned migration sequence %q has negative equivalent version", kind)
	}
	seen := make(map[string]struct{}, len(baseline.Tables))
	for _, table := range baseline.Tables {
		if !ownedSequenceKindRE.MatchString(table) {
			return Sequence{}, fmt.Errorf("db: owned migration sequence %q has invalid table %q", kind, table)
		}
		if _, duplicate := seen[table]; duplicate {
			return Sequence{}, fmt.Errorf("db: owned migration sequence %q repeats table %q", kind, table)
		}
		seen[table] = struct{}{}
	}
	if baseline.EquivalentVersion > 0 {
		if len(baseline.Tables) == 0 {
			return Sequence{}, fmt.Errorf("db: owned migration sequence %q baseline requires owned tables", kind)
		}
		for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
			if strings.TrimSpace(baseline.Fingerprints[dialect]) == "" {
				return Sequence{}, fmt.Errorf("db: owned migration sequence %q baseline requires a %s fingerprint", kind, dialect)
			}
		}
	}
	var canonical []Migration
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		dir, err := fs.Sub(fsys, dialect)
		if err != nil {
			return Sequence{}, fmt.Errorf("db: owned migration sequence %q: select %s directory: %w", kind, dialect, err)
		}
		files, err := LoadMigrations(dir)
		if err != nil {
			return Sequence{}, fmt.Errorf("db: owned migration sequence %q %s: %w", kind, dialect, err)
		}
		if canonical == nil {
			canonical = files
			continue
		}
		if err := compareMigrationIdentities(canonical, files); err != nil {
			return Sequence{}, fmt.Errorf("db: owned migration sequence %q dialect set mismatch (%s): %w", kind, dialect, err)
		}
	}
	return Sequence{
		kind: kind, ledger: ledgerForSequenceKind(kind), owner: opts.owner,
		componentVersion: opts.componentVersion,
		fsys:             fsys, baseline: cloneBaseline(baseline),
	}, nil
}

func validateSequenceOwner(owner string) error {
	if owner == "" {
		return fmt.Errorf("migration sequence owner is empty")
	}
	if !utf8.ValidString(owner) {
		return fmt.Errorf("migration sequence owner is not valid UTF-8")
	}
	if len(owner) > maxSequenceOwnerBytes {
		return fmt.Errorf("migration sequence owner is %d bytes, maximum is %d", len(owner), maxSequenceOwnerBytes)
	}
	if strings.TrimSpace(owner) != owner {
		return fmt.Errorf("migration sequence owner must not contain leading or trailing whitespace")
	}
	for _, r := range owner {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("migration sequence owner contains whitespace or a control character")
		}
	}
	return nil
}

func validateSequenceVersion(version string) error {
	if !utf8.ValidString(version) {
		return fmt.Errorf("migration sequence version is not valid UTF-8")
	}
	if len(version) > maxSequenceVersionBytes {
		return fmt.Errorf("migration sequence version is %d bytes, maximum is %d", len(version), maxSequenceVersionBytes)
	}
	if strings.TrimSpace(version) != version {
		return fmt.Errorf("migration sequence version must not contain leading or trailing whitespace")
	}
	for _, r := range version {
		if unicode.IsControl(r) {
			return fmt.Errorf("migration sequence version contains a control character")
		}
	}
	return nil
}

// forbiddenSequenceKinds are names no sequence may ever use, mapped to the
// framework identity each is reserved for: the shared manifest and repair
// history tables live inside the derived-ledger namespace, and "app" is the
// application ledger's identity in repair history rows.
var forbiddenSequenceKinds = map[string]string{
	sequenceManifestKind: "the global migration manifest",
	"app":                "the application migration ledger",
	"repairs":            "the migration repair history table",
}

// ValidateSequenceKind reports whether kind is a legal owned-sequence kind:
// it must match the sequence-kind grammar and must not collide with a
// reserved framework identity (manifest, app, repairs). It is the single
// gate — OwnedSequence, claim repair and every kind-derived identifier
// resolve through it — so tooling must use it before deriving a ledger name
// from an externally observed kind string.
func ValidateSequenceKind(kind string) error {
	if !ownedSequenceKindRE.MatchString(kind) {
		return fmt.Errorf("db: owned migration kind %q must match %s", kind, ownedSequenceKindRE)
	}
	if identity, forbidden := forbiddenSequenceKinds[kind]; forbidden {
		return fmt.Errorf("db: owned migration kind %q is reserved for %s", kind, identity)
	}
	return nil
}

func ledgerForSequenceKind(kind string) string { return "schema_migrations_chok_" + kind }

func compareMigrationIdentities(want, got []Migration) error {
	if len(want) != len(got) {
		return fmt.Errorf("migration count %d, want %d", len(got), len(want))
	}
	for i := range want {
		if want[i].Version != got[i].Version || want[i].Name != got[i].Name {
			return fmt.Errorf("migration %d_%s, want %d_%s", got[i].Version, got[i].Name, want[i].Version, want[i].Name)
		}
	}
	return nil
}

func cloneBaseline(in Baseline) Baseline {
	out := Baseline{EquivalentVersion: in.EquivalentVersion}
	out.Tables = append([]string(nil), in.Tables...)
	if in.Fingerprints != nil {
		out.Fingerprints = make(map[string]string, len(in.Fingerprints))
		for dialect, fingerprint := range in.Fingerprints {
			out.Fingerprints[dialect] = fingerprint
		}
	}
	return out
}

// Kind reports the component identity of the sequence.
func (s Sequence) Kind() string { return s.kind }

// Ledger reports the derived framework-owned ledger table.
func (s Sequence) Ledger() string { return s.ledger }

// Owner reports the stable component import path that owns the sequence.
func (s Sequence) Owner() string { return s.owner }

// ComponentVersion reports the optional informational component version.
func (s Sequence) ComponentVersion() string { return s.componentVersion }

// OwnedTables returns a caller-safe copy of the sequence's baseline tables.
func (s Sequence) OwnedTables() []string {
	return append([]string(nil), s.baseline.Tables...)
}

func resolveOwnedSequence(h *DB, seq Sequence) (migrationEngine, error) {
	if h == nil || h.gdb == nil {
		return migrationEngine{}, fmt.Errorf("db: resolve migration sequence: nil database handle")
	}
	if seq.kind == "" || seq.ledger == "" || seq.owner == "" || seq.fsys == nil {
		return migrationEngine{}, fmt.Errorf("db: invalid zero migration sequence; construct it with db.OwnedSequence")
	}
	dialect := h.gdb.Dialector.Name()
	if dialect != "sqlite" && dialect != "mysql" && dialect != "postgres" {
		return migrationEngine{}, fmt.Errorf("db: migration sequence %s does not support dialect %q", seq.kind, dialect)
	}
	selected, err := fs.Sub(seq.fsys, dialect)
	if err != nil {
		return migrationEngine{}, fmt.Errorf("db: migration sequence %s: select %s directory: %w", seq.kind, dialect, err)
	}
	if _, err := fs.ReadDir(selected, "."); err != nil {
		return migrationEngine{}, fmt.Errorf("db: migration sequence %s: read %s directory: %w", seq.kind, dialect, err)
	}
	return migrationEngine{seq: migrationSequence{
		kind: seq.kind, ledger: seq.ledger, dialect: dialect, fsys: selected,
		baseline: &seq.baseline, owner: seq.owner, componentVersion: seq.componentVersion,
	}}, nil
}

// ApplySequence applies a framework-owned sequence under the same audited
// dirty/fence/repair state machine as application migrations.
func ApplySequence(ctx context.Context, h *DB, seq Sequence) (*ApplyReport, error) {
	e, err := resolveOwnedSequence(h, seq)
	if err != nil {
		return &ApplyReport{}, err
	}
	report, err := e.apply(ctx, h)
	return report, wrapOwnedSequenceError(e, err)
}

// SequenceStatus returns the read-only audit state of an owned sequence.
func SequenceStatus(ctx context.Context, h *DB, seq Sequence) (*MigrationStatus, error) {
	e, err := resolveOwnedSequence(h, seq)
	if err != nil {
		return nil, err
	}
	status, err := e.status(ctx, h)
	return status, wrapOwnedSequenceError(e, err)
}

// RepairSequence explicitly repairs one row of an owned sequence.
func RepairSequence(ctx context.Context, h *DB, seq Sequence, opts RepairOptions) (*RepairReport, error) {
	e, err := resolveOwnedSequence(h, seq)
	if err != nil {
		return nil, err
	}
	report, err := e.repair(ctx, h, opts)
	return report, wrapOwnedSequenceError(e, err)
}

func wrapOwnedSequenceError(e migrationEngine, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("db: sequence=%s ledger=%s dialect=%s: %w", e.seq.kind, e.seq.ledger, e.seq.dialect, err)
}

// SequencePresent reports whether the ledger or every owned table is present.
// It never creates or upgrades the ledger.
func SequencePresent(ctx context.Context, h *DB, seq Sequence) (bool, error) {
	e, err := resolveOwnedSequence(h, seq)
	if err != nil {
		return false, err
	}
	gdb := h.gdb.WithContext(ctx)
	if gdb.Migrator().HasTable(e.seq.ledger) {
		return true, nil
	}
	if e.seq.baseline == nil || len(e.seq.baseline.Tables) == 0 {
		return false, nil
	}
	for _, table := range e.seq.baseline.Tables {
		if !gdb.Migrator().HasTable(table) {
			return false, nil
		}
	}
	return true, nil
}

type schemaColumnSnapshot struct {
	Name          string `json:"name"`
	DatabaseType  string `json:"database_type"`
	ColumnType    string `json:"column_type,omitempty"`
	PrimaryKey    *bool  `json:"primary_key,omitempty"`
	AutoIncrement *bool  `json:"auto_increment,omitempty"`
	Length        *int64 `json:"length,omitempty"`
	Precision     *int64 `json:"precision,omitempty"`
	Scale         *int64 `json:"scale,omitempty"`
	Nullable      *bool  `json:"nullable,omitempty"`
	Unique        *bool  `json:"unique,omitempty"`
	Default       string `json:"default,omitempty"`
}

type schemaIndexSnapshot struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns,omitempty"`
	PrimaryKey *bool    `json:"primary_key,omitempty"`
	Unique     *bool    `json:"unique,omitempty"`
	Option     string   `json:"option,omitempty"`
	Definition string   `json:"definition,omitempty"`
}

type schemaTableSnapshot struct {
	Name        string                 `json:"name"`
	Columns     []schemaColumnSnapshot `json:"columns"`
	Indexes     []schemaIndexSnapshot  `json:"indexes,omitempty"`
	Definition  string                 `json:"definition,omitempty"`
	Constraints []string               `json:"constraints,omitempty"`
}

type schemaCatalogSnapshot struct {
	Dialect string                `json:"dialect"`
	Tables  []schemaTableSnapshot `json:"tables"`
}

// SchemaFingerprint returns a deterministic, dialect-aware catalog snapshot.
// The returned JSON is intended to be generated from a fresh AutoMigrate
// database and embedded as a Baseline fingerprint.
func SchemaFingerprint(ctx context.Context, h *DB, tables []string) (string, error) {
	if h == nil || h.gdb == nil {
		return "", fmt.Errorf("db: schema fingerprint: nil database handle")
	}
	gdb := h.gdb.WithContext(ctx)
	dialect := gdb.Dialector.Name()
	names := append([]string(nil), tables...)
	sort.Strings(names)
	catalog := schemaCatalogSnapshot{Dialect: dialect}
	for _, table := range names {
		if !gdb.Migrator().HasTable(table) {
			return "", fmt.Errorf("db: schema fingerprint: missing table %s", table)
		}
		tableSnapshot, err := snapshotTable(gdb, dialect, table)
		if err != nil {
			return "", err
		}
		catalog.Tables = append(catalog.Tables, tableSnapshot)
	}
	raw, err := json.Marshal(catalog)
	if err != nil {
		return "", fmt.Errorf("db: encode schema fingerprint: %w", err)
	}
	return string(raw), nil
}

func snapshotTable(gdb *gorm.DB, dialect, table string) (schemaTableSnapshot, error) {
	out := schemaTableSnapshot{Name: table}
	columns, err := gdb.Migrator().ColumnTypes(table)
	if err != nil {
		return out, fmt.Errorf("db: schema fingerprint: inspect %s columns: %w", table, err)
	}
	for _, column := range columns {
		item := schemaColumnSnapshot{Name: column.Name(), DatabaseType: strings.ToLower(column.DatabaseTypeName())}
		if value, ok := column.ColumnType(); ok {
			item.ColumnType = normalizeDDL(value)
		}
		item.PrimaryKey = optionalBool(column.PrimaryKey())
		item.AutoIncrement = optionalBool(column.AutoIncrement())
		if value, ok := column.Length(); ok {
			item.Length = &value
		}
		if precision, scale, ok := column.DecimalSize(); ok {
			item.Precision, item.Scale = &precision, &scale
		}
		item.Nullable = optionalBool(column.Nullable())
		item.Unique = optionalBool(column.Unique())
		if value, ok := column.DefaultValue(); ok {
			item.Default = normalizeDDL(value)
		}
		out.Columns = append(out.Columns, item)
	}
	sort.Slice(out.Columns, func(i, j int) bool { return out.Columns[i].Name < out.Columns[j].Name })

	indexes, err := gdb.Migrator().GetIndexes(table)
	if err != nil {
		return out, fmt.Errorf("db: schema fingerprint: inspect %s indexes: %w", table, err)
	}
	for _, index := range indexes {
		item := schemaIndexSnapshot{Name: index.Name(), Columns: append([]string(nil), index.Columns()...), Option: normalizeDDL(index.Option())}
		item.PrimaryKey = optionalBool(index.PrimaryKey())
		item.Unique = optionalBool(index.Unique())
		out.Indexes = append(out.Indexes, item)
	}
	if err := enrichTableSnapshot(gdb, dialect, table, &out); err != nil {
		return out, err
	}
	sort.Slice(out.Indexes, func(i, j int) bool { return out.Indexes[i].Name < out.Indexes[j].Name })
	sort.Strings(out.Constraints)
	return out, nil
}

func optionalBool(value bool, ok bool) *bool {
	if !ok {
		return nil
	}
	return &value
}

func enrichTableSnapshot(gdb *gorm.DB, dialect, table string, out *schemaTableSnapshot) error {
	switch dialect {
	case "sqlite":
		var rows []struct {
			Type string
			Name string
			SQL  string
		}
		if err := gdb.Raw("SELECT type, name, sql FROM sqlite_master WHERE tbl_name = ? AND type IN ('table','index') AND sql IS NOT NULL ORDER BY type, name", table).Scan(&rows).Error; err != nil {
			return fmt.Errorf("db: schema fingerprint: inspect sqlite %s DDL: %w", table, err)
		}
		for _, row := range rows {
			if row.Type == "table" {
				out.Definition = normalizeDDL(row.SQL)
				continue
			}
			for i := range out.Indexes {
				if out.Indexes[i].Name == row.Name {
					out.Indexes[i].Definition = normalizeDDL(row.SQL)
				}
			}
		}
	case "mysql":
		var name, ddl string
		if err := gdb.Raw("SHOW CREATE TABLE `"+table+"`").Row().Scan(&name, &ddl); err != nil {
			return fmt.Errorf("db: schema fingerprint: inspect mysql %s DDL: %w", table, err)
		}
		out.Definition = normalizeMySQLDDL(ddl)
	case "postgres":
		var schemaName string
		if err := gdb.Raw("SELECT current_schema()").Scan(&schemaName).Error; err != nil {
			return fmt.Errorf("db: schema fingerprint: inspect postgres current schema: %w", err)
		}
		var definitions []struct {
			Name       string
			Definition string
		}
		if err := gdb.Raw("SELECT indexname AS name, indexdef AS definition FROM pg_indexes WHERE schemaname = current_schema() AND tablename = ? ORDER BY indexname", table).Scan(&definitions).Error; err != nil {
			return fmt.Errorf("db: schema fingerprint: inspect postgres %s indexes: %w", table, err)
		}
		byName := make(map[string]string, len(definitions))
		for _, definition := range definitions {
			ddl := strings.ReplaceAll(definition.Definition, schemaName+".", "")
			ddl = strings.ReplaceAll(ddl, `"`+schemaName+`".`, "")
			byName[definition.Name] = normalizeDDL(ddl)
		}
		for i := range out.Indexes {
			// gorm's postgres GetIndexes currently reuses an internal column
			// slice across calls, which can duplicate Columns on repeated
			// introspection. indexdef is the authoritative, stable shape and
			// also preserves predicates/expressions that Columns cannot.
			out.Indexes[i].Columns = nil
			out.Indexes[i].Definition = byName[out.Indexes[i].Name]
		}
		if err := gdb.Raw(`SELECT pg_get_constraintdef(c.oid, true) AS definition
			FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = current_schema() AND t.relname = ? ORDER BY c.conname`, table).
			Scan(&out.Constraints).Error; err != nil {
			return fmt.Errorf("db: schema fingerprint: inspect postgres %s constraints: %w", table, err)
		}
		for i := range out.Constraints {
			out.Constraints[i] = normalizeDDL(out.Constraints[i])
		}
	}
	return nil
}

var mysqlAutoIncrementRE = regexp.MustCompile(`(?i)\sAUTO_INCREMENT=\d+`)

func normalizeMySQLDDL(value string) string {
	return normalizeDDL(mysqlAutoIncrementRE.ReplaceAllString(value, ""))
}

func normalizeDDL(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func fingerprintDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func schemaFingerprintDifference(expected, actual string) string {
	var want, got schemaCatalogSnapshot
	if json.Unmarshal([]byte(expected), &want) != nil || json.Unmarshal([]byte(actual), &got) != nil {
		return fmt.Sprintf("expected_sha256=%s actual_sha256=%s", fingerprintDigest(expected), fingerprintDigest(actual))
	}
	if want.Dialect != got.Dialect {
		return fmt.Sprintf("dialect expected=%s actual=%s", want.Dialect, got.Dialect)
	}
	if len(want.Tables) != len(got.Tables) {
		return fmt.Sprintf("table_count expected=%d actual=%d expected_sha256=%s actual_sha256=%s",
			len(want.Tables), len(got.Tables), fingerprintDigest(expected), fingerprintDigest(actual))
	}
	for i := range want.Tables {
		wantRaw, _ := json.Marshal(want.Tables[i])
		gotRaw, _ := json.Marshal(got.Tables[i])
		if string(wantRaw) != string(gotRaw) {
			return fmt.Sprintf("table=%s expected=%s actual=%s", want.Tables[i].Name, wantRaw, gotRaw)
		}
	}
	return fmt.Sprintf("expected_sha256=%s actual_sha256=%s", fingerprintDigest(expected), fingerprintDigest(actual))
}

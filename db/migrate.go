package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
)

// MigrationStatus is one versioned migration of one owner.
type MigrationStatus struct {
	Owner   string
	Version string
	Name    string
	// State is "applied", "pending", "partial" (MySQL: the statements
	// before the failing one are applied), "drifted" (applied, but the
	// file changed since) or "missing" (applied, but the file is gone).
	State     string
	AppliedAt time.Time
	// Statements counts the statements that have run.
	Statements int
}

var (
	// ErrNotMigrated is returned with migrate: off when versioned files
	// are not all applied, so a process never runs on a stale schema.
	ErrNotMigrated = errors.New("db: migrations are not applied")
	// ErrChecksumDrift is returned when an applied migration's file has
	// changed: migrations only move forward, so fix it with a new one.
	ErrChecksumDrift = errors.New("db: an applied migration has changed")
)

const ledgerTable = "chok_migrations"

// fileName is <14-digit version>_<name>.sql, such as
// 20261024120000_create_posts.sql.
var fileName = regexp.MustCompile(`^(\d{14})_([A-Za-z0-9_-]+)\.sql$`)

type migration struct {
	owner, version, name, file string
	stmts                      []string
	sum                        string
}

type ledgerRow struct {
	Owner, Version, Name, Checksum string
	AppliedAt                      *time.Time
	StatementIndex                 int
}

// loadMigrations reads an owner's SQL files: from the dialect's
// subdirectory (sqlite/, mysql/, postgres/) when fsys has one, else from
// its root. Other files are ignored; a .sql file with a malformed name is
// an error rather than a migration that silently never runs.
func loadMigrations(fsys fs.FS, owner, dialect string) ([]migration, error) {
	if fi, err := fs.Stat(fsys, dialect); err == nil && fi.IsDir() {
		sub, err := fs.Sub(fsys, dialect)
		if err != nil {
			return nil, err
		}
		fsys = sub
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("db: read %s migrations: %w", owner, err)
	}
	var out []migration
	seen := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("db: %s migration %s: name it <14-digit version>_<name>.sql, such as 20261024120000_create_posts.sql", owner, e.Name())
		}
		if prev, dup := seen[m[1]]; dup {
			return nil, fmt.Errorf("db: %s migrations %s and %s share version %s", owner, prev, e.Name(), m[1])
		}
		seen[m[1]] = e.Name()
		raw, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		text := strings.ReplaceAll(string(raw), "\r\n", "\n")
		stmts := splitSQLStatements(text)
		if len(stmts) == 0 {
			return nil, fmt.Errorf("db: %s migration %s has no statements", owner, e.Name())
		}
		out = append(out, migration{owner: owner, version: m[1], name: m[2], file: e.Name(), stmts: stmts, sum: checksum(text)})
	}
	slices.SortFunc(out, func(a, b migration) int { return strings.Compare(a.version, b.version) })
	return out, nil
}

func checksum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// prefixSum fingerprints the first n statements: the checksum a partial
// MySQL migration carries, so a rerun can tell those statements are the
// ones that ran.
func prefixSum(stmts []string, n int) string {
	return checksum(strings.Join(stmts[:n], ";\n"))
}

func ensureLedger(ctx context.Context, gdb *gorm.DB) error {
	var ddl string
	switch gdb.Name() {
	case "mysql":
		ddl = "CREATE TABLE IF NOT EXISTS " + ledgerTable + ` (
			owner VARCHAR(64) NOT NULL, version VARCHAR(32) NOT NULL, name VARCHAR(255) NOT NULL,
			checksum CHAR(64) NOT NULL, applied_at DATETIME(6) NULL, statement_index INT NOT NULL DEFAULT 0,
			PRIMARY KEY (owner, version)) DEFAULT CHARSET=utf8mb4`
	case "postgres":
		ddl = "CREATE TABLE IF NOT EXISTS " + ledgerTable + ` (
			owner VARCHAR(64) NOT NULL, version VARCHAR(32) NOT NULL, name VARCHAR(255) NOT NULL,
			checksum CHAR(64) NOT NULL, applied_at TIMESTAMPTZ NULL, statement_index INT NOT NULL DEFAULT 0,
			PRIMARY KEY (owner, version))`
	default:
		ddl = "CREATE TABLE IF NOT EXISTS " + ledgerTable + ` (
			owner TEXT NOT NULL, version TEXT NOT NULL, name TEXT NOT NULL,
			checksum TEXT NOT NULL, applied_at DATETIME NULL, statement_index INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (owner, version))`
	}
	return gdb.WithContext(ctx).Exec(ddl).Error
}

func readLedger(ctx context.Context, gdb *gorm.DB, owner string) (map[string]ledgerRow, error) {
	gdb = gdb.WithContext(ctx)
	out := map[string]ledgerRow{}
	if !gdb.Migrator().HasTable(ledgerTable) {
		return out, nil
	}
	var rows []ledgerRow
	if err := gdb.Raw("SELECT owner, version, name, checksum, applied_at, statement_index FROM "+ledgerTable+" WHERE owner = ?", owner).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("db: read %s: %w", ledgerTable, err)
	}
	for _, r := range rows {
		out[r.Version] = r
	}
	return out, nil
}

// status compares an owner's files with its ledger rows.
func status(files []migration, rows map[string]ledgerRow) []MigrationStatus {
	var out []MigrationStatus
	have := map[string]bool{}
	for _, f := range files {
		have[f.version] = true
		st := MigrationStatus{Owner: f.owner, Version: f.version, Name: f.name, State: "pending"}
		if r, ok := rows[f.version]; ok {
			st.Statements = r.StatementIndex
			switch {
			case r.AppliedAt == nil:
				st.State = "partial"
			case r.Checksum != f.sum:
				st.State, st.AppliedAt = "drifted", r.AppliedAt.UTC()
			default:
				st.State, st.AppliedAt = "applied", r.AppliedAt.UTC()
			}
		}
		out = append(out, st)
	}
	for v, r := range rows {
		if !have[v] {
			st := MigrationStatus{Owner: r.Owner, Version: v, Name: r.Name, State: "missing", Statements: r.StatementIndex}
			if r.AppliedAt != nil {
				st.AppliedAt = r.AppliedAt.UTC()
			}
			out = append(out, st)
		}
	}
	slices.SortFunc(out, func(a, b MigrationStatus) int { return strings.Compare(a.Version, b.Version) })
	return out
}

// apply runs an owner's pending migrations under the migration lock.
func (d *DB) apply(ctx context.Context, owner string, files []migration) error {
	gdb := d.gdb.WithContext(ctx)
	release, err := lockMigrations(ctx, gdb)
	if err != nil {
		return err
	}
	defer release()
	if err := ensureLedger(ctx, gdb); err != nil {
		return fmt.Errorf("db: create %s: %w", ledgerTable, err)
	}
	rows, err := readLedger(ctx, gdb, owner)
	if err != nil {
		return err
	}
	var newest string
	var drifted, missing []string
	for _, st := range status(files, rows) {
		switch st.State {
		case "drifted":
			drifted = append(drifted, st.Version+"_"+st.Name)
		case "missing":
			missing = append(missing, st.Version+"_"+st.Name)
		}
		if st.State == "applied" || st.State == "drifted" {
			newest = max(newest, st.Version)
		}
	}
	if len(drifted) > 0 {
		return fmt.Errorf("%w: %s %s (restore the applied file and add a new migration for the change)", ErrChecksumDrift, owner, strings.Join(drifted, ", "))
	}
	if len(missing) > 0 {
		return fmt.Errorf("db: %s migrations %s are applied but their files are gone; migrations only move forward", owner, strings.Join(missing, ", "))
	}
	for _, f := range files {
		row, has := rows[f.version]
		if has && row.AppliedAt != nil {
			continue
		}
		if !has && f.version < newest {
			return fmt.Errorf("db: %s migration %s is older than the applied %s; give it a newer version", owner, f.file, newest)
		}
		if gdb.Name() == "mysql" {
			err = d.applyByStatement(ctx, gdb, f, row, has)
		} else {
			err = applyInTx(ctx, gdb, f)
		}
		if err != nil {
			return err
		}
		d.Logger().InfoContext(ctx, "db: migration applied", "owner", owner, "migration", f.file, "statements", len(f.stmts))
	}
	return nil
}

// applyInTx runs a whole file and its ledger row in one transaction, the
// way SQLite and Postgres can.
func applyInTx(ctx context.Context, gdb *gorm.DB, f migration) error {
	return runTransaction(ctx, gdb, func(tx *gorm.DB) error {
		// SQLite has no cross-process lock: the writer's BEGIN IMMEDIATE
		// is it, so look again now that this transaction holds it.
		var n int64
		if err := tx.Raw("SELECT COUNT(*) FROM "+ledgerTable+" WHERE owner = ? AND version = ? AND applied_at IS NOT NULL",
			f.owner, f.version).Scan(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		pool := tx.Statement.ConnPool
		for i, stmt := range f.stmts {
			if _, err := pool.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("db: %s migration %s, statement %d of %d: %w (nothing of it is applied)", f.owner, f.file, i+1, len(f.stmts), err)
			}
		}
		return tx.Exec("INSERT INTO "+ledgerTable+" (owner, version, name, checksum, applied_at, statement_index) VALUES (?, ?, ?, ?, ?, ?)",
			f.owner, f.version, f.name, f.sum, time.Now().UTC(), len(f.stmts)).Error
	})
}

// applyByStatement runs a MySQL migration one statement at a time and
// records each one: MySQL commits DDL implicitly, so a file that fails
// halfway leaves its first statements applied, and the next run resumes
// after them instead of failing on "table already exists".
func (d *DB) applyByStatement(ctx context.Context, gdb *gorm.DB, f migration, row ledgerRow, has bool) error {
	start := 0
	if has {
		if row.StatementIndex > len(f.stmts) || prefixSum(f.stmts, row.StatementIndex) != row.Checksum {
			return fmt.Errorf("db: %s migration %s stopped after statement %d last time, and those statements have changed since; restore them, or repair the schema by hand and mark the migration applied",
				f.owner, f.file, row.StatementIndex)
		}
		start = row.StatementIndex
		d.Logger().InfoContext(ctx, "db: resuming a partly applied migration", "owner", f.owner, "migration", f.file, "from_statement", start+1)
	}
	pool := gdb.Statement.ConnPool
	for i := start; i < len(f.stmts); i++ {
		if _, err := pool.ExecContext(ctx, f.stmts[i]); err != nil {
			return fmt.Errorf("db: %s migration %s, statement %d of %d: %w (statements before it are applied; fix it and rerun to resume there)",
				f.owner, f.file, i+1, len(f.stmts), err)
		}
		var err error
		if !has {
			err = gdb.Exec("INSERT INTO "+ledgerTable+" (owner, version, name, checksum, applied_at, statement_index) VALUES (?, ?, ?, ?, NULL, ?)",
				f.owner, f.version, f.name, prefixSum(f.stmts, i+1), i+1).Error
			has = true
		} else {
			err = gdb.Exec("UPDATE "+ledgerTable+" SET checksum = ?, statement_index = ?, name = ? WHERE owner = ? AND version = ?",
				prefixSum(f.stmts, i+1), i+1, f.name, f.owner, f.version).Error
		}
		if err != nil {
			return fmt.Errorf("db: record %s migration %s after statement %d: %w", f.owner, f.file, i+1, err)
		}
	}
	return gdb.Exec("UPDATE "+ledgerTable+" SET checksum = ?, applied_at = ?, statement_index = ? WHERE owner = ? AND version = ?",
		f.sum, time.Now().UTC(), len(f.stmts), f.owner, f.version).Error
}

// markApplied records f as applied without running it.
func markApplied(ctx context.Context, gdb *gorm.DB, f migration) error {
	release, err := lockMigrations(ctx, gdb)
	if err != nil {
		return err
	}
	defer release()
	if err := ensureLedger(ctx, gdb); err != nil {
		return err
	}
	gdb = gdb.WithContext(ctx)
	now := time.Now().UTC()
	res := gdb.Exec("UPDATE "+ledgerTable+" SET checksum = ?, applied_at = ?, statement_index = ?, name = ? WHERE owner = ? AND version = ?",
		f.sum, now, len(f.stmts), f.name, f.owner, f.version)
	if res.Error != nil || res.RowsAffected > 0 {
		return res.Error
	}
	return gdb.Exec("INSERT INTO "+ledgerTable+" (owner, version, name, checksum, applied_at, statement_index) VALUES (?, ?, ?, ?, ?, ?)",
		f.owner, f.version, f.name, f.sum, now, len(f.stmts)).Error
}

// lockMigrations takes the cross-process migration lock: a Postgres
// advisory lock or a MySQL GET_LOCK, each held by a pinned connection.
// SQLite needs none beyond the writer's immediate transaction.
func lockMigrations(ctx context.Context, gdb *gorm.DB) (release func(), err error) {
	dialect := gdb.Name()
	if dialect != "postgres" && dialect != "mysql" {
		return func() {}, nil
	}
	pool, err := gdb.DB()
	if err != nil {
		return nil, err
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: migration lock: %w", err)
	}
	unlock := func(ctx context.Context) error { return nil }
	switch dialect {
	case "postgres":
		if _, err = conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", pgLockKey); err == nil {
			unlock = func(ctx context.Context) error {
				_, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", pgLockKey)
				return err
			}
		}
	case "mysql":
		timeout := 60
		if dl, ok := ctx.Deadline(); ok {
			timeout = max(1, int(time.Until(dl).Seconds()))
		}
		var got sql.NullInt64
		if err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", mysqlLockName, timeout).Scan(&got); err == nil && got.Int64 != 1 {
			err = fmt.Errorf("db: another process held the migration lock for %ds", timeout)
		}
		if err == nil {
			unlock = func(ctx context.Context) error {
				_, err := conn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", mysqlLockName)
				return err
			}
		}
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("db: migration lock: %w", err)
	}
	return func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if unlock(cctx) != nil {
			// The session may still hold the lock: drop it from the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, nil
}

const mysqlLockName = "chok:migrations"

// pgLockKey is FNV-1a of "chok:migrations" as a signed 64-bit key.
var pgLockKey = func() int64 {
	h := uint64(14695981039346656037)
	for _, c := range []byte("chok:migrations") {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return int64(h)
}()

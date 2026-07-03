package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"

	"gorm.io/gorm"
)

// Versioned migrations (SPEC §5.3): embedded, forward-only,
// sequence-numbered *.sql files applied once each and recorded in a
// schema_migrations ledger under a cross-process migration lock.
// There are no down migrations, no checksums and no dirty flag in
// v2.0 — the audit story is "the ledger says exactly which forward
// steps ran"; PG/SQLite wrap each file in a transaction, MySQL's
// auto-committing DDL is documented as partially-applied-on-failure.

// ledgerTable is the migration ledger. It is also a member of the
// framework-table whitelist below.
const ledgerTable = "schema_migrations"

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
	Version int64
	Name    string
	File    string
	SQL     string
}

// AppliedMigration is one ledger row.
type AppliedMigration struct {
	Version   int64
	Name      string
	AppliedAt time.Time
}

// MigrationStatus is the read-only view chok migrate status renders.
type MigrationStatus struct {
	Applied []AppliedMigration
	Pending []Migration
	// FrameworkTables echoes the AutoMigrate-exempt whitelist so every
	// status surface presents it honestly next to the ledger.
	FrameworkTables []string
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
		out = append(out, Migration{Version: ver, Name: m[2], File: name, SQL: string(raw)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// ensureLedger creates the schema_migrations table. The DDL is the
// portable intersection of sqlite/mysql/postgres.
func ensureLedger(gdb *gorm.DB) error {
	return gdb.Exec(
		"CREATE TABLE IF NOT EXISTS " + ledgerTable + " (" +
			"version BIGINT PRIMARY KEY, " +
			"name VARCHAR(255) NOT NULL, " +
			"applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)",
	).Error
}

// appliedMigrations reads the ledger sorted by version.
func appliedMigrations(gdb *gorm.DB) ([]AppliedMigration, error) {
	rows, err := gdb.Raw(
		"SELECT version, name, applied_at FROM " + ledgerTable + " ORDER BY version",
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("db: read %s: %w", ledgerTable, err)
	}
	defer rows.Close()
	var out []AppliedMigration
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.AppliedAt); err != nil {
			return nil, fmt.Errorf("db: scan %s: %w", ledgerTable, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// splitPending returns the files still to apply, erroring on drift:
// a ledger row whose file vanished means the binary and the database
// disagree about history — refuse to guess.
func splitPending(files []Migration, applied []AppliedMigration) ([]Migration, error) {
	byVersion := make(map[int64]Migration, len(files))
	for _, f := range files {
		byVersion[f.Version] = f
	}
	appliedSet := make(map[int64]bool, len(applied))
	for _, a := range applied {
		appliedSet[a.Version] = true
		if _, ok := byVersion[a.Version]; !ok {
			return nil, fmt.Errorf(
				"db: ledger has version %d (%s) but no matching migration file — refusing to continue on drifted history",
				a.Version, a.Name)
		}
	}
	var pending []Migration
	for _, f := range files {
		if !appliedSet[f.Version] {
			pending = append(pending, f)
		}
	}
	return pending, nil
}

// ApplyMigrations brings the database up to the migration set in fsys:
// ensure ledger → acquire the dialect lock → apply each pending file
// (statement by statement, one transaction per file, ledger row in the
// same transaction) → release. Returns the migrations applied by this
// call. Concurrent callers across processes serialize on the lock;
// the loser sees the winner's ledger rows and applies nothing.
func ApplyMigrations(ctx context.Context, h *DB, fsys fs.FS) ([]Migration, error) {
	files, err := LoadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	gdb := h.gdb.WithContext(ctx)
	if err := ensureLedger(gdb); err != nil {
		return nil, fmt.Errorf("db: ensure %s: %w", ledgerTable, err)
	}

	release, err := acquireMigrationLock(ctx, gdb)
	if err != nil {
		return nil, err
	}
	defer release()

	applied, err := appliedMigrations(gdb)
	if err != nil {
		return nil, err
	}
	pending, err := splitPending(files, applied)
	if err != nil {
		return nil, err
	}

	var done []Migration
	for _, m := range pending {
		if err := applyOne(gdb, m); err != nil {
			return done, err
		}
		done = append(done, m)
	}
	return done, nil
}

// applyOne runs a single migration file inside one transaction —
// transactional DDL on Postgres/SQLite; on MySQL each DDL statement
// auto-commits, so a mid-file failure leaves earlier statements
// applied and NO ledger row (rerun re-attempts the whole file;
// documented v2.0 behaviour).
func applyOne(gdb *gorm.DB, m Migration) error {
	stmts := splitSQLStatements(m.SQL)
	if len(stmts) == 0 {
		return fmt.Errorf("db: migration %s contains no statements", m.File)
	}
	err := gdb.Transaction(func(tx *gorm.DB) error {
		for i, stmt := range stmts {
			if err := tx.Exec(stmt).Error; err != nil {
				return fmt.Errorf("statement %d: %w", i+1, err)
			}
		}
		return tx.Exec(
			"INSERT INTO "+ledgerTable+" (version, name) VALUES (?, ?)",
			m.Version, m.Name,
		).Error
	})
	if err != nil {
		return fmt.Errorf("db: apply migration %s: %w", m.File, err)
	}
	return nil
}

// MigrationsStatus reports applied/pending without taking the lock —
// a read-only status must work even while an up is in flight. A
// missing ledger table reads as "nothing applied".
func MigrationsStatus(ctx context.Context, h *DB, fsys fs.FS) (*MigrationStatus, error) {
	files, err := LoadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	gdb := h.gdb.WithContext(ctx)

	st := &MigrationStatus{FrameworkTables: FrameworkTables()}
	if gdb.Migrator().HasTable(ledgerTable) {
		applied, err := appliedMigrations(gdb)
		if err != nil {
			return nil, err
		}
		st.Applied = applied
	}
	pending, err := splitPending(files, st.Applied)
	if err != nil {
		return nil, err
	}
	st.Pending = pending
	return st, nil
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

// acquireMigrationLock takes the cross-process migration lock for the
// handle's dialect and returns its release func:
//
//   - postgres: pg_advisory_lock on a dedicated *sql.Conn (advisory
//     locks are per-session — the conn is pinned until release)
//   - mysql: GET_LOCK on a dedicated conn, timeout from ctx deadline
//   - sqlite (and anything else): no-op — a file database has a single
//     writer; the per-migration transaction (BEGIN → busy timeout)
//     already serializes competing processes
func acquireMigrationLock(ctx context.Context, gdb *gorm.DB) (release func(), err error) {
	switch gdb.Dialector.Name() {
	case "postgres":
		return acquireConnLock(ctx, gdb,
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
	case "mysql":
		timeout := int64(60)
		if dl, ok := ctx.Deadline(); ok {
			if remaining := int64(time.Until(dl).Seconds()); remaining > 0 {
				timeout = remaining
			} else {
				timeout = 1
			}
		}
		return acquireConnLock(ctx, gdb,
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
	default:
		return func() {}, nil
	}
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

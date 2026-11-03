package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// gorm's Scan runs its query under gorm's recorder logger, then hands the
// SQL the recorder kept to the handle's logger: the handle's ParamsFilter
// is never asked about that query. The recorder asks only the package
// variable gormlogger.RecorderParamsFilter, whose default keeps the
// values, so it is set here. It is process-wide: in a process that
// imports chok/db, a query read with Scan through any gorm handle, chok's
// or another, is logged with its placeholders.
func init() {
	gormlogger.RecorderParamsFilter = keepPlaceholders
}

// keepPlaceholders drops a statement's bound values, so gorm logs its SQL
// with the placeholders where the values would go.
func keepPlaceholders(_ context.Context, sql string, _ ...any) (string, []any) {
	return sql, nil
}

// gormLogger bridges gorm to the handle's slog logger: failed statements
// at error (a missing record is not a failure), slow ones at warn, the
// rest at debug. SQL is logged with placeholders, never with values, a
// query read with Scan included (see init). A statement's error is logged
// without the values a driver's message can quote, as its driver, its
// code and the names it carries (see errorText).
type gormLogger struct {
	d     *DB
	slow  time.Duration
	level gormlogger.LogLevel
}

func (g *gormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	cp := *g
	cp.level = level
	return &cp
}

// ParamsFilter keeps bound values out of the logged SQL.
func (g *gormLogger) ParamsFilter(ctx context.Context, sql string, params ...any) (string, []any) {
	return keepPlaceholders(ctx, sql, params...)
}

func (g *gormLogger) Info(ctx context.Context, msg string, data ...any) {
	g.d.Logger().InfoContext(ctx, "db: "+fmt.Sprintf(msg, data...))
}

func (g *gormLogger) Warn(ctx context.Context, msg string, data ...any) {
	g.d.Logger().WarnContext(ctx, "db: "+fmt.Sprintf(msg, data...))
}

func (g *gormLogger) Error(ctx context.Context, msg string, data ...any) {
	g.d.Logger().ErrorContext(ctx, "db: "+fmt.Sprintf(msg, data...))
}

func (g *gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if g.level == gormlogger.Silent {
		return
	}
	l := g.d.Logger()
	elapsed := time.Since(begin)
	switch {
	case err != nil && !expected(err):
		sql, rows := fc()
		l.ErrorContext(ctx, "db: query failed", "error", errorText(err), "elapsed", elapsed, "rows", rows, "sql", sql)
	case err != nil:
		if l.Enabled(ctx, slog.LevelDebug) {
			sql, rows := fc()
			l.DebugContext(ctx, "db: query refused", "error", errorText(err), "elapsed", elapsed, "rows", rows, "sql", sql)
		}
	case g.slow > 0 && elapsed >= g.slow:
		sql, rows := fc()
		l.WarnContext(ctx, "db: slow query", "elapsed", elapsed, "threshold", g.slow, "rows", rows, "sql", sql)
	case l.Enabled(ctx, slog.LevelDebug):
		sql, rows := fc()
		l.DebugContext(ctx, "db: query", "elapsed", elapsed, "rows", rows, "sql", sql)
	}
}

// expected reports errors that are outcomes rather than faults: a missing
// record, a canceled request, a constraint the data violated (store turns
// those into 404, 409 and 400 answers).
func expected(err error) bool {
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"duplicate", "unique constraint", "foreign key", "constraint failed", "violates check constraint", "check constraint"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// The codes errorText reads.
const (
	mysqlDuplicateEntry     = 1062 // ER_DUP_ENTRY
	sqliteError             = 1    // SQLITE_ERROR, the low byte of its extended codes
	sqliteConstraintTrigger = 1811 // SQLITE_CONSTRAINT_TRIGGER: a trigger's RAISE
)

// mysqlDuplicateKey finds the key in MySQL's Duplicate entry 'x' for key
// 'k'. The key ends the message; the value comes first and may itself
// read for key 'y', so the match is anchored at the end.
var mysqlDuplicateKey = regexp.MustCompile(`for key '([^']+)'$`)

// scanValue and scanBool find the value database/sql quotes when it
// cannot scan it into the destination. converting driver.Value type
// string ("x") to a int quotes it with %q, whose escapes the match steps
// over. couldn't convert "x" into type bool, from database/sql/driver,
// quotes it with %q, %d or %v (%T), and the match runs to the last into
// type bool, so a value that reads like one goes as well.
var (
	scanValue = regexp.MustCompile(`(converting driver\.Value type \S+ )\("(?:[^"\\]|\\.)*"\) `)
	scanBool  = regexp.MustCompile(`(couldn't convert ).* (into type bool)`)
)

// errorText is what the log says of a statement's error. The drivers'
// messages quote the values a statement bound: MySQL's Duplicate entry
// 'x' for key 'k', Incorrect integer value 'x' for column 'c' and
// Truncated incorrect DOUBLE value 'x', Postgres's invalid input syntax
// for type integer "x" and value "x" is out of range. The SQL beside the
// error is logged with placeholders, and the message would bring back
// the values it leaves out. So a Postgres or MySQL error is logged as its
// driver, its code and the names it carries: Postgres's constraint, table
// and column fields, the key a MySQL duplicate names. With the statement,
// they locate the fault:
//
//	postgres 23505 constraint uk_user_email table users
//	mysql 1062 (23000) key users.uk_user_email
//	mysql 1366 (HY000)
//
// SQLite's messages name objects (UNIQUE constraint failed: users.email)
// and are kept, except two kinds logged as their code alone (sqlite 1,
// sqlite 1811). SQLITE_ERROR is the code of SQL that does not compile,
// whose message quotes the SQL, and of a value a function or a virtual
// table refuses, whose message may quote it: a JSON path, a word of an
// FTS5 query. A trigger's RAISE text is the trigger's and may quote the
// row. database/sql's own error for a value it cannot scan into the
// destination quotes the value as well; it is cut out and the rest of
// the message kept. Any other error, a network fault, a cancellation,
// gorm's or chok's own, is logged as it is. The caller gets the error
// unchanged, and expected reads all of it.
func errorText(err error) string {
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		words := []string{"postgres", pg.Code}
		for _, f := range [...]struct{ key, name string }{
			{"constraint", pg.ConstraintName}, {"table", pg.TableName}, {"column", pg.ColumnName},
		} {
			if f.name != "" {
				words = append(words, f.key, f.name)
			}
		}
		return strings.Join(words, " ")
	}
	if my, ok := errors.AsType[*gomysql.MySQLError](err); ok {
		s := fmt.Sprintf("mysql %d", my.Number)
		if my.SQLState != [5]byte{} {
			s += " (" + string(my.SQLState[:]) + ")"
		}
		if my.Number == mysqlDuplicateEntry {
			if m := mysqlDuplicateKey.FindStringSubmatch(my.Message); m != nil {
				s += " key " + m[1]
			}
		}
		return s
	}
	// The pure-Go SQLite driver's error carries its extended result code.
	if lite, ok := errors.AsType[interface {
		error
		Code() int
	}](err); ok {
		if code := lite.Code(); code&0xff == sqliteError || code == sqliteConstraintTrigger {
			return fmt.Sprintf("sqlite %d", code)
		}
	}
	s := scanValue.ReplaceAllString(err.Error(), "$1")
	return scanBool.ReplaceAllString(s, "$1$2")
}

package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/zynthara/chok/internal/dberr"
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
// code and the names it carries (see dberr.Text).
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
		l.ErrorContext(ctx, "db: query failed", "error", dberr.Text(err), "elapsed", elapsed, "rows", rows, "sql", sql)
	case err != nil:
		if l.Enabled(ctx, slog.LevelDebug) {
			sql, rows := fc()
			l.DebugContext(ctx, "db: query refused", "error", dberr.Text(err), "elapsed", elapsed, "rows", rows, "sql", sql)
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
// those into 404, 409 and 400 answers). A violation is told by the
// driver's error type and code, never by the message, which may quote a
// value that reads like one: Postgres's class 23, MySQL's duplicate key,
// foreign key, NOT NULL and CHECK refusals, and every SQLITE_CONSTRAINT
// code. An error without a driver's type is no violation.
//
// MySQL's 1061, an index name already taken, counts too, though it is a
// schema error: createIndex in db/table.go expects it when a process
// migrating at the same time made the index first, and then reads the
// index back to check it, so the statement must not log a failure. A 1061
// anywhere else still fails its migration and reaches the caller; only
// its log line is at debug.
func expected(err error) bool {
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, context.Canceled) {
		return true
	}
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		return strings.HasPrefix(pg.Code, "23") // integrity constraint violation
	}
	if my, ok := errors.AsType[*gomysql.MySQLError](err); ok {
		switch my.Number {
		case mysqlDupKey, mysqlDupKeyName, mysqlDuplicateEntry,
			mysqlForeignDuplicateKeyWithChildInfo, mysqlForeignDuplicateKeyWithoutChildInfo,
			mysqlNoReferencedRow, mysqlRowIsReferenced, mysqlRowIsReferenced2, mysqlNoReferencedRow2,
			mysqlBadNull, mysqlNoDefaultForField, mysqlCheckViolated:
			return true
		}
		return false
	}
	if lite, ok := errors.AsType[sqliteCoder](err); ok {
		return lite.Code()&0xff == sqliteConstraint
	}
	return false
}

// sqliteCoder is the pure-Go SQLite driver's error, which carries its
// extended result code. Its methods are spelled out: embedding error
// would make go doc take error for a type of this package and stop
// listing Open under DB.
type sqliteCoder interface {
	Error() string
	Code() int
}

// The codes expected reads, MySQL's under the names of its error
// reference.
const (
	mysqlDupKey                              = 1022 // ER_DUP_KEY: Can't write; duplicate key in table 't'
	mysqlBadNull                             = 1048 // ER_BAD_NULL_ERROR: Column 'c' cannot be null
	mysqlDupKeyName                          = 1061 // ER_DUP_KEYNAME: Duplicate key name 'k' (see expected)
	mysqlDuplicateEntry                      = 1062 // ER_DUP_ENTRY: Duplicate entry 'x' for key 'k'
	mysqlNoReferencedRow                     = 1216 // ER_NO_REFERENCED_ROW: a foreign key refuses an insert or an update
	mysqlRowIsReferenced                     = 1217 // ER_ROW_IS_REFERENCED: a foreign key refuses a delete or an update
	mysqlNoDefaultForField                   = 1364 // ER_NO_DEFAULT_FOR_FIELD: a NOT NULL column without a default left out
	mysqlRowIsReferenced2                    = 1451 // ER_ROW_IS_REFERENCED_2: 1217 naming the constraint
	mysqlNoReferencedRow2                    = 1452 // ER_NO_REFERENCED_ROW_2: 1216 naming the constraint
	mysqlForeignDuplicateKeyWithChildInfo    = 1761 // ER_FOREIGN_DUPLICATE_KEY_WITH_CHILD_INFO: a cascade would duplicate a key of the child table it names
	mysqlForeignDuplicateKeyWithoutChildInfo = 1762 // ER_FOREIGN_DUPLICATE_KEY_WITHOUT_CHILD_INFO: the same, naming no child table
	mysqlCheckViolated                       = 3819 // ER_CHECK_CONSTRAINT_VIOLATED: Check constraint 'c' is violated.
	sqliteConstraint                         = 19   // SQLITE_CONSTRAINT, the low byte of its extended codes
)

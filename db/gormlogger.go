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

// The codes expected and errorText read, MySQL's under the names of its
// error reference.
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
	sqliteError                              = 1    // SQLITE_ERROR, the low byte of its extended codes
	sqliteConstraint                         = 19   // SQLITE_CONSTRAINT, the low byte of its extended codes
	sqliteConstraintTrigger                  = 1811 // SQLITE_CONSTRAINT_TRIGGER: a trigger's RAISE
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
//
// pgxEncode finds the argument pgx quotes when it cannot encode it for
// the type of its parameter, before the statement is sent: pgx v5's
// ExtendedQueryBuilder.Build wraps pgtype's unable to encode x into
// binary format for int4 (OID 23), or unable to encode x for an element
// of an array or a field of a composite, as failed to encode args[i].
// The argument is printed with %#v or %v and may hold anything, a
// separator or a line break included, so the match runs from the index
// to the end of the text.
//
// pgtypeEncode finds the value in pgtype's error as pgx returns it bare
// in its simple protocol, which a DSN's
// default_query_exec_mode=simple_protocol picks: unable to encode x into
// text format for unknown type (OID 0): cannot find encode plan. What
// follows the type may quote the value again (31337 is greater than
// maximum value for int2) or nest another unable to encode, so the match
// runs to the last format, keeps it and its type, and drops the rest.
//
// postgresText and mysqlText find a Postgres or a MySQL error that lost
// its type. gorm adds an error to a statement's as %v; %w, which keeps
// the type of the last one alone: when the rollback fails after the
// statement, the statement's error is text before the rollback's. pgx
// writes it ERROR: message (SQLSTATE 22P02), with FATAL or PANIC for the
// severity, and the match runs to the last SQLSTATE, so a value that
// reads like one goes as well. go-sql-driver writes it Error 1062
// (23000): message, whose end nothing marks but the separator gorm puts
// after it, so the match runs to the first separator or the end of the
// text, a line break in the value included; a value that holds the
// separator ends it early.
var (
	scanValue    = regexp.MustCompile(`(converting driver\.Value type \S+ )\("(?:[^"\\]|\\.)*"\) `)
	scanBool     = regexp.MustCompile(`(couldn't convert ).* (into type bool)`)
	pgxEncode    = regexp.MustCompile(`(?s)(failed to encode args\[\d+\]): .*`)
	pgtypeEncode = regexp.MustCompile(`(?s)(unable to encode ).* (into (?:binary|text) format for .*? \(OID \d+\)).*`)
	postgresText = regexp.MustCompile(`(?s)\b(?:ERROR|FATAL|PANIC): .* \(SQLSTATE ([0-9A-Z]{5})\)`)
	mysqlText    = regexp.MustCompile(`(?s)\bError (\d+) (\([0-9A-Z]{5}\)): .*?(; |$)`)
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
// the message kept. pgx's error for an argument it cannot encode for the
// type of its parameter, which a raw statement given a Go value of the
// wrong type meets, quotes the argument: it is cut after the argument's
// index (failed to encode args[0]), or, where pgx's simple protocol
// returns pgtype's error bare, after the type it encoded for (unable to
// encode into text format for unknown type (OID 0)). A Postgres or
// MySQL error that lost its type is logged as its driver and code, and
// the text around it is kept: gorm keeps the type of a statement's last
// error alone, so when the rollback fails after the statement, the
// statement's error is text before the rollback's (mysql 1062 (23000);
// driver: bad connection). Any other error, a network fault, a
// cancellation, gorm's or chok's own, is logged as it is. The caller
// gets the error unchanged, and expected reads its type and code.
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
	if lite, ok := errors.AsType[sqliteCoder](err); ok {
		if code := lite.Code(); code&0xff == sqliteError || code == sqliteConstraintTrigger {
			return fmt.Sprintf("sqlite %d", code)
		}
	}
	s := scanValue.ReplaceAllString(err.Error(), "$1")
	s = scanBool.ReplaceAllString(s, "$1$2")
	s = pgxEncode.ReplaceAllString(s, "$1")
	s = pgtypeEncode.ReplaceAllString(s, "$1$2")
	s = postgresText.ReplaceAllString(s, "postgres $1")
	return mysqlText.ReplaceAllString(s, "mysql $1 $2$3")
}

// Package dberr words a database statement's error without the values a
// driver's message may quote. db's statement log and telemetry's span
// status both say a failed statement's error with Text, so the log and
// the trace name the same driver, code and names, and neither carries a
// bound value.
package dberr

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// The codes Text reads, MySQL's under the name of its error reference.
const (
	mysqlDuplicateEntry     = 1062 // ER_DUP_ENTRY: Duplicate entry 'x' for key 'k'
	sqliteError             = 1    // SQLITE_ERROR, the low byte of its extended codes
	sqliteConstraintTrigger = 1811 // SQLITE_CONSTRAINT_TRIGGER: a trigger's RAISE
)

// sqliteCoder is the pure-Go SQLite driver's error, which carries its
// extended result code.
type sqliteCoder interface {
	error
	Code() int
}

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

// Text is what the log and the trace say of a statement's error: db's
// statement log writes it beside the statement, telemetry sets it as the
// status of the statement's span. The drivers' messages quote the values
// a statement bound: MySQL's Duplicate entry 'x' for key 'k', Incorrect
// integer value 'x' for column 'c' and Truncated incorrect DOUBLE value
// 'x', Postgres's invalid input syntax for type integer "x" and value "x"
// is out of range. The SQL beside the error keeps placeholders, and the
// message would bring back the values it leaves out. So a Postgres or
// MySQL error is written as its driver, its code and the names it
// carries: Postgres's constraint, table and column fields, the key a
// MySQL duplicate names. With the statement, they locate the fault:
//
//	postgres 23505 constraint uk_user_email table users
//	mysql 1062 (23000) key users.uk_user_email
//	mysql 1366 (HY000)
//
// SQLite's messages name objects (UNIQUE constraint failed: users.email)
// and are kept, except two kinds written as their code alone (sqlite 1,
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
// MySQL error that lost its type is written as its driver and code, and
// the text around it is kept: gorm keeps the type of a statement's last
// error alone, so when the rollback fails after the statement, the
// statement's error is text before the rollback's (mysql 1062 (23000);
// driver: bad connection). Any other error, a network fault, a
// cancellation, gorm's or chok's own, is kept as it is. The caller gets
// the error unchanged, and db's expected reads its type and code.
func Text(err error) string {
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

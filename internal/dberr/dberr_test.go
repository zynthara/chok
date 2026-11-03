package dberr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// liteError imitates the pure-Go SQLite driver's error: an extended
// result code and the driver's message.
type liteError struct {
	code int
	msg  string
}

func (e liteError) Error() string { return e.msg }
func (e liteError) Code() int     { return e.code }

// TestText: a Postgres or MySQL error, bare or wrapped, is its
// driver, code and names, never the value its message quotes; so is a
// SQLite error of SQLITE_ERROR or a trigger's RAISE. Any other error is
// logged as it is.
func TestText(t *testing.T) {
	state := func(s string) (b [5]byte) {
		copy(b[:], s)
		return b
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{&pgconn.PgError{Severity: "ERROR", Code: "22P02", Message: `invalid input syntax for type integer: "s3cret-value"`}, "postgres 22P02"},
		{&pgconn.PgError{Severity: "ERROR", Code: "22003", Message: `value "s3cret-value" is out of range for type integer`}, "postgres 22003"},
		{&pgconn.PgError{
			Severity: "ERROR", Code: "23505", Message: `duplicate key value violates unique constraint "uk_user_email"`,
			Detail: "Key (email)=(s3cret-value) already exists.", ConstraintName: "uk_user_email", TableName: "users",
		}, "postgres 23505 constraint uk_user_email table users"},
		{&pgconn.PgError{
			Severity: "ERROR", Code: "23502", Message: `null value in column "email" of relation "users" violates not-null constraint`,
			Detail: "Failing row contains (1, s3cret-value, null).", TableName: "users", ColumnName: "email",
		}, "postgres 23502 table users column email"},
		{&gomysql.MySQLError{Number: 1062, SQLState: state("23000"), Message: "Duplicate entry 's3cret-value' for key 'users.uk_user_email'"},
			"mysql 1062 (23000) key users.uk_user_email"},
		// A value that reads like a key does not stand in for the key.
		{&gomysql.MySQLError{Number: 1062, SQLState: state("23000"), Message: "Duplicate entry 'x' for key 's3cret-value' for key 'users.uk_user_email'"},
			"mysql 1062 (23000) key users.uk_user_email"},
		{&gomysql.MySQLError{Number: 1366, SQLState: state("HY000"), Message: "Incorrect integer value: 's3cret-value' for column 'age' at row 1"},
			"mysql 1366 (HY000)"},
		{&gomysql.MySQLError{Number: 1292, SQLState: state("22007"), Message: "Truncated incorrect DOUBLE value: 's3cret-value'"}, "mysql 1292 (22007)"},
		{&gomysql.MySQLError{Number: 1105, Message: "s3cret-value"}, "mysql 1105"},
		{liteError{1, "SQL logic error: bad JSON path: 's3cret-value' (1)"}, "sqlite 1"},
		// The code cannot tell SQL that does not compile from a refused
		// value, so its message goes too.
		{liteError{1, "SQL logic error: no such table: users (1)"}, "sqlite 1"},
		{liteError{257, "SQL logic error: no such collation sequence: nocase2 (257)"}, "sqlite 257"},
		{liteError{1811, "constraint failed: email s3cret-value is taken (1811)"}, "sqlite 1811"},
	} {
		for _, err := range []error{c.err, fmt.Errorf("create: %w", c.err)} {
			if got := Text(err); got != c.want {
				t.Errorf("%v: %q, want %q", err, got, c.want)
			}
		}
	}
	for _, err := range []error{
		liteError{2067, "constraint failed: UNIQUE constraint failed: users.email (2067)"},
		liteError{5, "database is locked (5) (SQLITE_BUSY)"},
		errors.New("connection reset by peer"),
		context.Canceled,
		gorm.ErrRecordNotFound,
	} {
		for _, err := range []error{err, fmt.Errorf("create: %w", err)} {
			if got := Text(err); got != err.Error() {
				t.Errorf("%q, want it as it is: %q", got, err.Error())
			}
		}
	}
	// database/sql quotes a value it cannot scan with %q, %d or %v; the
	// value goes and the rest stays. A quote or a closing parenthesis in
	// the value does not end it early.
	scan := func(format string, v ...any) error {
		return fmt.Errorf("sql: Scan error on column index 0, name %q: %w", "n", fmt.Errorf(format, v...))
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{scan("converting driver.Value type %T (%q) to a %s: %v", "", "s3cret-value", "int", "invalid syntax"),
			`sql: Scan error on column index 0, name "n": converting driver.Value type string to a int: invalid syntax`},
		{scan("converting driver.Value type %T (%q) to a %s: %v", []byte(nil), `s3cret "value") to a int: x`, "float64", "invalid syntax"),
			`sql: Scan error on column index 0, name "n": converting driver.Value type []uint8 to a float64: invalid syntax`},
		{scan("converting driver.Value type string (%q) to a UUID: %v", "s3cret-value", "invalid uuid"),
			`sql: Scan error on column index 0, name "n": converting driver.Value type string to a UUID: invalid uuid`},
		{scan("sql/driver: couldn't convert %q into type bool", `s3cret "value" into type bool`),
			`sql: Scan error on column index 0, name "n": sql/driver: couldn't convert into type bool`},
		{scan("sql/driver: couldn't convert %d into type bool", 31337),
			`sql: Scan error on column index 0, name "n": sql/driver: couldn't convert into type bool`},
		{scan("sql/driver: couldn't convert %v (%T) into type bool", 3.5, 3.5),
			`sql: Scan error on column index 0, name "n": sql/driver: couldn't convert into type bool`},
	} {
		if got := Text(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
		if got := Text(fmt.Errorf("find: %w", c.err)); got != "find: "+c.want {
			t.Errorf("wrapped %v: %q, want %q", c.err, got, "find: "+c.want)
		}
	}
	// pgx quotes, with %#v or %v, an argument it cannot encode for the
	// type of its parameter, after failed to encode args[i]: the index
	// stays and the rest goes, whatever the value holds.
	encode := func(i int, err error) error { return fmt.Errorf("failed to encode args[%d]: %w", i, err) }
	for _, c := range []struct {
		err  error
		want string
	}{
		{encode(0, fmt.Errorf("unable to encode %#v into %s format for %s (OID %d): %w",
			struct{ V string }{"s3cret-value"}, "binary", "int4", 23, errors.New("cannot find encode plan"))),
			"failed to encode args[0]"},
		{encode(1, fmt.Errorf("unable to encode %v into OID %d in binary format", "s3cret-value", 16385)), "failed to encode args[1]"},
		{encode(12, fmt.Errorf("unable to encode %v", "s3cret; value\ns3cret-value")), "failed to encode args[12]"},
	} {
		if got := Text(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
		if got := Text(fmt.Errorf("exec: %w", c.err)); got != "exec: "+c.want {
			t.Errorf("wrapped %v: %q, want %q", c.err, got, "exec: "+c.want)
		}
	}
	// In pgx's simple protocol pgtype's error comes bare: the value goes,
	// the format and the type stay, and what follows them goes too, which
	// may quote the value again or nest another encode error.
	pgtype := func(v any, format, typ string, oid int, err error) error {
		return fmt.Errorf("unable to encode %#v into %s format for %s (OID %d): %w", v, format, typ, oid, err)
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		// As pgx 5.10 answers a struct in the simple protocol.
		{errors.New(`unable to encode db_test.loggedArg{V:"s3cret-value"} into text format for unknown type (OID 0): cannot find encode plan`),
			"unable to encode into text format for unknown type (OID 0)"},
		{pgtype(31337, "binary", "int2", 21, fmt.Errorf("%d is greater than maximum value for int2", 31337)),
			"unable to encode into binary format for int2 (OID 21)"},
		{pgtype(struct{ V any }{"x"}, "text", "unknown type", 0, pgtype("s3cret-value", "text", "unknown type", 0, errors.New("cannot find encode plan"))),
			"unable to encode into text format for unknown type (OID 0)"},
	} {
		if got := Text(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
		if got := Text(fmt.Errorf("exec: %w", c.err)); got != "exec: "+c.want {
			t.Errorf("wrapped %v: %q, want %q", c.err, got, "exec: "+c.want)
		}
	}
	// gorm adds an error to a statement's as %v; %w, so only the last
	// keeps its type: a driver's error before it, the statement's when
	// the rollback fails too, is text, logged as its driver and code.
	// MySQL's message runs to the separator, a line break in the value
	// included; Postgres's runs to its last SQLSTATE, so a value that reads
	// like one goes as well.
	join := func(first, last error) error { return fmt.Errorf("%v; %w", first, last) }
	rollback := errors.New("rollback failed")
	dup := &gomysql.MySQLError{Number: 1062, SQLState: state("23000"), Message: "Duplicate entry 's3cret-value' for key 'users.uk_user_email'"}
	for _, c := range []struct {
		err  error
		want string
	}{
		{join(dup, rollback), "mysql 1062 (23000); rollback failed"},
		{join(&gomysql.MySQLError{Number: 1366, SQLState: state("HY000"), Message: "Incorrect integer value: 's3cret\nvalue' for column 'age' at row 1"}, rollback),
			"mysql 1366 (HY000); rollback failed"},
		{join(join(dup, errors.New("driver: bad connection")), rollback), "mysql 1062 (23000); driver: bad connection; rollback failed"},
		{join(&pgconn.PgError{Severity: "ERROR", Code: "23505", Message: `duplicate key value violates unique constraint "uk_user_email"`}, rollback),
			"postgres 23505; rollback failed"},
		{join(&pgconn.PgError{Severity: "ERROR", Code: "22P02", Message: `invalid input syntax for type integer: "s3cret (SQLSTATE 00000); value"`}, rollback),
			"postgres 22P02; rollback failed"},
		{join(&pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}, rollback),
			"postgres 57P01; rollback failed"},
	} {
		if got := Text(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
		if got := Text(fmt.Errorf("create: %w", c.err)); got != "create: "+c.want {
			t.Errorf("wrapped %v: %q, want %q", c.err, got, "create: "+c.want)
		}
	}
}

package db_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/zynthara/chok/db"
	"github.com/zynthara/chok/internal/dbtest"
)

// loggedAccount is a model whose email the test declares unique with
// db.Unique, so a second row with the same email is refused.
type loggedAccount struct {
	db.Model
	Email string `gorm:"size:64;not null"`
}

// logField returns the value of key in a line of slog's text handler,
// unquoted.
func logField(line, key string) string {
	_, v, ok := strings.Cut(line, " "+key+"=")
	if !ok {
		return ""
	}
	if q, err := strconv.QuotedPrefix(v); err == nil {
		s, _ := strconv.Unquote(q)
		return s
	}
	v, _, _ = strings.Cut(strings.TrimSpace(v), " ")
	return v
}

// TestGORMLogger_RefusedErrorLogsNoValue: a duplicate is logged at debug
// as a refused statement whose error names the driver, the code and the
// constraint, and not the value MySQL quotes in its message (Duplicate
// entry 'x' for key 'k').
func TestGORMLogger_RefusedErrorLogsNoValue(t *testing.T) {
	d, log := debugHandle(t, db.Tables(db.Table(&loggedAccount{}, db.Unique("uk_logged_account_email", "email"))))
	gdb := d.Unsafe(t.Context())
	if err := gdb.Create(&loggedAccount{Email: boundValue}).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&loggedAccount{Email: boundValue}).Error; err == nil {
		t.Fatal("the unique index refuses the second row")
	}
	s := log.String()
	var line string
	for l := range strings.Lines(s) {
		if strings.Contains(l, `msg="db: query refused"`) && strings.HasPrefix(logField(l, "sql"), "INSERT INTO") {
			line = l
		}
	}
	if !strings.Contains(line, "level=DEBUG") {
		t.Fatalf("the insert is logged at debug as refused on %s:\n%s", dbtest.Driver(), s)
	}
	want := map[string]string{
		"sqlite":   "constraint failed: UNIQUE constraint failed: logged_accounts.email (2067)",
		"postgres": "postgres 23505 constraint uk_logged_account_email table logged_accounts",
		"mysql":    "mysql 1062 (23000) key logged_accounts.uk_logged_account_email",
	}[dbtest.Driver()]
	if got := logField(line, "error"); got != want {
		t.Errorf("error=%q on %s, want %q", got, dbtest.Driver(), want)
	}
	if strings.Contains(s, boundValue) {
		t.Errorf("the value stays out of the log on %s:\n%s", dbtest.Driver(), s)
	}
}

// TestGORMLogger_FailedErrorLogsNoValue: a value the database cannot
// read is logged at error as a failed statement whose error names the
// driver and the code, and not the value the message quotes: MySQL's
// Incorrect integer value 'x', Postgres's invalid input syntax "x" and,
// as SQLite stores text in an integer column, the path of its bad JSON
// path.
func TestGORMLogger_FailedErrorLogsNoValue(t *testing.T) {
	d, log := debugHandle(t)
	gdb := d.Unsafe(t.Context())
	query := "INSERT INTO logged_numbers (n) VALUES (?)"
	if dbtest.Driver() == "sqlite" {
		query = "SELECT json_extract('{}', ?)"
	} else if err := gdb.Exec("CREATE TABLE logged_numbers (n integer)").Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(query, boundValue).Error; err == nil {
		t.Fatalf("%s refuses %q on %s", query, boundValue, dbtest.Driver())
	}
	s := log.String()
	line := statementLine(s, "db: query failed", query)
	if !strings.Contains(line, "level=ERROR") {
		t.Fatalf("the statement is logged at error on %s:\n%s", dbtest.Driver(), s)
	}
	want := map[string]string{
		"sqlite":   "sqlite 1",
		"postgres": "postgres 22P02",
		"mysql":    "mysql 1366 (HY000)",
	}[dbtest.Driver()]
	if got := logField(line, "error"); got != want {
		t.Errorf("error=%q on %s, want %q", got, dbtest.Driver(), want)
	}
	if strings.Contains(s, boundValue) {
		t.Errorf("the value stays out of the log on %s:\n%s", dbtest.Driver(), s)
	}
}

// loggedInt and loggedBool read the column of logged_texts, which holds
// text, into an int and a bool field, as a model would after the type of
// its column changed under it.
type loggedInt struct{ N int }

type loggedBool struct{ N bool }

func (loggedInt) TableName() string  { return "logged_texts" }
func (loggedBool) TableName() string { return "logged_texts" }

// TestGORMLogger_ScanErrorLogsNoValue: a value that database/sql cannot
// scan into the field is logged at error without the value its message
// quotes (converting driver.Value type string ("x") to a int, couldn't
// convert "x" into type bool).
func TestGORMLogger_ScanErrorLogsNoValue(t *testing.T) {
	d, log := debugHandle(t)
	gdb := d.Unsafe(t.Context())
	// SQLite keeps text in an integer column; the others want a text one.
	column, src := "varchar(64)", "string"
	switch dbtest.Driver() {
	case "sqlite":
		column = "integer"
	case "mysql":
		src = "[]uint8"
	}
	if err := gdb.Exec("CREATE TABLE logged_texts (n " + column + ")").Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec("INSERT INTO logged_texts (n) VALUES (?)", boundValue).Error; err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dest any
		want string
	}{
		{&[]loggedInt{}, `sql: Scan error on column index 0, name "n": converting driver.Value type ` + src + ` to a int: invalid syntax`},
		{&[]loggedBool{}, `sql: Scan error on column index 0, name "n": sql/driver: couldn't convert into type bool`},
	} {
		start := len(log.String())
		if err := gdb.Find(c.dest).Error; err == nil {
			t.Fatalf("%T holds %q on %s", c.dest, boundValue, dbtest.Driver())
		}
		var line string
		for l := range strings.Lines(log.String()[start:]) {
			if strings.Contains(l, `msg="db: query failed"`) && strings.Contains(logField(l, "sql"), "logged_texts") {
				line = l
			}
		}
		if !strings.Contains(line, "level=ERROR") {
			t.Fatalf("reading %T is logged at error on %s:\n%s", c.dest, dbtest.Driver(), log.String()[start:])
		}
		if got := logField(line, "error"); got != c.want {
			t.Errorf("%T: error=%q on %s, want %q", c.dest, got, dbtest.Driver(), c.want)
		}
	}
	if s := log.String(); strings.Contains(s, boundValue) {
		t.Errorf("the value stays out of the log on %s:\n%s", dbtest.Driver(), s)
	}
}

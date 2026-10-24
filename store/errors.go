package store

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gorm.io/gorm"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zynthara/chok/v3"
	"github.com/zynthara/chok/v3/store/where"
)

// NotFoundError is a locator that matched no row the caller may see.
type NotFoundError struct {
	Locator string
}

func (e *NotFoundError) Error() string { return "store: not found: " + e.Locator }

// APIError is the 404.
func (e *NotFoundError) APIError() *chok.Error { return chok.ErrNotFound }

// Is matches chok.ErrNotFound.
func (e *NotFoundError) Is(target error) bool { return chok.ErrNotFound.Is(target) }

// VersionConflictError is an update or delete whose version no longer
// matches the row: someone changed it since it was read.
type VersionConflictError struct {
	Locator string
	Version int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("store: version %d of %s is stale", e.Version, e.Locator)
}

// APIError is the 409, with metadata.version.
func (e *VersionConflictError) APIError() *chok.Error {
	return chok.ErrConflict.Msgf("the resource changed since version %d; reload it and retry", e.Version).With("version", e.Version)
}

// Is matches chok.ErrConflict.
func (e *VersionConflictError) Is(target error) bool { return chok.ErrConflict.Is(target) }

// DuplicateError is a write that a unique constraint refused. Field is
// the field the constraint guards, when known; Constraint its name.
type DuplicateError struct {
	Field      string
	Constraint string
	cause      error
}

func (e *DuplicateError) Error() string {
	if e.Field != "" {
		return "store: duplicate " + e.Field
	}
	return "store: duplicate entry"
}

func (e *DuplicateError) Unwrap() error { return e.cause }

// APIError is the 409, with metadata.field.
func (e *DuplicateError) APIError() *chok.Error {
	if e.Field != "" {
		return chok.ErrConflict.Msgf("%s is already taken", e.Field).With("field", e.Field)
	}
	if e.Constraint != "" {
		return chok.ErrConflict.Msgf("duplicate entry").With("constraint", e.Constraint)
	}
	return chok.ErrConflict.Msgf("duplicate entry")
}

// Is matches chok.ErrConflict.
func (e *DuplicateError) Is(target error) bool { return chok.ErrConflict.Is(target) }

// EmptyChangesError is an update that changes nothing.
type EmptyChangesError struct{}

func (e *EmptyChangesError) Error() string { return "store: the update changes no field" }

// APIError is the 400.
func (e *EmptyChangesError) APIError() *chok.Error {
	return chok.ErrBadRequest.Msgf("the request changes no updatable field")
}

// Is matches chok.ErrBadRequest.
func (e *EmptyChangesError) Is(target error) bool { return chok.ErrBadRequest.Is(target) }

// NoPrincipalError is a write to an owner-scoped model without a
// principal in the context.
type NoPrincipalError struct{}

func (e *NoPrincipalError) Error() string { return "store: an owner-scoped write needs a principal" }

// APIError is the 401.
func (e *NoPrincipalError) APIError() *chok.Error { return chok.ErrUnauthorized }

// Is matches chok.ErrUnauthorized.
func (e *NoPrincipalError) Is(target error) bool { return chok.ErrUnauthorized.Is(target) }

// MissingConditionsError is store.Where with no filter: a programming
// error that would otherwise update or delete every row.
type MissingConditionsError struct{}

func (e *MissingConditionsError) Error() string {
	return "store: store.Where needs at least one filter condition"
}

// APIError is a 500: the request is fine, the code is not.
func (e *MissingConditionsError) APIError() *chok.Error { return chok.ErrInternal.Wrap(e) }

// queryError turns a malformed query option into a 400; anything else
// passes through.
func queryError(err error) error {
	if errors.Is(err, where.ErrInvalidParam) || errors.Is(err, where.ErrUnknownField) {
		return chok.ErrBadRequest.Msgf("%s", strings.TrimPrefix(err.Error(), "where: ")).Wrap(err)
	}
	return err
}

// mapError classifies a database error: a missing record, a duplicate
// key with the field it collided on, or the error as it is.
func (s *Store[T]) mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &NotFoundError{Locator: "row"}
	}
	if !isDuplicate(err) {
		return err
	}
	dup := &DuplicateError{cause: err}
	if name := constraintName(err.Error()); name != "" {
		dup.Constraint = name
		dup.Field = s.constraintField(name)
	}
	return dup
}

// constraintField resolves a constraint name, or SQLite's column list,
// to a field: the declared name first, then the columns. An unknown
// constraint has no field.
func (s *Store[T]) constraintField(name string) string {
	if f, ok := s.constraints[name]; ok {
		return f
	}
	if !strings.Contains(name, ".") {
		return ""
	}
	name = sqliteCode.ReplaceAllString(name, "")
	var cols []string
	for seg := range strings.SplitSeq(name, ",") {
		seg = strings.TrimSpace(seg)
		if _, bare, ok := strings.Cut(seg, "."); ok {
			seg = bare
		}
		cols = append(cols, seg)
	}
	if f, ok := s.constraints[strings.Join(cols, ",")]; ok {
		return f
	}
	if len(cols) == 1 {
		if f, ok := s.constraints[cols[0]]; ok {
			return f
		}
	}
	// SQLite names the columns, not the index.
	return s.fieldOfColumns(cols)
}

// The constraint names the dialects put in a duplicate-key message:
//
//	MySQL     Duplicate entry 'x' for key 'posts.uk_post_title'   (8.0.19+ prefixes the table)
//	Postgres  duplicate key value violates unique constraint "uk_post_title"
//	SQLite    UNIQUE constraint failed: posts.title, posts.owner_id, posts.delete_token (2067)
var (
	mysqlKey     = regexp.MustCompile(`for key '([^']+)'`)
	postgresKey  = regexp.MustCompile(`unique constraint "([^"]+)"`)
	sqliteUnique = regexp.MustCompile(`UNIQUE constraint failed:\s*([^\n]+)`)
	sqliteCode   = regexp.MustCompile(`\s*\(\d+\)$`)
)

func constraintName(msg string) string {
	if m := mysqlKey.FindStringSubmatch(msg); m != nil {
		if _, bare, ok := strings.Cut(m[1], "."); ok {
			return bare
		}
		return m[1]
	}
	if m := postgresKey.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	if m := sqliteUnique.FindStringSubmatch(msg); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// sqliteCoder is the extended result code the pure-Go SQLite driver
// exposes.
type sqliteCoder interface{ Code() int }

const (
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
	sqliteConstraintRowID      = 2579
	mysqlDuplicateEntry        = 1062
)

// isDuplicate recognizes a unique violation by the driver's own code
// where it has one: MySQL 1062, SQLSTATE 23505, SQLite's extended codes.
// The message is the fallback for an error that lost its type.
func isDuplicate(err error) bool {
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pg.Code == "23505"
	}
	if my, ok := errors.AsType[*mysql.MySQLError](err); ok {
		return my.Number == mysqlDuplicateEntry
	}
	if sc, ok := errors.AsType[interface {
		error
		sqliteCoder
	}](err); ok {
		switch sc.Code() {
		case sqliteConstraintPrimaryKey, sqliteConstraintUnique, sqliteConstraintRowID:
			return true
		}
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry") ||
		strings.Contains(msg, "UNIQUE constraint failed")
}

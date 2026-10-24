// Package where is the query vocabulary of store: filters, ordering,
// pagination and the query modifiers, each an Option that a store applies
// to its query. Fields are the model's public (JSON) names; the store maps
// them to columns and refuses names it does not know.
//
//	posts.List(ctx, where.Eq("status", "published"), where.Order("created_at", true), where.Page(1, 20))
//
// An Option receives the *gorm.DB under construction, which is why gorm
// appears in its signature: this package is where SQL fragments are made.
package where

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"

	"gorm.io/gorm"

	"github.com/zynthara/chok/v3/store/internal/q"
)

const (
	maxPageSize = 10_000 // caps every page, limit and cursor size
	maxInList   = 500    // caps an IN list; store.ListIn chunks larger inputs
)

var (
	// ErrInvalidParam reports a bad page, size, order or cursor.
	ErrInvalidParam = errors.New("where: invalid parameter")
	// ErrUnknownField reports a field the store does not expose for queries.
	ErrUnknownField = q.ErrUnknownField
)

// Option is one part of a query.
type Option func(tx *gorm.DB, cfg *Config, fields map[string]string) (*gorm.DB, error)

// Config is what the options of one query declared, read by the store
// that runs them.
type Config struct {
	Count       bool // Count: report the total
	HasFilter   bool // some WHERE condition was applied
	HasPage     bool // Page, Limit, Offset or Cursor was applied
	HasOrder    bool
	Degenerate  bool // a filter can match nothing (In with no values)
	WithTrashed bool // include soft-deleted rows
	OnlyTrashed bool // only soft-deleted rows
	// Version is the optimistic-lock version an Update or Delete must
	// match; 0 means none.
	Version int
	// Preloads are the relations to load with the rows.
	Preloads []string
	// Cursor and CursorSize are a cursor page request for
	// store.ListWithCursor; FirstPage marks the request for page one.
	Cursor     string
	CursorSize int
	FirstPage  bool
	// Orders are the order clauses, in the order given.
	Orders []orderBy
	// FiltersOnly, set by the store for a count, turns Count into a
	// no-op.
	FiltersOnly bool
	// PageNumber is the 1-based page Page asked for (0 for Offset or
	// cursor pagination); Offset and Limit are the rows to skip and take
	// (Limit 0: the store's default). The store renders them.
	PageNumber, Offset, Limit int
}

// orderBy is one order clause: the public field and its column.
type orderBy struct {
	Field, Column string
	Desc          bool
}

func compare(op string) func(field string, v any) Option {
	return func(field string, v any) Option {
		return func(tx *gorm.DB, cfg *Config, fields map[string]string) (*gorm.DB, error) {
			cfg.HasFilter = true
			col, err := q.Column(fields, field)
			if err != nil {
				return nil, err
			}
			if v == nil {
				cfg.Degenerate = true
				return tx.Where("1 = 0"), nil
			}
			return tx.Where(col+" "+op+" ?", v), nil
		}
	}
}

var (
	eq  = compare("=")
	ne  = compare("<>")
	gt  = compare(">")
	gte = compare(">=")
	lt  = compare("<")
	lte = compare("<=")
)

// Eq is field = v. A nil v matches nothing; use Null for IS NULL.
func Eq(field string, v any) Option { return eq(field, v) }

// Ne is field <> v.
func Ne(field string, v any) Option { return ne(field, v) }

// Gt is field > v.
func Gt(field string, v any) Option { return gt(field, v) }

// Gte is field >= v.
func Gte(field string, v any) Option { return gte(field, v) }

// Lt is field < v.
func Lt(field string, v any) Option { return lt(field, v) }

// Lte is field <= v.
func Lte(field string, v any) Option { return lte(field, v) }

// In is field IN (values...). One slice argument is unwrapped; no values
// match nothing. More than 500 values is an error (ErrInvalidParam):
// store.ListIn chunks a larger set.
func In(field string, values ...any) Option {
	var args any = values
	n := len(values)
	if len(values) == 1 {
		if rv := reflect.ValueOf(values[0]); values[0] == nil {
			args, n = []any{}, 0
		} else if rv.Kind() == reflect.Slice {
			args, n = values[0], rv.Len()
		}
	}
	return func(tx *gorm.DB, cfg *Config, fields map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := q.Column(fields, field)
		if err != nil {
			return nil, err
		}
		if n > maxInList {
			return nil, fmt.Errorf("%w: IN list of %d values exceeds %d; chunk the input", ErrInvalidParam, n, maxInList)
		}
		if n == 0 {
			cfg.Degenerate = true
			return tx.Where("1 = 0"), nil
		}
		return tx.Where(col+" IN ?", args), nil
	}
}

// Like is field LIKE pattern, with % and _ as the wildcards the caller
// wrote; Contains, StartsWith and EndsWith match their input literally
// instead. Case sensitivity is the dialect's: PostgreSQL compares case,
// MySQL follows the column collation (case-insensitive by default) and
// SQLite folds ASCII letters.
func Like(field, pattern string) Option {
	return func(tx *gorm.DB, cfg *Config, fields map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := q.Column(fields, field)
		if err != nil {
			return nil, err
		}
		return tx.Where(col+" LIKE ?", pattern), nil
	}
}

func likeEscaped(field, pattern string) Option {
	return func(tx *gorm.DB, cfg *Config, fields map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := q.Column(fields, field)
		if err != nil {
			return nil, err
		}
		return tx.Where(col+" LIKE ? ESCAPE '!'", pattern), nil
	}
}

// escapeLike makes s match itself under ESCAPE '!'. The escape character
// is not the backslash, which a MySQL string literal would swallow and
// SQLite does not know: '!' is plain in every dialect.
func escapeLike(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

// Contains matches field containing s literally.
func Contains(field, s string) Option { return likeEscaped(field, "%"+escapeLike(s)+"%") }

// StartsWith matches field starting with s literally.
func StartsWith(field, s string) Option { return likeEscaped(field, escapeLike(s)+"%") }

// EndsWith matches field ending with s literally.
func EndsWith(field, s string) Option { return likeEscaped(field, "%"+escapeLike(s)) }

// Null is field IS NULL.
func Null(field string) Option {
	return func(tx *gorm.DB, cfg *Config, fields map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := q.Column(fields, field)
		if err != nil {
			return nil, err
		}
		return tx.Where(col + " IS NULL"), nil
	}
}

// NotNull is field IS NOT NULL.
func NotNull(field string) Option {
	return func(tx *gorm.DB, cfg *Config, fields map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := q.Column(fields, field)
		if err != nil {
			return nil, err
		}
		return tx.Where(col + " IS NOT NULL"), nil
	}
}

// Order sorts by field, ascending unless desc is true. Several orders
// apply in the order given; the store renders them, ending with its own
// tie-break so pages never overlap.
func Order(field string, desc ...bool) Option {
	d := len(desc) > 0 && desc[0]
	return func(tx *gorm.DB, cfg *Config, fields map[string]string) (*gorm.DB, error) {
		col, err := q.Column(fields, field)
		if err != nil {
			return nil, err
		}
		cfg.HasOrder = true
		cfg.Orders = append(cfg.Orders, orderBy{Field: field, Column: col, Desc: d})
		return tx, nil
	}
}

// Page asks for 1-based page of size rows; size 0 takes the store's
// default page size.
func Page(page, size int) Option {
	return func(tx *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if page < 1 {
			return nil, fmt.Errorf("%w: page %d; pages start at 1", ErrInvalidParam, page)
		}
		if size < 0 || size > maxPageSize {
			return nil, fmt.Errorf("%w: size %d; it must be 1 to %d", ErrInvalidParam, size, maxPageSize)
		}
		// With size 0 the store picks the size and the offset with it.
		offset := int64(page-1) * int64(size)
		if offset > math.MaxInt32 {
			return nil, fmt.Errorf("%w: page %d of size %d is too far", ErrInvalidParam, page, size)
		}
		cfg.HasPage, cfg.PageNumber, cfg.Offset, cfg.Limit = true, page, int(offset), size
		return tx, nil
	}
}

// Limit caps the rows at n.
func Limit(n int) Option {
	return func(tx *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if n < 1 || n > maxPageSize {
			return nil, fmt.Errorf("%w: limit %d; it must be 1 to %d", ErrInvalidParam, n, maxPageSize)
		}
		cfg.HasPage, cfg.Limit = true, n
		return tx, nil
	}
}

// Offset skips n rows.
func Offset(n int) Option {
	return func(tx *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if n < 0 {
			return nil, fmt.Errorf("%w: offset %d", ErrInvalidParam, n)
		}
		cfg.HasPage, cfg.Offset = true, n
		return tx, nil
	}
}

// Cursor asks for the page of size rows after the opaque cursor c, as
// returned in a previous page's Cursor ("" for the first page). Only
// store.ListWithCursor honours it.
func Cursor(c string, size int) Option {
	return func(tx *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if size < 1 || size > maxPageSize {
			return nil, fmt.Errorf("%w: cursor size %d; it must be 1 to %d", ErrInvalidParam, size, maxPageSize)
		}
		cfg.HasPage, cfg.Cursor, cfg.CursorSize, cfg.FirstPage = true, c, size, c == ""
		return tx, nil
	}
}

// Count asks List to report the total matching rows.
func Count() Option {
	return func(tx *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if !cfg.FiltersOnly {
			cfg.Count = true
		}
		return tx, nil
	}
}

// Preload loads relation, a gorm association of the model, with the
// rows. The rows were fenced by the store; the association query is not
// fenced again, so preload only what the rows' readers may see.
func Preload(relation string) Option {
	return func(tx *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		cfg.Preloads = append(cfg.Preloads, relation)
		return tx, nil
	}
}

// WithTrashed includes soft-deleted rows.
func WithTrashed() Option {
	return func(tx *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		cfg.WithTrashed = true
		return tx, nil
	}
}

// OnlyTrashed matches only soft-deleted rows.
func OnlyTrashed() Option {
	return func(tx *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		cfg.OnlyTrashed = true
		return tx, nil
	}
}

// Version makes an Update or Delete apply only while the row is still at
// version v; otherwise it fails with a version conflict.
func Version(v int) Option {
	return func(tx *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if v < 1 {
			return nil, fmt.Errorf("%w: version %d", ErrInvalidParam, v)
		}
		cfg.Version = v
		return tx, nil
	}
}

// Package where provides query building options for store operations.
//
// Field-based options (WithFilter*, WithOrder) require a field whitelist
// configured via store.WithQueryFields. Unrecognized fields return an error.
package where

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"

	"gorm.io/gorm"
)

// MaxPageSize is the hard upper bound on page size accepted by WithPage
// and WithLimit. Requests above this value are rejected with
// ErrInvalidParam. Individual Stores may tighten further via
// store.WithMaxPageSize. Set deliberately below math.MaxInt32 to leave
// headroom for offset arithmetic (page * size) on 32-bit systems.
const MaxPageSize = 10_000

var (
	// ErrInvalidParam indicates a client-provided query parameter is invalid
	// (e.g. page < 1). Distinguished from config/field errors which are server bugs.
	ErrInvalidParam = errors.New("where: invalid parameter")

	// ErrUnknownField indicates a field name not present in the query whitelist.
	// Typically caused by client input (sort/filter on a non-queryable field).
	ErrUnknownField = errors.New("where: unknown field")

	// ErrFieldNotConfigured indicates WithQueryFields was not called on the Store.
	// This is a server-side configuration error (programming bug), not client input.
	ErrFieldNotConfigured = errors.New("where: fields not configured")
)

// Config holds query metadata extracted from options.
type Config struct {
	Count     bool // true if WithCount() was applied
	HasFilter bool // true if any WHERE condition was applied
	HasPage   bool // true if pagination (WithPage/WithOffset/WithLimit) was applied
	HasCursor bool // true if cursor-based pagination (WithCursor) was applied
	// DegenerateFilter is set when a filter option collapsed to a
	// guaranteed-empty result (currently only WithFilterIn over an empty
	// slice, which renders WHERE 1=0). HasFilter is still set so callers
	// that gate on "any filter present" see the filter, but locators that
	// want to reject "filter that matches nothing" (e.g. Update/Delete)
	// can inspect this flag to refuse the operation.
	DegenerateFilter bool
	MaxPageSize      int  // when > 0, LIMIT is clamped to this value
	limit            int  // tracks the effective LIMIT for MaxPageSize clamping
	countOnly        bool // internal: when true, pagination/order/count options become no-ops
}

// Option modifies a GORM query and/or query config.
// fieldMap is provided by Store at apply-time.
type Option func(db *gorm.DB, cfg *Config, fieldMap map[string]string) (*gorm.DB, error)

// Op is a comparison operator.
type Op string

const (
	Eq  Op = "="
	Ne  Op = "<>"
	Gt  Op = ">"
	Gte Op = ">="
	Lt  Op = "<"
	Lte Op = "<="
)

// --- Pagination ---

// WithPage sets page-based pagination.
// Returns ErrInvalidParam if page < 1, size < 1, size > MaxPageSize, or
// the implied offset (page-1)*size would overflow int32. Using int64
// math here lets us detect overflow even on 32-bit platforms.
func WithPage(page, size int) Option {
	return func(db *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if page < 1 {
			return nil, fmt.Errorf("%w: page %d, must be >= 1", ErrInvalidParam, page)
		}
		if size < 1 {
			return nil, fmt.Errorf("%w: page size %d, must be >= 1", ErrInvalidParam, size)
		}
		if size > MaxPageSize {
			return nil, fmt.Errorf("%w: page size %d exceeds MaxPageSize %d", ErrInvalidParam, size, MaxPageSize)
		}
		offset64 := int64(page-1) * int64(size)
		if offset64 > math.MaxInt32 {
			return nil, fmt.Errorf("%w: page %d size %d produces offset overflow", ErrInvalidParam, page, size)
		}
		cfg.HasPage = true
		cfg.limit = size
		if cfg.countOnly {
			return db, nil
		}
		return db.Offset(int(offset64)).Limit(size), nil
	}
}

// WithOffset sets a raw offset. Negative offsets are rejected.
func WithOffset(offset int) Option {
	return func(db *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if offset < 0 {
			return nil, fmt.Errorf("%w: offset %d, must be >= 0", ErrInvalidParam, offset)
		}
		cfg.HasPage = true
		if cfg.countOnly {
			return db, nil
		}
		return db.Offset(offset), nil
	}
}

// WithMaxPageSize clamps any subsequent LIMIT to at most max. This is a
// server-side safety measure that prevents clients from requesting
// unbounded result sets. Applied silently (no error) because this is a
// policy constraint, not a user input error.
func WithMaxPageSize(max int) Option {
	return func(db *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		cfg.MaxPageSize = max
		return db, nil
	}
}

// WithLimit sets a raw limit. Rejects limit < 1 and limit > MaxPageSize.
func WithLimit(limit int) Option {
	return func(db *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if limit < 1 {
			return nil, fmt.Errorf("%w: limit %d, must be >= 1", ErrInvalidParam, limit)
		}
		if limit > MaxPageSize {
			return nil, fmt.Errorf("%w: limit %d exceeds MaxPageSize %d", ErrInvalidParam, limit, MaxPageSize)
		}
		cfg.HasPage = true
		cfg.limit = limit
		if cfg.countOnly {
			return db, nil
		}
		return db.Limit(limit), nil
	}
}

// --- Filters (field whitelist enforced) ---

// WithFilter adds WHERE field = value.
// A nil value is treated as degenerate (SQL's three-valued logic makes
// `col = NULL` always false) and flagged via Config.DegenerateFilter so
// locators can reject "filter present but matches nothing" on
// Update/Delete. Use explicit IS NULL semantics via a custom option if
// that is the intended query.
func WithFilter(field string, value any) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		if value == nil {
			cfg.DegenerateFilter = true
			return db.Where("1 = 0"), nil
		}
		return db.Where(col+" = ?", value), nil
	}
}

// WithFilterOp adds WHERE field op value.
// op must be one of the predefined constants (Eq, Ne, Gt, Gte, Lt, Lte).
// A nil value is treated as degenerate for the same reason as WithFilter.
func WithFilterOp(field string, op Op, value any) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		if !validOp(op) {
			return nil, fmt.Errorf("where: unknown operator %q", string(op))
		}
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		if value == nil {
			cfg.DegenerateFilter = true
			return db.Where("1 = 0"), nil
		}
		return db.Where(col+" "+string(op)+" ?", value), nil
	}
}

func validOp(op Op) bool {
	switch op {
	case Eq, Ne, Gt, Gte, Lt, Lte:
		return true
	}
	return false
}

// MaxInList is the maximum number of values accepted by WithFilterIn.
// Above this, databases start rejecting queries: SQLite ~999, MySQL
// limited by max_allowed_packet, PostgreSQL ~65535 bound parameters.
// Callers with more values should chunk the IN list manually.
const MaxInList = 500

// WithFilterIn adds WHERE field IN (...).
// When called with a single slice argument (e.g. WithFilterIn("id", mySlice)),
// the slice is unwrapped so GORM receives the flat values instead of a
// nested []any{[]T{...}}.
// A nil or empty slice produces no-match (WHERE 1=0), rather than
// driver-dependent behaviour from a nil/empty IN list.
// Lists larger than MaxInList are rejected with ErrInvalidParam.
func WithFilterIn(field string, values ...any) Option {
	// Resolve args eagerly at option-construction time.
	var args any = values
	argLen := len(values)
	if len(values) == 1 {
		if values[0] == nil {
			// nil argument → guaranteed no-match
			args = []any{}
			argLen = 0
		} else if rv := reflect.ValueOf(values[0]); rv.Kind() == reflect.Slice {
			if rv.IsNil() || rv.Len() == 0 {
				// empty/nil slice → guaranteed no-match
				args = []any{}
				argLen = 0
			} else {
				args = values[0] // unwrap single slice
				argLen = rv.Len()
			}
		}
	}
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		if argLen > MaxInList {
			return nil, fmt.Errorf("%w: IN list size %d exceeds MaxInList %d; chunk the input", ErrInvalidParam, argLen, MaxInList)
		}
		// Empty IN is rendered as a literal "1 = 0" so we never emit
		// "IN ()" (MySQL syntax error) or rely on driver-specific behaviour
		// for zero-length slices. The col name is still resolved above so
		// unknown fields still fail fast. DegenerateFilter signals to
		// locators (Update/Delete) that the filter, while "present",
		// matches nothing — they may choose to reject rather than silently
		// no-op.
		if argLen == 0 {
			_ = col
			cfg.DegenerateFilter = true
			return db.Where("1 = 0"), nil
		}
		return db.Where(col+" IN ?", args), nil
	}
}

// WithFilterLike adds WHERE field LIKE pattern. The pattern is treated
// as a literal substring of arbitrary length — `%` and `_` in the input
// are escaped so user-supplied values cannot widen the match set. Use
// when you need a positional wildcard but want to keep the rest of the
// input safe (the helper does NOT inject leading/trailing `%`; pair it
// with WithFilterContains / StartsWith / EndsWith for those shapes).
//
// For the rare case where the caller genuinely wants the raw LIKE
// grammar (e.g. internal admin tooling that builds patterns server-side
// from a trusted source), use WithFilterLikeRaw and own the escaping.
func WithFilterLike(field string, pattern string) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		return db.Where(col+` LIKE ? ESCAPE '\'`, escapeLikePattern(pattern)), nil
	}
}

// WithFilterLikeRaw adds WHERE field LIKE pattern with the pattern
// passed through verbatim — `%` and `_` retain their wildcard meaning.
// Reserved for trusted callers that build patterns server-side; passing
// untrusted input here lets attackers expand the match set.
func WithFilterLikeRaw(field string, pattern string) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		return db.Where(col+" LIKE ?", pattern), nil
	}
}

// escapeLikePattern escapes SQL LIKE metacharacters (% _ \) in user-
// supplied input. Use with a literal `ESCAPE '\\'` clause — GORM's
// default driver behaviour accepts the backslash escape on MySQL,
// PostgreSQL, and SQLite without explicit ESCAPE clause.
func escapeLikePattern(s string) string {
	// Replace the escape char first so we don't double-escape.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

// WithFilterContains adds WHERE field LIKE '%<escaped value>%'. Wildcards
// in the caller-supplied value are escaped, so user input cannot expand
// the match set.
func WithFilterContains(field string, value string) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		return db.Where(col+` LIKE ? ESCAPE '\'`, "%"+escapeLikePattern(value)+"%"), nil
	}
}

// WithFilterStartsWith adds WHERE field LIKE '<escaped value>%'.
func WithFilterStartsWith(field string, value string) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		return db.Where(col+` LIKE ? ESCAPE '\'`, escapeLikePattern(value)+"%"), nil
	}
}

// WithFilterEndsWith adds WHERE field LIKE '%<escaped value>'.
func WithFilterEndsWith(field string, value string) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		return db.Where(col+` LIKE ? ESCAPE '\'`, "%"+escapeLikePattern(value)), nil
	}
}

// --- Ordering (field whitelist enforced) ---

// WithOrder adds ORDER BY field [DESC]. desc defaults to false (ASC).
func WithOrder(field string, desc ...bool) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		if cfg.countOnly {
			return db, nil
		}
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		dir := "ASC"
		if len(desc) > 0 && desc[0] {
			dir = "DESC"
		}
		return db.Order(col + " " + dir), nil
	}
}

// --- Cursor-based pagination ---

// CursorDirection specifies the scan direction for cursor-based pagination.
type CursorDirection string

const (
	// CursorAfter fetches rows AFTER the cursor value (ascending keyset).
	CursorAfter CursorDirection = "after"
	// CursorBefore fetches rows BEFORE the cursor value (descending keyset).
	CursorBefore CursorDirection = "before"
)

// WithCursor adds keyset-based (cursor) pagination. Instead of OFFSET, it
// uses WHERE field > cursor ORDER BY field LIMIT size, which is O(1)
// regardless of how deep the page is. Ideal for infinite-scroll APIs.
//
// field is validated against the query whitelist. direction controls
// whether to scan forward (CursorAfter) or backward (CursorBefore).
// cursor is the last-seen value of the sort field from the previous page.
// size is the maximum number of items to return.
//
// **Uniqueness requirement**: field MUST be a strictly unique column
// (typically `id` or `rid`). If multiple rows can share the same value
// (e.g. `created_at` at second resolution), rows on the boundary will
// be silently skipped because `> cursor` excludes equal values. Use
// WithCursorBy for a composite (field, id) cursor that handles ties.
//
// Typical usage:
//
//	where.WithCursor("id", where.CursorAfter, lastID, 20)
func WithCursor(field string, direction CursorDirection, cursor any, size int) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasPage = true
		cfg.HasCursor = true
		if cfg.countOnly {
			return db, nil
		}
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		if size < 1 {
			return nil, fmt.Errorf("%w: cursor size %d, must be >= 1", ErrInvalidParam, size)
		}
		// Enforce the package-level ceiling so a caller can't bypass
		// the global safety limit via cursor pagination. Per-Store caps
		// (cfg.MaxPageSize) are applied by Apply's trailing clamp.
		if size > MaxPageSize {
			return nil, fmt.Errorf("%w: cursor size %d exceeds MaxPageSize %d", ErrInvalidParam, size, MaxPageSize)
		}
		cfg.limit = size
		switch direction {
		case CursorAfter:
			db = db.Where(col+" > ?", cursor).Order(col + " ASC").Limit(size)
		case CursorBefore:
			db = db.Where(col+" < ?", cursor).Order(col + " DESC").Limit(size)
		default:
			return nil, fmt.Errorf("%w: cursor direction must be 'after' or 'before'", ErrInvalidParam)
		}
		return db, nil
	}
}

// WithCursorBy is the composite-cursor variant of WithCursor. It uses
// (field, id) as the keyset so rows sharing the same field value are
// still deterministically ordered and never skipped at page boundaries.
// Use this for non-unique sort columns like `created_at`.
//
// cursor encodes the last row's (field, id) pair. When both are nil,
// the first page is fetched (no cursor WHERE). The SQL is:
//
//	WHERE (field, id) > (?, ?) ORDER BY field ASC, id ASC LIMIT size   // CursorAfter
//	WHERE (field, id) < (?, ?) ORDER BY field DESC, id DESC LIMIT size // CursorBefore
//
// This relies on row-value comparison support, which all major SQL
// engines (MySQL 8+, PostgreSQL, SQLite 3.15+) implement.
func WithCursorBy(field string, direction CursorDirection, fieldCursor any, idCursor uint, size int) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasPage = true
		cfg.HasCursor = true
		if cfg.countOnly {
			return db, nil
		}
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		if size < 1 {
			return nil, fmt.Errorf("%w: cursor size %d, must be >= 1", ErrInvalidParam, size)
		}
		if size > MaxPageSize {
			return nil, fmt.Errorf("%w: cursor size %d exceeds MaxPageSize %d", ErrInvalidParam, size, MaxPageSize)
		}
		cfg.limit = size
		firstPage := fieldCursor == nil || fieldCursor == "" || idCursor == 0
		switch direction {
		case CursorAfter:
			if firstPage {
				db = db.Order(col + " ASC").Order("id ASC").Limit(size)
			} else {
				db = db.Where("("+col+", id) > (?, ?)", fieldCursor, idCursor).
					Order(col + " ASC").Order("id ASC").Limit(size)
			}
		case CursorBefore:
			if firstPage {
				db = db.Order(col + " DESC").Order("id DESC").Limit(size)
			} else {
				db = db.Where("("+col+", id) < (?, ?)", fieldCursor, idCursor).
					Order(col + " DESC").Order("id DESC").Limit(size)
			}
		default:
			return nil, fmt.Errorf("%w: cursor direction must be 'after' or 'before'", ErrInvalidParam)
		}
		return db, nil
	}
}

// --- Count control ---

// WithCount instructs List to execute a COUNT query and return actual total.
// Without this, List returns total = 0 and skips COUNT.
func WithCount() Option {
	return func(db *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if !cfg.countOnly {
			cfg.Count = true
		}
		return db, nil
	}
}

// Apply applies all options to the given GORM DB and returns the modified DB and config.
// Used internally by Store.
func Apply(db *gorm.DB, fieldMap map[string]string, opts []Option) (*gorm.DB, *Config, error) {
	cfg := &Config{}
	var err error
	for _, o := range opts {
		db, err = o(db, cfg, fieldMap)
		if err != nil {
			return nil, nil, err
		}
	}
	// Enforce max page size: only clamp when the requested limit exceeds
	// the cap (or no explicit limit was set). This preserves smaller limits
	// requested by WithPage / WithLimit.
	if cfg.MaxPageSize > 0 && (cfg.limit == 0 || cfg.limit > cfg.MaxPageSize) {
		db = db.Limit(cfg.MaxPageSize)
	}
	return db, cfg, nil
}

// ApplyFiltersOnly applies only filter options (skips pagination, ordering, count).
// Used by Store.List for the COUNT query so that LIMIT/OFFSET do not affect the total.
func ApplyFiltersOnly(db *gorm.DB, fieldMap map[string]string, opts []Option) (*gorm.DB, error) {
	cfg := &Config{countOnly: true}
	var err error
	for _, o := range opts {
		db, err = o(db, cfg, fieldMap)
		if err != nil {
			return nil, err
		}
	}
	return db, nil
}

// resolveField maps a public field name to a DB column via the whitelist.
// The resolved column name is validated as a SQL identifier before being
// returned: only ASCII letters/digits/underscore plus a single optional
// "table." qualifier are accepted. This is a defence-in-depth check on
// top of the whitelist itself — even if a Store accidentally registers
// a tainted column name (e.g. via auto-discovery from a model with
// hand-rolled JSON tags), the bad value never reaches GORM as raw SQL.
func resolveField(fm map[string]string, field string) (string, error) {
	if fm == nil {
		return "", fmt.Errorf("%w, cannot use field %q", ErrFieldNotConfigured, field)
	}
	col, ok := fm[field]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownField, field)
	}
	if !isSafeColumnIdent(col) {
		return "", fmt.Errorf("%w: column name %q for field %q is not a safe SQL identifier",
			ErrUnknownField, col, field)
	}
	return col, nil
}

// isSafeColumnIdent reports whether s is a safe column identifier for
// inclusion in raw SQL fragments (ORDER BY / cursor predicates). Accepts
// `name` and `qualifier.name` where each segment matches
// [A-Za-z_][A-Za-z0-9_]*. Rejects empty strings, whitespace, quotes,
// semicolons, comments, and any non-ASCII characters.
func isSafeColumnIdent(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	segments := strings.Split(s, ".")
	if len(segments) > 2 {
		return false
	}
	for _, seg := range segments {
		if seg == "" {
			return false
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			switch {
			case c == '_':
			case c >= 'a' && c <= 'z':
			case c >= 'A' && c <= 'Z':
			case c >= '0' && c <= '9':
				if i == 0 {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

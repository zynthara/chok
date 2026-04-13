// Package where provides query building options for store operations.
//
// Field-based options (WithFilter*, WithOrder) require a field whitelist
// configured via store.WithQueryFields. Unrecognized fields return an error.
package where

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

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
	countOnly bool // internal: when true, pagination/order/count options become no-ops
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
// Returns ErrInvalidParam if page < 1 or size < 1 (explicit validation, no silent correction).
func WithPage(page, size int) Option {
	return func(db *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		if page < 1 {
			return nil, fmt.Errorf("%w: page %d, must be >= 1", ErrInvalidParam, page)
		}
		if size < 1 {
			return nil, fmt.Errorf("%w: page size %d, must be >= 1", ErrInvalidParam, size)
		}
		cfg.HasPage = true
		if cfg.countOnly {
			return db, nil
		}
		return db.Offset((page - 1) * size).Limit(size), nil
	}
}

// WithOffset sets a raw offset.
func WithOffset(offset int) Option {
	return func(db *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		cfg.HasPage = true
		if cfg.countOnly {
			return db, nil
		}
		return db.Offset(offset), nil
	}
}

// WithLimit sets a raw limit.
func WithLimit(limit int) Option {
	return func(db *gorm.DB, cfg *Config, _ map[string]string) (*gorm.DB, error) {
		cfg.HasPage = true
		if cfg.countOnly {
			return db, nil
		}
		return db.Limit(limit), nil
	}
}

// --- Filters (field whitelist enforced) ---

// WithFilter adds WHERE field = value.
func WithFilter(field string, value any) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		return db.Where(col+" = ?", value), nil
	}
}

// WithFilterOp adds WHERE field op value.
// op must be one of the predefined constants (Eq, Ne, Gt, Gte, Lt, Lte).
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

// WithFilterIn adds WHERE field IN (...).
func WithFilterIn(field string, values ...any) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		return db.Where(col+" IN ?", values), nil
	}
}

// WithFilterLike adds WHERE field LIKE pattern.
func WithFilterLike(field string, pattern string) Option {
	return func(db *gorm.DB, cfg *Config, fm map[string]string) (*gorm.DB, error) {
		cfg.HasFilter = true
		col, err := resolveField(fm, field)
		if err != nil {
			return nil, err
		}
		return db.Where(col+" LIKE ?", pattern), nil
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

// --- Count control ---

// WithCount instructs List to execute a COUNT query and return actual total.
// Without this, List returns total = -1 and skips COUNT.
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
func resolveField(fm map[string]string, field string) (string, error) {
	if fm == nil {
		return "", fmt.Errorf("%w, cannot use field %q", ErrFieldNotConfigured, field)
	}
	col, ok := fm[field]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownField, field)
	}
	return col, nil
}

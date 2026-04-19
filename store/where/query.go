package where

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Reserved query parameter names.
const (
	QueryPage  = "page"
	QuerySize  = "size"
	QueryOrder = "order"
)

// DefaultPageSize is used when "size" is absent from the query.
const DefaultPageSize = 20

// FromQuery parses URL query parameters into []Option.
//
// Supported parameters:
//   - page: page number (default 1)
//   - size: items per page (default DefaultPageSize, overridden by defaultSize if > 0)
//   - order: "field:desc" or "field:asc" (field must be in allowedFields)
//   - Any key in allowedFields: equality filter (WHERE field = value)
//
// Unknown parameters are silently ignored. Use FromQueryStrict to reject
// them instead.
// Always includes WithCount().
func FromQuery(params url.Values, allowedFields map[string]string, defaultSize ...int) ([]Option, error) {
	return fromQuery(params, allowedFields, false, defaultSize...)
}

// FromQueryStrict is like FromQuery but rejects unknown query parameters
// with ErrInvalidParam. Used by Store.ListFromQuery when the Store was
// constructed with WithStrict.
func FromQueryStrict(params url.Values, allowedFields map[string]string, defaultSize ...int) ([]Option, error) {
	return fromQuery(params, allowedFields, true, defaultSize...)
}

func fromQuery(params url.Values, allowedFields map[string]string, strict bool, defaultSize ...int) ([]Option, error) {
	var opts []Option

	// --- Pagination ---
	page := 1
	size := DefaultPageSize
	if len(defaultSize) > 0 && defaultSize[0] > 0 {
		size = defaultSize[0]
	}
	if v := params.Get(QueryPage); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%w: page must be an integer", ErrInvalidParam)
		}
		page = p
	}
	if v := params.Get(QuerySize); v != "" {
		s, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%w: size must be an integer", ErrInvalidParam)
		}
		size = s
	}
	opts = append(opts, WithPage(page, size))

	// --- Order ---
	if v := params.Get(QueryOrder); v != "" {
		field, desc, err := parseOrder(v, allowedFields)
		if err != nil {
			return nil, err
		}
		opts = append(opts, WithOrder(field, desc))
	}

	// --- Filters ---
	reserved := map[string]bool{QueryPage: true, QuerySize: true, QueryOrder: true}
	for key, values := range params {
		if reserved[key] || len(values) == 0 || values[0] == "" {
			continue
		}
		if _, ok := allowedFields[key]; ok {
			opts = append(opts, WithFilter(key, values[0]))
			continue
		}
		if strict {
			return nil, fmt.Errorf("%w: unknown query parameter %q", ErrInvalidParam, key)
		}
		// Non-strict: silently ignore unknown keys to preserve the legacy
		// permissive behaviour used by most callers.
	}

	opts = append(opts, WithCount())
	return opts, nil
}

func parseOrder(s string, allowedFields map[string]string) (string, bool, error) {
	parts := strings.SplitN(s, ":", 2)
	field := parts[0]
	if _, ok := allowedFields[field]; !ok {
		return "", false, fmt.Errorf("%w: %q", ErrUnknownField, field)
	}
	desc := false
	if len(parts) == 2 {
		switch strings.ToLower(parts[1]) {
		case "desc":
			desc = true
		case "asc":
			// default
		default:
			return "", false, fmt.Errorf("%w: order direction must be asc or desc", ErrInvalidParam)
		}
	}
	return field, desc, nil
}

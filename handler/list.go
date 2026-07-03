package handler

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strconv"

	"github.com/zynthara/chok/v2/store/where"
)

// QueryLister is implemented by store.Store[T] (and wrappers that embed it).
// The interface decouples handler from store — handler never imports the store package.
type QueryLister[T any] interface {
	ListFromQuery(ctx context.Context, query url.Values) ([]T, int64, error)
}

// HandleList creates an http.Handler that parses page/size/order/filter
// from URL query parameters and returns a paginated ListResult.
//
// Supported query params (parsed by where.FromQuery):
//
//	page   — page number (default 1)
//	size   — items per page (default 20)
//	order  — "field:asc" or "field:desc"
//	<field> — equality filter (field must be in WithQueryFields)
//
// Usage:
//
//	r.Handle("GET", "/posts", handler.HandleList(postStore))
func HandleList[T any](lister QueryLister[T], opts ...HandleOption) http.Handler {
	cfg := &handleConfig{}
	for _, o := range opts {
		o(cfg)
	}

	return &metaHandler{
		serve: func(w http.ResponseWriter, r *http.Request) {
			items, total, err := lister.ListFromQuery(r.Context(), r.URL.Query())
			if err != nil {
				WriteResponse(w, r, 0, nil, err)
				return
			}
			// Guarantee non-nil slice so JSON serializes as [] not null.
			if items == nil {
				items = []T{}
			}
			// Parse page/size from query for the response envelope.
			// Bounds were already enforced by where.FromQuery; clamp here
			// only for the envelope's own display values. int64 math guards
			// against overflow if clients send arbitrary integers.
			q := r.URL.Query()
			page, _ := strconv.Atoi(q.Get("page"))
			size, _ := strconv.Atoi(q.Get("size"))
			if page < 1 {
				page = 1
			}
			if size < 1 {
				size = 20
			}
			if size > where.MaxPageSize {
				size = where.MaxPageSize
			}
			hasMore := total > 0 && int64(page)*int64(size) < total
			WriteResponse(w, r, http.StatusOK, &ListResult[T]{
				Items:   items,
				Total:   total,
				Page:    page,
				Size:    size,
				HasMore: hasMore,
			}, nil)
		},
		meta: Meta{
			RespType: reflect.TypeOf((*T)(nil)).Elem(),
			Code:     http.StatusOK,
			Summary:  cfg.summary,
			Tags:     cfg.tags,
			IsList:   true,
			Public:   cfg.public,
		},
	}
}

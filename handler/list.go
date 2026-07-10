package handler

import (
	"context"
	"net/http"
	"net/url"
	"reflect"

	"github.com/zynthara/chok/v2/store/where"
)

// QueryLister is implemented by store.Store[T] (and wrappers that embed it).
// The interface decouples handler from store — handler never imports the
// store package (where is the shared query layer both already speak).
// The where.PageInfo return is the pagination the query actually executed
// with; HandleList renders the envelope from it instead of re-deriving
// values from the raw request.
type QueryLister[T any] interface {
	ListFromQuery(ctx context.Context, query url.Values) ([]T, int64, where.PageInfo, error)
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
			items, total, pageInfo, err := lister.ListFromQuery(r.Context(), r.URL.Query())
			if err != nil {
				WriteResponse(w, r, 0, nil, err)
				return
			}
			// Guarantee non-nil slice so JSON serializes as [] not null.
			if items == nil {
				items = []T{}
			}
			// The envelope renders the pagination the query executed with
			// — same-sourced with the SQL LIMIT/OFFSET, so store caps and
			// defaults show up instead of an echo of the raw request.
			WriteResponse(w, r, http.StatusOK, &ListResult[T]{
				Items:   items,
				Total:   total,
				Page:    pageInfo.Page,
				Size:    pageInfo.Size,
				HasMore: pageInfo.HasMore,
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

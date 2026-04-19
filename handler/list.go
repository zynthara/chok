package handler

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/store/where"
)

// QueryLister is implemented by store.Store[T] (and wrappers that embed it).
// The interface decouples handler from store — handler never imports the store package.
type QueryLister[T any] interface {
	ListFromQuery(ctx context.Context, query url.Values) ([]T, int64, error)
}

// HandleList creates a gin handler that parses page/size/order/filter from
// URL query parameters and returns a paginated ListResult.
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
//	rg.GET("/posts", handler.HandleList(postStore))
func HandleList[T any](lister QueryLister[T], opts ...HandleOption) gin.HandlerFunc {
	cfg := &handleConfig{}
	for _, o := range opts {
		o(cfg)
	}

	ginH := func(c *gin.Context) {
		items, total, err := lister.ListFromQuery(c.Request.Context(), c.Request.URL.Query())
		if err != nil {
			WriteResponse(c, 0, nil, err)
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
		q := c.Request.URL.Query()
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
		WriteResponse(c, http.StatusOK, &ListResult[T]{
			Items:   items,
			Total:   total,
			Page:    page,
			Size:    size,
			HasMore: hasMore,
		}, nil)
	}
	registerMeta(ginH, &HandlerMeta{
		RespType: reflect.TypeOf((*T)(nil)).Elem(),
		Code:     http.StatusOK,
		Summary:  cfg.summary,
		Tags:     cfg.tags,
		IsList:   true,
		Public:   cfg.public,
	})
	return ginH
}

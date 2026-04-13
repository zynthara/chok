package handler

import (
	"context"
	"net/http"
	"net/url"
	"reflect"

	"github.com/gin-gonic/gin"
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
		WriteResponse(c, http.StatusOK, &ListResult[T]{Items: items, Total: total}, nil)
	}
	registerMeta(ginH, &HandlerMeta{
		RespType: reflect.TypeOf((*T)(nil)).Elem(),
		Code:     http.StatusOK,
		Summary:  cfg.summary,
		Tags:     cfg.tags,
		IsList:   true,
	})
	return ginH
}

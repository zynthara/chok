package swagger

import (
	"context"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/handler"
)

// Post registers a POST route and adds it to the OpenAPI spec.
func Post[T, R any](doc *Spec, rg *gin.RouterGroup, path string, h handler.HandlerFunc[T, R], op Op) {
	code := op.Code
	if code == 0 {
		code = http.StatusCreated
	}
	op.Code = code
	rg.POST(path, handler.HandleRequest(h, handler.WithSuccessCode(code)))
	doc.addOperation("post", rg.BasePath()+path, op,
		reflect.TypeOf((*T)(nil)).Elem(),
		reflect.TypeOf((*R)(nil)).Elem(),
	)
}

// Get registers a GET route and adds it to the OpenAPI spec.
func Get[T, R any](doc *Spec, rg *gin.RouterGroup, path string, h handler.HandlerFunc[T, R], op Op) {
	if op.Code == 0 {
		op.Code = http.StatusOK
	}
	rg.GET(path, handler.HandleRequest(h, handler.WithSuccessCode(op.Code)))
	doc.addOperation("get", rg.BasePath()+path, op,
		reflect.TypeOf((*T)(nil)).Elem(),
		reflect.TypeOf((*R)(nil)).Elem(),
	)
}

// Put registers a PUT route and adds it to the OpenAPI spec.
func Put[T, R any](doc *Spec, rg *gin.RouterGroup, path string, h handler.HandlerFunc[T, R], op Op) {
	if op.Code == 0 {
		op.Code = http.StatusOK
	}
	rg.PUT(path, handler.HandleRequest(h, handler.WithSuccessCode(op.Code)))
	doc.addOperation("put", rg.BasePath()+path, op,
		reflect.TypeOf((*T)(nil)).Elem(),
		reflect.TypeOf((*R)(nil)).Elem(),
	)
}

// Patch registers a PATCH route and adds it to the OpenAPI spec.
func Patch[T, R any](doc *Spec, rg *gin.RouterGroup, path string, h handler.HandlerFunc[T, R], op Op) {
	if op.Code == 0 {
		op.Code = http.StatusOK
	}
	rg.PATCH(path, handler.HandleRequest(h, handler.WithSuccessCode(op.Code)))
	doc.addOperation("patch", rg.BasePath()+path, op,
		reflect.TypeOf((*T)(nil)).Elem(),
		reflect.TypeOf((*R)(nil)).Elem(),
	)
}

// Action registers a route for ActionFunc (no response body) and adds it to the spec.
func Action[T any](doc *Spec, rg *gin.RouterGroup, method, path string, h handler.ActionFunc[T], op Op) {
	if op.Code == 0 {
		op.Code = http.StatusNoContent
	}
	ginH := handler.HandleAction(h, handler.WithSuccessCode(op.Code))
	switch strings.ToUpper(method) {
	case "POST":
		rg.POST(path, ginH)
	case "PUT":
		rg.PUT(path, ginH)
	case "PATCH":
		rg.PATCH(path, ginH)
	case "DELETE":
		rg.DELETE(path, ginH)
	default:
		rg.Handle(strings.ToUpper(method), path, ginH)
	}
	doc.addOperation(method, rg.BasePath()+path, op,
		reflect.TypeOf((*T)(nil)).Elem(),
		nil,
	)
}

// List registers a GET list route using HandleList and adds it to the spec.
// The response schema is automatically wrapped in ListResult[T].
func List[T any](doc *Spec, rg *gin.RouterGroup, path string, lister handler.QueryLister[T], op Op) {
	if op.Code == 0 {
		op.Code = http.StatusOK
	}
	rg.GET(path, handler.HandleList[T](lister))

	if doc == nil {
		return
	}

	// Build ListResult[T] schema manually.
	itemSchema := schemaFromType(reflect.TypeOf((*T)(nil)).Elem(), "")
	listSchema := &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"items": {Type: "array", Items: itemSchema},
			"total": {Type: "integer"},
		},
	}

	// List has standard query params (page, size, order).
	params := []Parameter{
		{Name: "page", In: "query", Schema: &Schema{Type: "integer"}},
		{Name: "size", In: "query", Schema: &Schema{Type: "integer"}},
		{Name: "order", In: "query", Schema: &Schema{Type: "string"}},
	}

	oaPath := ginPathToOpenAPI(rg.BasePath() + path)
	pi := doc.paths[oaPath]
	if pi == nil {
		pi = &pathItem{}
		doc.paths[oaPath] = pi
	}

	oper := &operation{
		Summary:     op.Summary,
		Description: op.Description,
		Tags:        op.Tags,
		Deprecated:  op.Deprecated,
		Parameters:  params,
		Responses: map[string]*response{
			statusCode(op.Code): {
				Description: http.StatusText(op.Code),
				Content: map[string]*mediaType{
					"application/json": {Schema: listSchema},
				},
			},
		},
	}
	if doc.bearerAuth && !op.Public {
		oper.Security = []map[string][]string{{"BearerAuth": {}}}
	}
	pi.Get = oper
}

// _ ensures handler import is used.
var _ = context.Background

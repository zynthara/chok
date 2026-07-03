package handler

import (
	"reflect"
	"strings"
)

// Meta is the route metadata a constructed handler carries: request /
// response types for schema generation plus the documentation knobs.
// HandleRequest / HandleAction / HandleList attach it to the returned
// http.Handler; the web router type-asserts `interface{ Meta() Meta }`
// (aliased as web.HandlerMeta) at registration time and records it in
// the route table — this replaces v1's unsafe closure-address registry
// wholesale (SPEC §4.2 item 1).
type Meta struct {
	ReqType  reflect.Type // request struct type (nil for HandleList)
	RespType reflect.Type // response type (nil for ActionFunc)
	Code     int          // success HTTP status code
	Summary  string       // user-provided or auto-derived
	Tags     []string     // user-provided or auto-derived
	IsList   bool         // true for HandleList
	Public   bool         // true = no auth required (omits security in OpenAPI)
}

// AutoSummary derives a summary from the HTTP method and path.
//
//	POST /api/v1/posts        → "Create posts"
//	GET  /api/v1/posts        → "List posts"
//	GET  /api/v1/posts/{rid}  → "Get posts"
//	PUT  /api/v1/posts/{rid}  → "Update posts"
//	DELETE /api/v1/posts/{rid} → "Delete posts"
func AutoSummary(method, path string) string {
	resource := extractResource(path)
	var verb string
	switch strings.ToUpper(method) {
	case "POST":
		verb = "Create"
	case "GET":
		if hasParam(path) {
			verb = "Get"
		} else {
			verb = "List"
		}
	case "PUT":
		verb = "Update"
	case "PATCH":
		verb = "Patch"
	case "DELETE":
		verb = "Delete"
	default:
		verb = method
	}
	return verb + " " + resource
}

// AutoTags derives tags from the path — the last non-parameter segment.
//
//	/api/v1/posts        → ["posts"]
//	/api/v1/posts/{rid}  → ["posts"]
func AutoTags(path string) []string {
	resource := extractResource(path)
	if resource == "" {
		return nil
	}
	return []string{resource}
}

func extractResource(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if p := parts[i]; p != "" && !strings.HasPrefix(p, "{") && !strings.HasPrefix(p, "*") && !strings.HasPrefix(p, ":") {
			return p
		}
	}
	return ""
}

// hasParam recognizes both ServeMux ({rid}) and legacy gin (:rid)
// parameter spellings — Auto* helpers may see either during docs
// generation from mixed sources.
func hasParam(path string) bool {
	return strings.Contains(path, "{") || strings.Contains(path, ":")
}

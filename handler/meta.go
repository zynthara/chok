package handler

import (
	"reflect"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// HandlerMeta stores type and documentation metadata for a chok handler.
// Registered automatically by HandleRequest, HandleAction, and HandleList.
type HandlerMeta struct {
	ReqType  reflect.Type // request struct type (nil for HandleList)
	RespType reflect.Type // response type (nil for ActionFunc)
	Code     int          // success HTTP status code
	Summary  string       // user-provided or auto-derived
	Tags     []string     // user-provided or auto-derived
	IsList   bool         // true for HandleList
}

var (
	metaMu    sync.RWMutex
	metaStore = map[uintptr]*HandlerMeta{}
)

func registerMeta(fn gin.HandlerFunc, meta *HandlerMeta) {
	ptr := reflect.ValueOf(fn).Pointer()
	metaMu.Lock()
	metaStore[ptr] = meta
	metaMu.Unlock()
}

// LookupMeta returns the metadata for a gin handler created by
// HandleRequest, HandleAction, or HandleList. Returns nil for
// non-chok handlers (middleware, static files, etc.).
func LookupMeta(fn gin.HandlerFunc) *HandlerMeta {
	ptr := reflect.ValueOf(fn).Pointer()
	metaMu.RLock()
	defer metaMu.RUnlock()
	return metaStore[ptr]
}

// AutoSummary derives a summary from the HTTP method and path.
//
//	POST /api/v1/posts       → "Create posts"
//	GET  /api/v1/posts       → "List posts"
//	GET  /api/v1/posts/:rid  → "Get posts"
//	PUT  /api/v1/posts/:rid  → "Update posts"
//	DELETE /api/v1/posts/:rid → "Delete posts"
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
//	/api/v1/posts       → ["posts"]
//	/api/v1/posts/:rid  → ["posts"]
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
		if !strings.HasPrefix(parts[i], ":") && !strings.HasPrefix(parts[i], "*") {
			return parts[i]
		}
	}
	return ""
}

func hasParam(path string) bool {
	return strings.Contains(path, ":")
}

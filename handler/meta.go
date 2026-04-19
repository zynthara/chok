package handler

import (
	"reflect"
	"strings"
	"sync"
	"unsafe"

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
	Public   bool         // true = no auth required (omits security in OpenAPI)
}

// Why we can't use reflect.Value.Pointer() as the key:
//
// A gin.HandlerFunc is a closure. In Go's ABI, a closure value is a
// pointer to a {codePtr, capture₀, capture₁, …} struct. Two closures
// created from the same source literal — e.g. each call to
// HandleRequest[CreateReq, User] — share the same codePtr but have
// distinct closure-struct pointers. reflect.Value.Pointer returns the
// code pointer, so every route using the same generic instantiation
// would collide in metaStore, and swagger would see a single meta for
// all of them.
//
// Instead we key on the closure-struct address via unsafe: a gin.HandlerFunc
// value is itself a pointer, so *(*unsafe.Pointer)(unsafe.Pointer(&fn))
// yields the closure-struct address. That address is unique per call to
// HandleRequest/HandleAction/HandleList and stable for the closure's
// lifetime (closures are heap-allocated for the captured env).
var (
	metaMu     sync.RWMutex
	metaStore  = map[uintptr]*HandlerMeta{} // closure-struct addr → meta
	routeIndex = map[string]*HandlerMeta{}  // "METHOD PATH" → meta, built by IndexRoutes
)

// closureAddr returns the heap address of a gin.HandlerFunc's closure
// struct. Two distinct closures with different captured environments
// return different addresses even when they share a code pointer. This
// is the stable identity we use for metadata lookup.
func closureAddr(fn gin.HandlerFunc) uintptr {
	if fn == nil {
		return 0
	}
	return uintptr(*(*unsafe.Pointer)(unsafe.Pointer(&fn)))
}

func registerMeta(fn gin.HandlerFunc, meta *HandlerMeta) {
	addr := closureAddr(fn)
	if addr == 0 {
		return
	}
	metaMu.Lock()
	metaStore[addr] = meta
	metaMu.Unlock()
}

// IndexRoutes re-indexes handler metadata by method+path from gin routes.
// Call once after all routes are registered (typically at swagger populate
// time). After this call, LookupRoute is the preferred lookup path, and
// metaStore is pruned to drop closure-addr entries — the map is otherwise
// append-only and would leak in long-running tests that build and discard
// many Apps. Clearing happens after the re-index so nothing referenced by
// routeIndex is lost.
func IndexRoutes(engine *gin.Engine) {
	metaMu.Lock()
	defer metaMu.Unlock()
	for _, route := range engine.Routes() {
		addr := closureAddr(route.HandlerFunc)
		if meta, ok := metaStore[addr]; ok {
			routeIndex[route.Method+" "+route.Path] = meta
		}
	}
	// Drop the closure-keyed map — callers should use LookupRoute now.
	// Orphan entries (from handlers never wired into the engine) are
	// released for GC. LookupMeta becomes a miss, which is documented.
	metaStore = map[uintptr]*HandlerMeta{}
}

// LookupRoute returns metadata by HTTP method and path. Available after
// IndexRoutes has been called. Preferred over LookupMeta for stable
// key identity that does not depend on function pointer addresses.
func LookupRoute(method, path string) *HandlerMeta {
	metaMu.RLock()
	defer metaMu.RUnlock()
	return routeIndex[method+" "+path]
}

// LookupMeta returns the metadata for a gin handler by closure address.
// Retained for backward compatibility; prefer LookupRoute after IndexRoutes.
func LookupMeta(fn gin.HandlerFunc) *HandlerMeta {
	addr := closureAddr(fn)
	metaMu.RLock()
	defer metaMu.RUnlock()
	return metaStore[addr]
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

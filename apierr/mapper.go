package apierr

import (
	"context"
	"sync"
)

// ErrorMapper maps a non-*Error to an *Error. Returns nil if unrecognized.
type ErrorMapper func(error) *Error

var (
	mapperMu sync.RWMutex
	mappers  []ErrorMapper
)

// RegisterMapper registers a global error mapper. Typically called during
// the application startup phase (e.g. inside the Setup callback).
// Panics if m is nil.
//
// Deprecated: Global mappers are shared across all App instances, making
// parallel tests and multi-App scenarios unsafe. Prefer per-App scoped
// mappers via chok.WithErrorMapper(m), which uses MapperRegistry under
// the hood and is resolved first by ResolveWithContext.
//
// Thread-safe: protected by a read-write mutex. Resolve acquires a read lock,
// so concurrent requests are not blocked by each other.
func RegisterMapper(m ErrorMapper) {
	if m == nil {
		panic("apierr: mapper must not be nil")
	}
	mapperMu.Lock()
	defer mapperMu.Unlock()
	mappers = append(mappers, m)
}

// Resolve tries all registered mappers in order. Returns nil if no mapper matches.
func Resolve(err error) *Error {
	mapperMu.RLock()
	defer mapperMu.RUnlock()
	for _, m := range mappers {
		if ae := m(err); ae != nil {
			return ae
		}
	}
	return nil
}

// ResetMappersForTest clears all registered mappers.
// Exported for cross-package test cleanup only — do not use in production code.
func ResetMappersForTest() {
	mapperMu.Lock()
	defer mapperMu.Unlock()
	mappers = nil
}

// --- MapperRegistry (scoped, per-App) ----------------------------------------

// MapperRegistry holds a set of ErrorMappers scoped to an application
// instance. Unlike the global RegisterMapper, MapperRegistry is safe
// for parallel tests and multi-App scenarios — each App gets its own
// registry that doesn't interfere with other instances.
type MapperRegistry struct {
	mu          sync.RWMutex
	mappers     []ErrorMapper
	renderHooks []RenderHook
}

// RenderHook is invoked just before the handler serializes an
// *Error to the response body. Hooks may mutate the *Error in place
// (typically to fill ae.Message via i18n) but MUST NOT replace it.
// Multiple hooks run in registration order; the first one to set
// ae.Message wins by convention (later hooks check before writing).
//
// RenderHooks have access to the request context, so they can read
// the current language, principal, request ID, etc. — anything the
// app stamps onto ctx via middleware.
//
// The handler layer hands hooks a per-response shallow copy of the
// resolved error, never a shared sentinel — mutations affect only the
// response being rendered. Hooks that touch Metadata/Headers must
// replace the map on the copy rather than writing into the shared one.
type RenderHook func(ctx context.Context, ae *Error)

// NewMapperRegistry creates an empty MapperRegistry.
func NewMapperRegistry() *MapperRegistry {
	return &MapperRegistry{}
}

// Register adds a mapper to this registry. Thread-safe.
func (r *MapperRegistry) Register(m ErrorMapper) {
	if m == nil {
		panic("apierr: mapper must not be nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mappers = append(r.mappers, m)
}

// Resolve tries all mappers in this registry. Returns nil if no mapper matches.
func (r *MapperRegistry) Resolve(err error) *Error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.mappers {
		if ae := m(err); ae != nil {
			return ae
		}
	}
	return nil
}

// RegisterRenderHook adds a render hook to this registry. Thread-safe.
// Components that want to mutate every outgoing *Error (i18n message
// localization, ...) register here.
func (r *MapperRegistry) RegisterRenderHook(h RenderHook) {
	if h == nil {
		panic("apierr: render hook must not be nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renderHooks = append(r.renderHooks, h)
}

// Render invokes every registered hook in order. Nil ae is a no-op.
func (r *MapperRegistry) Render(ctx context.Context, ae *Error) {
	if ae == nil {
		return
	}
	r.mu.RLock()
	hooks := append([]RenderHook(nil), r.renderHooks...)
	r.mu.RUnlock()
	for _, h := range hooks {
		h(ctx, ae)
	}
}

// --- Context-scoped mapper resolution ----------------------------------------

type mapperRegistryCtxKey struct{}

// WithMapperRegistry stores a MapperRegistry in ctx. The handler layer
// checks this before falling through to the global Resolve.
func WithMapperRegistry(ctx context.Context, r *MapperRegistry) context.Context {
	return context.WithValue(ctx, mapperRegistryCtxKey{}, r)
}

// MapperRegistryFrom retrieves the MapperRegistry from ctx, or nil.
func MapperRegistryFrom(ctx context.Context) *MapperRegistry {
	r, _ := ctx.Value(mapperRegistryCtxKey{}).(*MapperRegistry)
	return r
}

// ResolveWithContext checks the context-scoped registry first, then
// falls through to the global mappers. This is the preferred entry
// point for the handler layer.
func ResolveWithContext(ctx context.Context, err error) *Error {
	if r := MapperRegistryFrom(ctx); r != nil {
		if ae := r.Resolve(err); ae != nil {
			return ae
		}
	}
	return Resolve(err)
}

// RenderWithContext invokes every render hook registered on the
// context-scoped MapperRegistry. No-op when ctx has no registry
// (e.g. during package-level tests). Handlers call this immediately
// before serializing *Error so hooks can fill in localized fields
// from the request envelope.
func RenderWithContext(ctx context.Context, ae *Error) {
	if r := MapperRegistryFrom(ctx); r != nil {
		r.Render(ctx, ae)
	}
}

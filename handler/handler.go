package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"

	"github.com/zynthara/chok/apierr"
	"github.com/zynthara/chok/internal/ctxval"
	"github.com/zynthara/chok/log"
	"github.com/zynthara/chok/validate"
)

// HandlerFunc is a typed handler that returns a response and error.
type HandlerFunc[T any, R any] func(ctx context.Context, req *T) (R, error)

// ActionFunc is a typed handler that returns only an error (no response body).
type ActionFunc[T any] func(ctx context.Context, req *T) error

// ListResult is the standard paginated response wrapper.
// When page/size are parsed from the query, Page and Size are populated
// so clients can compute total pages without extra logic.
type ListResult[T any] struct {
	Items   []T   `json:"items"`
	Total   int64 `json:"total"`
	Page    int   `json:"page,omitempty"`
	Size    int   `json:"size,omitempty"`
	HasMore bool  `json:"has_more"`
}

// HandleOption controls handler behavior.
type HandleOption func(*handleConfig)

type handleConfig struct {
	successCode  int
	binders      []Binder
	summary      string
	tags         []string
	public       bool
	maxBodyBytes int64 // 0 = use default maxBodySize
}

// WithPublic marks this handler as not requiring authentication.
// In OpenAPI output, the route omits the security requirement so
// clients know no Bearer token is needed (e.g. login, register).
func WithPublic() HandleOption {
	return func(hc *handleConfig) { hc.public = true }
}

// WithSuccessCode overrides the default success HTTP status code.
func WithSuccessCode(code int) HandleOption {
	return func(hc *handleConfig) { hc.successCode = code }
}

// WithSummary sets the OpenAPI summary for this handler (optional).
// If not set, auto-derived from HTTP method + path.
func WithSummary(s string) HandleOption {
	return func(hc *handleConfig) { hc.summary = s }
}

// WithTags sets the OpenAPI tags for this handler (optional).
// If not set, auto-derived from path resource name.
func WithTags(tags ...string) HandleOption {
	return func(hc *handleConfig) { hc.tags = tags }
}

// WithBinders appends additional binders to the default set (uri, query, json).
// Custom binders run after the built-in ones, in the order provided.
func WithBinders(binders ...Binder) HandleOption {
	return func(hc *handleConfig) { hc.binders = append(hc.binders, binders...) }
}

// WithMaxBodySize overrides the default 4 MiB request-body cap for this
// handler. Use for upload endpoints or webhook receivers that legitimately
// accept larger payloads. Non-positive values are ignored (default kept).
func WithMaxBodySize(n int64) HandleOption {
	return func(hc *handleConfig) {
		if n > 0 {
			hc.maxBodyBytes = n
		}
	}
}

// ErrorResponse is the JSON body for error responses.
type ErrorResponse struct {
	Code      int            `json:"code"`
	Reason    string         `json:"reason"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Binder binds a specific source (URI params, query string, JSON body, etc.)
// into struct fields tagged with the corresponding struct tag.
// Bind must NOT run validation — validation runs once after all binders complete.
type Binder interface {
	// Tag returns the struct tag this binder handles (e.g., "uri", "form", "json").
	Tag() string
	// Bind maps values from the request into the target struct.
	Bind(c *gin.Context, target any) error
}

// defaultBinders is the built-in set: URI params, query string, JSON body.
var defaultBinders = []Binder{uriBinder{}, queryBinder{}, jsonBinder{}}

type uriBinder struct{}

func (uriBinder) Tag() string { return "uri" }
func (uriBinder) Bind(c *gin.Context, target any) error {
	m := make(map[string][]string)
	for _, p := range c.Params {
		m[p.Key] = []string{p.Value}
	}
	return binding.MapFormWithTag(target, m, "uri")
}

type queryBinder struct{}

func (queryBinder) Tag() string { return "form" }
func (queryBinder) Bind(c *gin.Context, target any) error {
	return binding.MapFormWithTag(target, c.Request.URL.Query(), "form")
}

type jsonBinder struct{}

func (jsonBinder) Tag() string { return "json" }
func (jsonBinder) Bind(c *gin.Context, target any) error {
	return bindJSON(c, target)
}

// activeBinders inspects struct tags at construction time and returns the
// subset of binders whose tags are present.
// Panics on conflicting tags (same field has multiple source tags).
//
// When T is not a struct (the generic HandleRequest[map[string]any, R]
// pattern is the canonical example), there are no struct tags to scan
// and the request body is overwhelmingly meant to be a JSON payload.
// We return the JSON binder from the supplied set so the body still
// arrives — without this, map/primitive request types would silently
// receive a zero value. Custom binder lists that omit JSON simply get
// an empty result, matching their explicit configuration.
func activeBinders(t reflect.Type, binders []Binder) []Binder {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		// Non-struct T fallback: return the JSON binder if the caller
		// supplied one. This covers HandleRequest[map[string]any, R].
		for _, b := range binders {
			if b.Tag() == "json" {
				return []Binder{b}
			}
		}
		return nil
	}
	present := scanTags(t, binders, make(map[reflect.Type]bool))
	var active []Binder
	for _, b := range binders {
		if present[b.Tag()] {
			active = append(active, b)
		}
	}
	return active
}

// scanTags recursively checks which binder tags appear in the struct's fields.
// seen guards against cyclic struct graphs (e.g. type R struct{ *R; ... })
// so construction never stack-overflows on pathological user types.
func scanTags(t reflect.Type, binders []Binder, seen map[reflect.Type]bool) map[string]bool {
	present := make(map[string]bool)
	if t.Kind() != reflect.Struct || seen[t] {
		return present
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		// Handle embedded structs (including pointer embeds like *Inner).
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for tag, found := range scanTags(ft, binders, seen) {
					if found {
						present[tag] = true
					}
				}
			}
			continue
		}
		var found []string
		for _, b := range binders {
			if f.Tag.Get(b.Tag()) != "" {
				found = append(found, b.Tag())
			}
		}
		if len(found) > 1 {
			panic(fmt.Sprintf("handler: field %q has conflicting source tags (%s)", f.Name, strings.Join(found, "/")))
		}
		if len(found) == 1 {
			present[found[0]] = true
		}
	}
	return present
}

// HandleRequest creates a gin.HandlerFunc with multi-source binding.
// Tag analysis happens once at construction time (Setup phase).
func HandleRequest[T any, R any](h HandlerFunc[T, R], opts ...HandleOption) gin.HandlerFunc {
	cfg := &handleConfig{successCode: http.StatusOK, binders: append([]Binder(nil), defaultBinders...)}
	for _, o := range opts {
		o(cfg)
	}
	active := activeBinders(reflect.TypeOf((*T)(nil)).Elem(), cfg.binders)

	maxBody := cfg.maxBodyBytes
	ginH := func(c *gin.Context) {
		if maxBody > 0 {
			c.Set(maxBodyCtxKey, maxBody)
		}
		req := new(T)
		if err := bindRequest(c, req, active); err != nil {
			WriteResponse(c, 0, nil, toBind(err))
			return
		}
		resp, err := h(c.Request.Context(), req)
		WriteResponse(c, cfg.successCode, resp, err)
	}
	registerMeta(ginH, &HandlerMeta{
		ReqType:  reflect.TypeOf((*T)(nil)).Elem(),
		RespType: reflect.TypeOf((*R)(nil)).Elem(),
		Code:     cfg.successCode,
		Summary:  cfg.summary,
		Tags:     cfg.tags,
		Public:   cfg.public,
	})
	return ginH
}

// HandleAction creates a gin.HandlerFunc for actions (no response body, 204 by default).
func HandleAction[T any](h ActionFunc[T], opts ...HandleOption) gin.HandlerFunc {
	cfg := &handleConfig{successCode: http.StatusNoContent, binders: append([]Binder(nil), defaultBinders...)}
	for _, o := range opts {
		o(cfg)
	}
	active := activeBinders(reflect.TypeOf((*T)(nil)).Elem(), cfg.binders)

	maxBody := cfg.maxBodyBytes
	ginH := func(c *gin.Context) {
		if maxBody > 0 {
			c.Set(maxBodyCtxKey, maxBody)
		}
		req := new(T)
		if err := bindRequest(c, req, active); err != nil {
			WriteResponse(c, 0, nil, toBind(err))
			return
		}
		err := h(c.Request.Context(), req)
		WriteResponse(c, cfg.successCode, nil, err)
	}
	registerMeta(ginH, &HandlerMeta{
		ReqType: reflect.TypeOf((*T)(nil)).Elem(),
		Code:    cfg.successCode,
		Summary: cfg.summary,
		Tags:    cfg.tags,
		Public:  cfg.public,
	})
	return ginH
}

// Validated wraps a HandlerFunc with additional validation functions.
// If a validator returns a plain error (not *apierr.Error), it is automatically
// wrapped as ErrInvalidArgument so it always produces a 4xx response.
func Validated[T any, R any](h HandlerFunc[T, R], validators ...validate.Func[T]) HandlerFunc[T, R] {
	return func(ctx context.Context, req *T) (R, error) {
		for _, v := range validators {
			if err := v(ctx, req); err != nil {
				var zero R
				return zero, wrapValidationError(err)
			}
		}
		return h(ctx, req)
	}
}

// ValidatedAction wraps an ActionFunc with additional validation functions.
// Same error wrapping as Validated.
func ValidatedAction[T any](h ActionFunc[T], validators ...validate.Func[T]) ActionFunc[T] {
	return func(ctx context.Context, req *T) error {
		for _, v := range validators {
			if err := v(ctx, req); err != nil {
				return wrapValidationError(err)
			}
		}
		return h(ctx, req)
	}
}

// wrapValidationError ensures validation errors are client-visible (4xx).
// *apierr.Error is returned as-is (validator chose the error type).
// Context errors (Canceled, DeadlineExceeded) pass through as runtime errors → 500.
// Other plain errors are wrapped as ErrInvalidArgument.
// Note: validators that perform I/O (DB, cache) should return *apierr.Error explicitly
// for runtime failures so they are not misclassified as 400.
func wrapValidationError(err error) error {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return err
	}
	// Runtime/infrastructure errors — let resolveError handle as 500.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return apierr.ErrInvalidArgument.WithMessage(err.Error())
}

// WriteResponse writes a success or error JSON response.
//
// Success: HTTP {code}, body = data.
// Error: HTTP {apierr.Code}, body = ErrorResponse with request_id from ctx.
//
// No-op when the response has already been written (e.g. a timeout
// middleware already wrote 504). Without this guard, a recovered panic
// in a later handler would trigger "http: superfluous response.WriteHeader
// call" warnings and produce a garbled body.
func WriteResponse(c *gin.Context, code int, data any, err error) {
	if c.Writer.Written() {
		return
	}
	if err == nil {
		if data == nil {
			c.Status(code)
			return
		}
		c.JSON(code, data)
		return
	}

	ctx := c.Request.Context()
	ae := resolveError(ctx, err)
	rid := ctxval.RequestIDFrom(ctx)

	// Emit caller-supplied headers (e.g. Retry-After on 429). Headers must
	// be written before c.JSON flushes the response.
	for hk, hv := range ae.Headers {
		c.Header(hk, hv)
	}

	resp := ErrorResponse{
		Code:      ae.Code,
		Reason:    ae.Reason,
		Message:   ae.Message,
		RequestID: rid,
		Metadata:  ae.Metadata,
	}
	c.JSON(ae.Code, resp)
}

// resolveError maps any error to *apierr.Error, logging internal errors.
func resolveError(ctx context.Context, err error) *apierr.Error {
	// *apierr.Error — use directly.
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae
	}

	// validator.ValidationErrors — ErrBind + field details.
	var ve validator.ValidationErrors
	if errors.As(err, &ve) {
		fields := make(map[string]string, len(ve))
		for _, fe := range ve {
			fields[fe.Field()] = fe.Tag()
		}
		return apierr.ErrBind.WithMetadata("fields", fields)
	}

	// Registered mappers (e.g. store.MapError). Checks context-scoped
	// registry first (per-App), then falls through to global mappers.
	if mapped := apierr.ResolveWithContext(ctx, err); mapped != nil {
		return mapped
	}

	// Unknown error — log and return ErrInternal.
	if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok && l != nil {
		l.ErrorContext(ctx, "internal error", "error", err)
	}
	return apierr.ErrInternal
}

// defaulter is implemented by request types that set default values.
// Default() is called after all binders complete and before validation,
// so defaults can satisfy "required" or "min" constraints.
// Unexported: users implement the Default() method; they don't need to
// reference the interface name.
type defaulter interface {
	Default()
}

// bindRequest performs multi-source binding using the active binders.
// Each binder runs without validation; validation runs once at the end
// so that cross-source required fields don't fail prematurely.
// If the request implements Defaulter, Default() is called between
// binding and validation.
//
// validator.v10 panics-without-panicking on non-struct targets (it
// returns InvalidValidationError); skip the call entirely in that
// case so HandleRequest[map[string]any, R] flows through cleanly.
func bindRequest(c *gin.Context, req any, binders []Binder) error {
	for _, b := range binders {
		if err := b.Bind(c, req); err != nil {
			return err
		}
	}
	if d, ok := req.(defaulter); ok {
		d.Default()
	}
	rv := reflect.ValueOf(req)
	for rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	return binding.Validator.ValidateStruct(req)
}

// maxBodySize is the default limit on JSON request body size (4 MB).
// Prevents unbounded memory allocation from oversized payloads. Override
// per handler via WithMaxBodySize — the chosen value is propagated to
// bindJSON through a gin.Context key.
const maxBodySize = 4 << 20

// maxBodyCtxKey is the gin.Context key HandleRequest/HandleAction use to
// publish the effective body-size cap before the binders run. bindJSON
// reads this key; when absent it falls back to maxBodySize.
const maxBodyCtxKey = "chok:max_body_bytes"

// bindJSON decodes JSON body with DisallowUnknownFields.
// Returns ErrBind if Content-Type is not application/json.
func bindJSON(c *gin.Context, obj any) error {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		// No body — skip JSON binding (validation will catch required fields).
		return nil
	}
	ct := c.ContentType()
	if ct != "application/json" {
		if ct == "" {
			return fmt.Errorf("missing Content-Type for JSON binding (expected application/json)")
		}
		return fmt.Errorf("unsupported Content-Type %q for JSON binding", ct)
	}
	limit := int64(maxBodySize)
	if v, ok := c.Get(maxBodyCtxKey); ok {
		if n, ok := v.(int64); ok && n > 0 {
			limit = n
		}
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(obj); err != nil {
		return err
	}
	// Check for trailing data.
	if dec.More() {
		return errors.New("request body contains multiple JSON values")
	}
	// Drain the body so downstream can't re-read.
	_, _ = io.ReadAll(c.Request.Body)
	return nil
}

// toBind converts any binding error to apierr.ErrBind.
func toBind(err error) *apierr.Error {
	if err == nil {
		return nil
	}
	// MaxBytesReader error → 413 Payload Too Large.
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return apierr.New(http.StatusRequestEntityTooLarge, "PayloadTooLarge",
			fmt.Sprintf("request body exceeds %d byte limit", mbe.Limit))
	}
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae
	}
	// Validator errors get field metadata.
	var ve validator.ValidationErrors
	if errors.As(err, &ve) {
		fields := make(map[string]string, len(ve))
		for _, fe := range ve {
			fields[fe.Field()] = fe.Tag()
		}
		return apierr.ErrBind.WithMetadata("fields", fields)
	}

	msg := err.Error()
	// JSON syntax/type errors give useful messages; just wrap.
	var se *json.SyntaxError
	if errors.As(err, &se) {
		msg = fmt.Sprintf("invalid JSON at offset %d", se.Offset)
	}
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		msg = fmt.Sprintf("field %q expects type %s", ute.Field, ute.Type)
	}
	// Unknown fields.
	if strings.HasPrefix(msg, "json: unknown field") {
		msg = strings.TrimPrefix(msg, "json: ")
	}

	return apierr.ErrBind.WithMessage(msg)
}

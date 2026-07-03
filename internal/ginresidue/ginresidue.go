// Package ginresidue carries the minimal gin-typed surface the account
// battery still consumes during the v2 transition (M2 mini-SPEC §11):
// verbatim copies of the v1 handler.WriteResponse / HandleRequest /
// HandleAction pipeline and the v1 middleware.Authn. The web stack
// proper (web/ middleware/ handler/) is gin-free as of M2; account
// imports THIS package (aliased as handler/middleware) until its own
// stdlib rewrite lands in M4 — at which point this package is deleted
// together with the gin dependency.
//
// Do not add new consumers and do not extend the surface: anything a
// migrated module needs lives in the real handler/middleware packages.
package ginresidue

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

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/auth"
	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/log"
)

// HandlerFunc is a typed handler that returns a response and error.
type HandlerFunc[T any, R any] func(ctx context.Context, req *T) (R, error)

// ActionFunc is a typed handler that returns only an error (no response body).
type ActionFunc[T any] func(ctx context.Context, req *T) error

// HandleOption controls handler behavior.
type HandleOption func(*handleConfig)

type handleConfig struct {
	successCode  int
	binders      []Binder
	maxBodyBytes int64
}

// WithSuccessCode overrides the default success HTTP status code.
func WithSuccessCode(code int) HandleOption {
	return func(hc *handleConfig) { hc.successCode = code }
}

// ErrorResponse is the JSON body for error responses.
type ErrorResponse struct {
	Code      int            `json:"code"`
	Reason    string         `json:"reason"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Binder binds a specific source into struct fields (v1 shape).
type Binder interface {
	Tag() string
	Bind(c *gin.Context, target any) error
}

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

func activeBinders(t reflect.Type, binders []Binder) []Binder {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
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
			panic(fmt.Sprintf("ginresidue: field %q has conflicting source tags (%s)", f.Name, strings.Join(found, "/")))
		}
		if len(found) == 1 {
			present[found[0]] = true
		}
	}
	return present
}

// HandleRequest creates a gin.HandlerFunc with multi-source binding
// (v1 pipeline; no swagger metadata — the closure registry is gone).
func HandleRequest[T any, R any](h HandlerFunc[T, R], opts ...HandleOption) gin.HandlerFunc {
	cfg := &handleConfig{successCode: http.StatusOK, binders: append([]Binder(nil), defaultBinders...)}
	for _, o := range opts {
		o(cfg)
	}
	active := activeBinders(reflect.TypeOf((*T)(nil)).Elem(), cfg.binders)

	maxBody := cfg.maxBodyBytes
	return func(c *gin.Context) {
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
}

// HandleAction creates a gin.HandlerFunc for actions (204 by default).
func HandleAction[T any](h ActionFunc[T], opts ...HandleOption) gin.HandlerFunc {
	cfg := &handleConfig{successCode: http.StatusNoContent, binders: append([]Binder(nil), defaultBinders...)}
	for _, o := range opts {
		o(cfg)
	}
	active := activeBinders(reflect.TypeOf((*T)(nil)).Elem(), cfg.binders)

	maxBody := cfg.maxBodyBytes
	return func(c *gin.Context) {
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
}

// WriteResponse writes a success or error JSON response (v1 semantics,
// gin flavour — including the written-guard).
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

func resolveError(ctx context.Context, err error) *apierr.Error {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae
	}
	var ve validator.ValidationErrors
	if errors.As(err, &ve) {
		fields := make(map[string]string, len(ve))
		for _, fe := range ve {
			fields[fe.Field()] = fe.Tag()
		}
		return apierr.ErrBind.WithMetadata("fields", fields)
	}
	if mapped := apierr.ResolveWithContext(ctx, err); mapped != nil {
		return mapped
	}
	if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok && l != nil {
		l.ErrorContext(ctx, "internal error", "error", err)
	}
	return apierr.ErrInternal
}

type defaulter interface {
	Default()
}

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

const maxBodySize = 4 << 20

const maxBodyCtxKey = "chok:max_body_bytes"

func bindJSON(c *gin.Context, obj any) error {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
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
	if dec.More() {
		return errors.New("request body contains multiple JSON values")
	}
	_, _ = io.ReadAll(c.Request.Body)
	return nil
}

func toBind(err error) *apierr.Error {
	if err == nil {
		return nil
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return apierr.New(http.StatusRequestEntityTooLarge, "PayloadTooLarge",
			fmt.Sprintf("request body exceeds %d byte limit", mbe.Limit))
	}
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae
	}
	var ve validator.ValidationErrors
	if errors.As(err, &ve) {
		fields := make(map[string]string, len(ve))
		for _, fe := range ve {
			fields[fe.Field()] = fe.Tag()
		}
		return apierr.ErrBind.WithMetadata("fields", fields)
	}

	msg := err.Error()
	var se *json.SyntaxError
	if errors.As(err, &se) {
		msg = fmt.Sprintf("invalid JSON at offset %d", se.Offset)
	}
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		msg = fmt.Sprintf("field %q expects type %s", ute.Field, ute.Type)
	}
	if strings.HasPrefix(msg, "json: unknown field") {
		msg = strings.TrimPrefix(msg, "json: ")
	}

	return apierr.ErrBind.WithMessage(msg)
}

// --- v1 middleware.Authn (gin flavour) --------------------------------

// TokenParser parses a token string and returns the subject and claims.
type TokenParser interface {
	Parse(token string) (subject string, claims map[string]any, err error)
}

// PrincipalResolver builds a full Principal from the JWT subject and claims.
type PrincipalResolver func(ctx context.Context, subject string, claims map[string]any) (auth.Principal, error)

// Authn creates a Bearer-token authentication middleware (v1 gin form).
func Authn(parser TokenParser, resolver PrincipalResolver) gin.HandlerFunc {
	if parser == nil {
		panic("ginresidue: Authn parser must not be nil")
	}
	return func(c *gin.Context) {
		tokenStr := extractBearer(c.GetHeader("Authorization"))
		if tokenStr == "" {
			WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
			c.Abort()
			return
		}

		subject, claims, err := parser.Parse(tokenStr)
		if err != nil {
			WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
			c.Abort()
			return
		}

		var p auth.Principal
		if resolver != nil {
			p, err = resolver(c.Request.Context(), subject, claims)
			if err != nil {
				WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
				c.Abort()
				return
			}
		} else {
			p = auth.Principal{Subject: subject, Claims: claims}
		}

		ctx := auth.WithPrincipal(c.Request.Context(), p)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

const maxAuthorizationLen = 8 << 10

func extractBearer(header string) string {
	if header == "" {
		return ""
	}
	if len(header) > maxAuthorizationLen {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

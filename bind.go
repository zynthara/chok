package chok

import (
	"encoding"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-playground/validator/v10"

	"github.com/zynthara/chok/internal/jsonx"
)

// source is where a request field's value comes from.
type source int

const (
	fromJSON source = iota
	fromPath
	fromQuery
	fromHeader
)

func (s source) String() string {
	switch s {
	case fromPath:
		return "path"
	case fromQuery:
		return "query"
	case fromHeader:
		return "header"
	}
	return "json"
}

// bindField is a request field filled from the path, the query or a header.
type bindField struct {
	src   source
	name  string
	index []int
	typ   reflect.Type
}

// binder is the per-route plan for one request type, built once.
type binder struct {
	typ      reflect.Type
	none     bool // T is None: nothing to bind
	json     bool // some field (or T itself) comes from the body
	fields   []bindField
	validate bool
}

var noneType = reflect.TypeFor[None]()

// newBinder inspects T. A field with more than one source tag panics: the
// mistake is in the code, and it surfaces at registration.
func newBinder(t reflect.Type) *binder {
	b := &binder{typ: t}
	if t == noneType {
		b.none = true
		return b
	}
	if t.Kind() != reflect.Struct {
		b.json = true
		return b
	}
	b.collect(t, nil)
	b.validate = true
	return b
}

func (b *binder) collect(t reflect.Type, index []int) {
	for i := range t.NumField() {
		f := t.Field(i)
		idx := append(append([]int(nil), index...), i)
		var tags []string
		for _, tag := range []string{"path", "query", "header", "json"} {
			if v, ok := f.Tag.Lookup(tag); ok && v != "-" {
				tags = append(tags, tag)
			}
		}
		if len(tags) > 1 {
			panic(fmt.Sprintf("chok: request field %s.%s has %s tags; a field has one source", t.Name(), f.Name, strings.Join(tags, " and ")))
		}
		if f.Anonymous && len(tags) == 0 {
			ft := f.Type
			if ft.Kind() == reflect.Struct {
				b.collect(ft, idx)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		src := fromJSON
		name := ""
		if len(tags) == 1 {
			name, _, _ = strings.Cut(f.Tag.Get(tags[0]), ",")
			switch tags[0] {
			case "path":
				src = fromPath
			case "query":
				src = fromQuery
			case "header":
				src = fromHeader
			}
		}
		if src == fromJSON {
			if f.Tag.Get("json") != "-" {
				b.json = true
			}
			continue
		}
		if name == "" {
			name = f.Name
		}
		b.fields = append(b.fields, bindField{src: src, name: name, index: idx, typ: f.Type})
	}
}

var (
	errUnsupportedMedia = NewError(http.StatusUnsupportedMediaType, "UnsupportedMediaType", "the request body must be application/json")
	errTooLarge         = NewError(http.StatusRequestEntityTooLarge, "PayloadTooLarge", "the request body is too large")
)

// bind fills req from the request: the JSON body first, then query,
// header and path, which also overwrite anything the body tried to set
// in their fields. Then Default(), then the validate rules.
func (b *binder) bind(w http.ResponseWriter, r *http.Request, req any, maxBody int64) error {
	if b.none {
		return nil
	}
	rv := reflect.ValueOf(req).Elem()
	if b.json {
		if err := decodeBody(w, r, req, maxBody); err != nil {
			return err
		}
	}
	query := r.URL.Query()
	for _, f := range b.fields {
		fv := rv.FieldByIndex(f.index)
		fv.SetZero()
		var vals []string
		switch f.src {
		case fromPath:
			if v := r.PathValue(f.name); v != "" {
				vals = []string{v}
			}
		case fromQuery:
			vals = query[f.name]
		case fromHeader:
			vals = r.Header.Values(f.name)
		}
		if len(vals) == 0 {
			continue
		}
		if err := setText(fv, vals); err != nil {
			return ErrBadRequest.Msgf("%s %s: %v", f.src, f.name, err).With("field", f.name)
		}
	}
	if d, ok := req.(interface{ Default() }); ok {
		d.Default()
	}
	if b.validate {
		if err := requestValidator().Struct(req); err != nil {
			if ve, ok := errors.AsType[validator.ValidationErrors](err); ok {
				return validationError(ve, nil)
			}
			return ErrBadRequest.Wrap(err)
		}
	}
	return nil
}

func decodeBody(w http.ResponseWriter, r *http.Request, req any, maxBody int64) error {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return nil
	}
	body := r.Body
	if maxBody > 0 {
		body = http.MaxBytesReader(w, r.Body, maxBody)
	}
	// A chunked body of unknown length may still be empty.
	data, err := io.ReadAll(body)
	if err != nil {
		if mbe, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return errTooLarge.Msgf("the request body exceeds %d bytes", mbe.Limit).With("limit", mbe.Limit)
		}
		return ErrBadRequest.Msgf("reading the request body: %v", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		return errUnsupportedMedia
	}
	if err := json.Unmarshal(data, req, jsonx.Options, json.RejectUnknownMembers(true)); err != nil {
		return jsonError(err)
	}
	return nil
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// jsonError names the offending member when json/v2 can.
func jsonError(err error) error {
	se, ok := errors.AsType[*json.SemanticError](err)
	if !ok {
		return ErrBadRequest.Msgf("invalid JSON body: %s", strings.TrimPrefix(err.Error(), "jsontext: ")).Wrap(err)
	}
	field := strings.ReplaceAll(strings.TrimPrefix(string(se.JSONPointer), "/"), "/", ".")
	var e *Error
	switch {
	case errors.Is(err, json.ErrUnknownName):
		e = ErrBadRequest.Msgf("unknown field %q", field)
	case field == "":
		e = ErrBadRequest.Msgf("the body cannot be a JSON %s", se.JSONKind)
	default:
		e = ErrBadRequest.Msgf("field %q: cannot use a JSON %s as %s", field, se.JSONKind, se.GoType)
	}
	if field != "" {
		e = e.With("field", field)
	}
	return e.Wrap(err)
}

var (
	timeType = reflect.TypeFor[time.Time]()
	durType  = reflect.TypeFor[time.Duration]()
	tuType   = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// setText assigns path, query or header text to a field: scalars take the
// first value, slices every value.
func setText(v reflect.Value, vals []string) error {
	t := v.Type()
	if t.Kind() == reflect.Pointer {
		p := reflect.New(t.Elem())
		if err := setText(p.Elem(), vals); err != nil {
			return err
		}
		v.Set(p)
		return nil
	}
	if t.Kind() == reflect.Slice && t != reflect.TypeFor[[]byte]() && !reflect.PointerTo(t).Implements(tuType) {
		out := reflect.MakeSlice(t, 0, len(vals))
		for _, s := range vals {
			for part := range strings.SplitSeq(s, ",") {
				ev := reflect.New(t.Elem()).Elem()
				if err := setScalar(ev, strings.TrimSpace(part)); err != nil {
					return err
				}
				out = reflect.Append(out, ev)
			}
		}
		v.Set(out)
		return nil
	}
	return setScalar(v, vals[0])
}

func setScalar(v reflect.Value, s string) error {
	t := v.Type()
	switch {
	case t == durType:
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%q is not a duration", s)
		}
		v.SetInt(int64(d))
		return nil
	case t == timeType:
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("%q is not an RFC 3339 time", s)
		}
		v.Set(reflect.ValueOf(tm))
		return nil
	case reflect.PointerTo(t).Implements(tuType):
		return v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s))
	}
	switch t.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("%q is not true or false", s)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, t.Bits())
		if err != nil {
			return fmt.Errorf("%q is not an integer", s)
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, t.Bits())
		if err != nil {
			return fmt.Errorf("%q is not a non-negative integer", s)
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, t.Bits())
		if err != nil {
			return fmt.Errorf("%q is not a number", s)
		}
		v.SetFloat(f)
	default:
		return fmt.Errorf("cannot bind into %s", t)
	}
	return nil
}

// requestValidator checks the validate tags and names fields by their
// source: json name, or path, query or header name.
var requestValidator = sync.OnceValue(func() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		for _, tag := range []string{"json", "path", "query", "header"} {
			if name, ok := f.Tag.Lookup(tag); ok {
				name, _, _ = strings.Cut(name, ",")
				if name == "-" {
					return ""
				}
				if name != "" {
					return name
				}
			}
		}
		return f.Name
	})
	return v
})

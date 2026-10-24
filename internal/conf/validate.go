package conf

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"
)

var validate = sync.OnceValue(func() *validator.Validate {
	return validator.New(validator.WithRequiredStructEnabled())
})

// Validator is implemented by configuration structs that check
// themselves beyond what validate tags express.
type Validator interface{ Validate() error }

// validateTree runs the validate tags over the whole value, then every
// Validate method, children before parents.
func (d *decoder) validateTree(v reflect.Value) {
	if err := validate().Struct(v.Addr().Interface()); err != nil {
		if ve, ok := errors.AsType[validator.ValidationErrors](err); ok {
			for _, fe := range ve {
				d.problems = append(d.problems, yamlPath(v.Type(), fe.StructNamespace())+": "+describeRule(fe))
			}
		}
	}
	d.callValidate("", v)
}

func (d *decoder) callValidate(path string, v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			d.callValidate(path, v.Elem())
		}
		return
	case reflect.Struct:
		if isScalar(v.Type()) {
			return
		}
		for _, f := range fieldsOf(v.Type()) {
			d.callValidate(join(path, f.key), v.FieldByIndex(f.index))
		}
	case reflect.Slice:
		for i := range v.Len() {
			d.callValidate(fmt.Sprintf("%s[%d]", path, i), v.Index(i))
		}
		return
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			ev := reflect.New(v.Type().Elem()).Elem()
			ev.Set(iter.Value())
			d.callValidate(join(path, iter.Key().String()), ev)
		}
		return
	default:
		return
	}
	var target any
	if v.CanAddr() {
		target = v.Addr().Interface()
	} else {
		target = v.Interface()
	}
	if val, ok := target.(Validator); ok {
		if err := val.Validate(); err != nil {
			if path == "" {
				d.problems = append(d.problems, err.Error())
			} else {
				d.problems = append(d.problems, path+": "+err.Error())
			}
		}
	}
}

// yamlPath turns a validator struct namespace such as
// "Config.Base.HTTP.Addr" or "Config.Log.Files[0].Path" into the key path
// an operator writes: http.addr, log.files[0].path.
func yamlPath(root reflect.Type, ns string) string {
	segs := strings.Split(ns, ".")[1:]
	t := root
	var out strings.Builder
	for _, seg := range segs {
		name, sub, _ := strings.Cut(seg, "[")
		t = deref(t)
		if t.Kind() != reflect.Struct {
			break
		}
		sf, ok := t.FieldByName(name)
		if !ok {
			break
		}
		t = sf.Type
		yname, inline := parseYAMLTag(sf.Tag.Get("yaml"))
		if (sf.Anonymous && yname == "") || inline {
			continue
		}
		if yname == "" {
			yname = strings.ToLower(sf.Name)
		}
		if out.Len() > 0 {
			out.WriteByte('.')
		}
		out.WriteString(yname)
		if sub != "" {
			out.WriteString("[" + sub)
			t = deref(t).Elem()
		}
	}
	return out.String()
}

func describeRule(fe validator.FieldError) string {
	got := ""
	if fe.Value() != nil {
		if s := fmt.Sprint(fe.Value()); s != "" {
			got = " (got " + strconv.Quote(s) + ")"
		}
	}
	switch fe.Tag() {
	case "required":
		return "is required"
	case "oneof":
		return "must be one of " + fe.Param() + got
	case "min", "gte":
		return "must be at least " + fe.Param() + got
	case "max", "lte":
		return "must be at most " + fe.Param() + got
	case "gt":
		return "must be greater than " + fe.Param() + got
	case "lt":
		return "must be less than " + fe.Param() + got
	case "url", "http_url":
		return "must be a URL" + got
	case "email":
		return "must be an email address" + got
	}
	if fe.Param() != "" {
		return "must satisfy " + fe.Tag() + "=" + fe.Param() + got
	}
	return "must satisfy " + fe.Tag() + got
}

// Package conf loads the typed configuration behind chok.Load.
//
// Sources, lowest to highest: `default` tags, the YAML file, environment
// variables named after the key path ({PREFIX}_HTTP_ADDR), then flags
// named after it (--http.addr). Decoding is strict: an unknown key or
// section is an error with a suggestion, a value of the wrong shape is an
// error naming its path. Validation runs the `validate` tags and then
// every Validate method. All problems are reported together.
package conf

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/spf13/pflag"
)

// Options controls one Load.
type Options struct {
	// Name is the application name: it derives the environment prefix
	// and the file candidates {name}.yaml and configs/{name}.yaml.
	Name string
	// EnvPrefix overrides the prefix derived from Name.
	EnvPrefix string
	// Override is the file named on the command line (--config); it
	// beats {PREFIX}_CONFIG and File. A missing one is an error.
	Override string
	// File is the file the code names (ConfigFile); a missing one is an
	// error.
	File string
	// Flags is a parsed flag set; changed flags named after a key path win.
	Flags *pflag.FlagSet
	// Environ is the environment, os.Environ() unless a test sets it.
	Environ []string
}

// Result reports what Load read.
type Result struct {
	// File is the configuration file read, "" when none was found.
	File string
	// Warnings lists environment variables with the prefix that match no key.
	Warnings []string
}

// Error is a failed Load with every problem found.
type Error struct {
	File     string
	Problems []string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("config")
	if e.File != "" {
		b.WriteString(" " + e.File)
	}
	if len(e.Problems) == 1 {
		b.WriteString(": " + e.Problems[0])
		return b.String()
	}
	fmt.Fprintf(&b, ": %d problems:", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - " + p)
	}
	return b.String()
}

// Prefix is the environment variable prefix derived from an app name.
func Prefix(name string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

// Load fills the struct dst points to.
func Load(dst any, o Options) (Result, error) {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return Result{}, fmt.Errorf("config: the type must be a struct, got %T", dst)
	}
	prefix := o.EnvPrefix
	if prefix == "" {
		prefix = Prefix(o.Name)
	}
	prefix = strings.ToUpper(prefix)
	environ := o.Environ
	if environ == nil {
		environ = os.Environ()
	}

	var res Result
	d := &decoder{lines: map[string]int{}}
	tree := map[string]any{}

	path, explicit := locate(o, prefix, environ)
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			res.File = path
			ft, err := parseYAML(data, d.lines)
			if err != nil {
				return res, &Error{File: path, Problems: []string{err.Error()}}
			}
			merge(tree, ft)
		case explicit || !errors.Is(err, fs.ErrNotExist):
			return res, &Error{File: path, Problems: []string{err.Error()}}
		}
	}

	t := v.Elem().Type()
	paths := map[string]string{}
	leaves(t, prefix, "", tree, paths)
	envTree, warnings := envLayer(paths, prefix, environ)
	res.Warnings = warnings
	merge(tree, envTree)
	merge(tree, flagLayer(o.Flags, paths))

	out := reflect.New(t).Elem()
	d.decode("", tree, out)
	if len(d.problems) == 0 {
		d.validateTree(out)
	}
	if len(d.problems) > 0 {
		return res, &Error{File: res.File, Problems: dedupe(d.problems)}
	}
	v.Elem().Set(out)
	return res, nil
}

// Defaults sets every field of the struct dst points to that has a
// `default` tag, at any depth; fields without one keep their value.
func Defaults(dst any) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config: Defaults needs a pointer to a struct, got %T", dst)
	}
	d := &decoder{}
	d.decode("", map[string]any{}, v.Elem())
	if len(d.problems) > 0 {
		return &Error{Problems: d.problems}
	}
	return nil
}

// Check runs the validate tags and Validate methods on the struct v points
// to, as Load does, for configuration built in code.
func Check(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config: Check needs a pointer to a struct, got %T", v)
	}
	d := &decoder{}
	d.validateTree(rv.Elem())
	if len(d.problems) > 0 {
		return &Error{Problems: dedupe(d.problems)}
	}
	return nil
}

// locate picks the file: --config, then {PREFIX}_CONFIG, then the code's,
// then the first of chok.yaml, {name}.yaml and configs/{name}.yaml that
// exists.
func locate(o Options, prefix string, environ []string) (path string, explicit bool) {
	if o.Override != "" {
		return o.Override, true
	}
	for _, kv := range environ {
		if name, val, ok := strings.Cut(kv, "="); ok && name == prefix+"_CONFIG" && val != "" {
			return val, true
		}
	}
	if o.File != "" {
		return o.File, true
	}
	candidates := []string{"chok.yaml"}
	if o.Name != "" {
		candidates = append(candidates, o.Name+".yaml", "configs/"+o.Name+".yaml")
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, false
		}
	}
	return "", false
}

// Field finds the field at a dotted key path in the struct cfg points to.
func Field(cfg any, path string) (reflect.Value, bool) {
	v := reflect.ValueOf(cfg)
	for key := range strings.SplitSeq(path, ".") {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		found := false
		for _, f := range fieldsOf(v.Type()) {
			if f.key == key {
				v, found = v.FieldByIndex(f.index), true
				break
			}
		}
		if !found {
			return reflect.Value{}, false
		}
	}
	return v, true
}

func dedupe(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

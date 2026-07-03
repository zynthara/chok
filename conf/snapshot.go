package conf

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/go-viper/mapstructure/v2"
)

// Snapshot is one immutable configuration state. Section decodes are
// cached per key; the underlying tree is never mutated after build.
// Callers must treat decoded values as read-only.
type Snapshot struct {
	settings map[string]any // frozen viper.AllSettings() tree
	loader   *Loader

	mu    sync.Mutex
	cache map[string]reflect.Value // section key → decoded struct value
}

func newSnapshot(settings map[string]any, l *Loader) *Snapshot {
	return &Snapshot{
		settings: settings,
		loader:   l,
		cache:    make(map[string]reflect.Value),
	}
}

// Has reports whether the dotted key exists in the tree (defaults,
// file, env-bound and flag values all count).
func (s *Snapshot) Has(key string) bool {
	_, ok := treeGet(s.settings, strings.ToLower(key))
	return ok
}

// Value returns the raw tree value at the dotted key.
func (s *Snapshot) Value(key string) (any, bool) {
	return treeGet(s.settings, strings.ToLower(key))
}

// Section decodes the subtree at key into out (a non-nil pointer to a
// struct). Registered sections hit the load-time cache; unregistered
// keys decode ad hoc with the same (viper-compatible) decoder.
func (s *Snapshot) Section(key string, out any) error {
	key = strings.ToLower(key)
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("conf: Section(%q) needs a non-nil pointer, got %T", key, out)
	}
	if t, registered := s.loader.SectionType(key); registered && rv.Elem().Type() == t {
		val, err := s.decodeRegistered(key)
		if err != nil {
			return err
		}
		rv.Elem().Set(val)
		return nil
	}
	return s.decodeInto(key, out)
}

// EnabledFor implements the kernel's component-switch rule
// (mini-SPEC §3.1): the merged tree's <key>.enabled value when
// present (defaults included), true when absent. Assembly is intent —
// yaml `enabled: false` is the kill switch.
func (s *Snapshot) EnabledFor(key string) bool {
	if key == "" {
		return true
	}
	raw, ok := treeGet(s.settings, strings.ToLower(key)+".enabled")
	if !ok {
		return true
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return true // unparseable switch: fail open, load-time validation owns the error
		}
		return b
	default:
		return true
	}
}

// decodeRegistered returns the cached decode of a registered section,
// decoding on first use. The returned value is the cached original —
// callers copy (Section does) or treat as read-only.
func (s *Snapshot) decodeRegistered(key string) (reflect.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.cache[key]; ok {
		return v, nil
	}
	t, ok := s.loader.SectionType(key)
	if !ok {
		return reflect.Value{}, fmt.Errorf("conf: section %q not registered", key)
	}
	ptr := reflect.New(t)
	if err := s.decodeInto(key, ptr.Interface()); err != nil {
		return reflect.Value{}, err
	}
	val := ptr.Elem()
	s.cache[key] = val
	return val, nil
}

// decodeInto decodes the subtree at key into out using the same hook
// stack viper.Unmarshal applies, plus weak typing so env-sourced
// strings coerce into ints/bools.
func (s *Snapshot) decodeInto(key string, out any) error {
	sub, ok := treeGet(s.settings, key)
	if !ok || sub == nil {
		sub = map[string]any{} // absent section decodes to defaults/zero
	}
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           out,
		TagName:          "mapstructure",
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	})
	if err != nil {
		return err
	}
	return dec.Decode(sub)
}

// treeGet resolves a lowercase dotted path in nested map[string]any.
func treeGet(tree map[string]any, key string) (any, bool) {
	parts := strings.Split(key, ".")
	var cur any = tree
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Package conf is the chok v2 configuration engine: a viper-backed
// loader (file + env + flag, v1-compatible precedence) that produces
// immutable snapshots published via RCU. Nothing hands out live
// pointers; a reload builds a whole new tree, validates it, and
// atomically swaps — "failure pollutes nothing" is structural
// (SPEC §3.4).
package conf

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Validatable is implemented by Options types that can check
// themselves. The loader validates every registered section
// recursively at load and reload time; errors abort the swap.
type Validatable interface {
	Validate() error
}

// SelfValidating marks a Validatable whose Validate() already covers
// its nested fields, so the recursive walker must not descend into it.
// Discriminator configs (one-of-N branches picked by a selector field)
// use this to keep unselected branches out of validation. The marker
// method is exported — unlike v1 — so modules outside this package can
// implement it.
type SelfValidating interface {
	Validatable
	IsSelfValidating()
}

// ReservedInstancesKey is the framework-owned subkey inside every
// component section that hosts named-instance sections
// (<key>.instances.<name>, mini-SPEC §1). Registered section types
// must not declare a field with this mapstructure name.
const ReservedInstancesKey = "instances"

// Loader assembles the full configuration pipeline. Register every
// typed section before Load; the section set is what drives default
// registration, env binding and validation — exactly the three jobs
// v1 derived from the user's mega config struct.
type Loader struct {
	appName      string
	envPrefix    string
	explicitPath string
	flags        *pflag.FlagSet
	sections     map[string]reflect.Type // lowercase dotted key → struct type
}

// NewLoader creates a Loader for the named app. envPrefix follows the
// v1 convention (upper-cased app name unless overridden).
func NewLoader(appName, envPrefix string) *Loader {
	return &Loader{
		appName:   appName,
		envPrefix: envPrefix,
		sections:  make(map[string]reflect.Type),
	}
}

// SetPath pins an explicit config file. A missing explicit file is a
// load error; auto-detected paths are optional.
func (l *Loader) SetPath(p string) { l.explicitPath = p }

// SetFlags binds a pflag set as the highest-priority source.
func (l *Loader) SetFlags(fs *pflag.FlagSet) { l.flags = fs }

// Register declares a typed section. sample must be a struct or
// *struct of the section's Options type. Duplicate keys and types
// that claim the reserved "instances" subkey fail fast.
func (l *Loader) Register(key string, sample any) error {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return errors.New("conf: section key must be non-empty")
	}
	t := reflect.TypeOf(sample)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return fmt.Errorf("conf: section %q must register a struct type, got %T", key, sample)
	}
	if prev, dup := l.sections[key]; dup {
		if prev == t {
			return nil // same type re-registered: idempotent
		}
		return fmt.Errorf("conf: section %q already registered with type %s (attempted %s)", key, prev, t)
	}
	if f, claimed := fieldByMapKey(t, ReservedInstancesKey); claimed {
		return fmt.Errorf("conf: section %q type %s declares reserved field %q (mapstructure %q) — the instances namespace is framework-owned", key, t, f, ReservedInstancesKey)
	}
	l.sections[key] = t
	return nil
}

// SectionKeys returns the registered keys, sorted (deterministic
// dispatch order for diff reporting and tests).
func (l *Loader) SectionKeys() []string {
	keys := make([]string, 0, len(l.sections))
	for k := range l.sections {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// SectionType returns the registered struct type for key.
func (l *Loader) SectionType(key string) (reflect.Type, bool) {
	t, ok := l.sections[strings.ToLower(key)]
	return t, ok
}

// Load runs the full pipeline and returns the first snapshot plus the
// resolved config path ("" when nothing was found to read).
func (l *Loader) Load() (*Snapshot, string, error) {
	return l.build()
}

// build is shared by Load and Store.Reload: fresh viper, full source
// stack, decode + validate every registered section, freeze the tree.
func (l *Loader) build() (*Snapshot, string, error) {
	v := viper.New()

	// 1. Defaults from `default` tags, per registered section.
	for key, t := range l.sections {
		registerDefaults(v, key, t)
	}

	// 2. Config file (explicit missing = error; detected missing = skip).
	path, explicit := l.resolveConfigPath()
	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			if explicit || !os.IsNotExist(err) {
				return nil, "", fmt.Errorf("conf: read config %s: %w", path, err)
			}
			path = "" // default lookup found nothing — env/flags only
		}
	}

	// 3. Env binding: every static leaf of every registered section.
	v.SetEnvPrefix(l.envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	for key, t := range l.sections {
		bindEnvs(v, key, t)
	}
	v.AutomaticEnv()

	// 4. Flags (highest priority).
	if l.flags != nil {
		if err := v.BindPFlags(l.flags); err != nil {
			return nil, "", fmt.Errorf("conf: bind flags: %w", err)
		}
	}

	// 5. Imperative env pass for dynamic map keys (yaml-discovered
	// paths the static walker can't know, e.g. account.providers.*).
	// Same mechanism as v1: for every known key, if the derived env
	// var is set, v.Set it so AllSettings sees the override.
	bindMapEnvOverrides(v, l.envPrefix)

	// 6. Freeze the tree.
	settings := v.AllSettings()

	snap := newSnapshot(settings, l)

	// 7. Decode + validate every registered section against the new
	// tree. Nothing is published until all of this passes.
	var errs []error
	for _, key := range l.SectionKeys() {
		val, err := snap.decodeRegistered(key)
		if err != nil {
			errs = append(errs, fmt.Errorf("conf: decode section %q: %w", key, err))
			continue
		}
		if err := validateTree(val, key); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, "", err
	}
	return snap, path, nil
}

// resolveConfigPath mirrors v1: explicit path → {PREFIX}_CONFIG env →
// ./{name}.yaml → ./configs/{name}.yaml. Returns (path, explicit);
// explicit=true means a missing file is an error.
func (l *Loader) resolveConfigPath() (string, bool) {
	if l.explicitPath != "" {
		return l.explicitPath, true
	}
	if ep := os.Getenv(l.envPrefix + "_CONFIG"); ep != "" {
		return ep, true
	}
	primary := "./" + l.appName + ".yaml"
	if _, err := os.Stat(primary); err == nil {
		return primary, false
	}
	alt := "./configs/" + l.appName + ".yaml"
	if _, err := os.Stat(alt); err == nil {
		return alt, false
	}
	return primary, false
}

// Store is the RCU holder: an atomic pointer to the current immutable
// Snapshot. Readers never block; Reload swaps wholesale or not at all.
type Store struct {
	loader *Loader
	cur    atomic.Pointer[Snapshot]
	path   atomic.Pointer[string] // resolved config path (for watchers)
}

// NewStore loads the initial snapshot and returns the store.
func NewStore(l *Loader) (*Store, error) {
	snap, path, err := l.Load()
	if err != nil {
		return nil, err
	}
	s := &Store{loader: l}
	s.cur.Store(snap)
	s.path.Store(&path)
	return s, nil
}

// Snapshot returns the current immutable snapshot.
func (s *Store) Snapshot() *Snapshot { return s.cur.Load() }

// Path returns the config file path resolved at the last successful
// load ("" when no file was read).
func (s *Store) Path() string { return *s.path.Load() }

// Reload rebuilds from all sources, validates, atomically swaps and
// returns the per-section diff against the previous snapshot. On any
// error the current snapshot stays published untouched.
func (s *Store) Reload() (*Diff, error) {
	fresh, path, err := s.loader.build()
	if err != nil {
		return nil, err
	}
	old := s.cur.Load()
	diff := diffSnapshots(old, fresh, s.loader)
	s.cur.Store(fresh)
	s.path.Store(&path)
	return diff, nil
}

// --- struct walking helpers (ported from v1 config.go) ---------------

// mapKeyOf resolves the effective mapstructure key of a struct field.
func mapKeyOf(f reflect.StructField) string {
	key := f.Tag.Get("mapstructure")
	if comma := strings.IndexByte(key, ','); comma >= 0 {
		key = key[:comma]
	}
	if key == "" {
		key = strings.ToLower(f.Name)
	}
	return key
}

// fieldByMapKey reports whether t declares a top-level field with the
// given mapstructure key, returning the Go field name.
func fieldByMapKey(t reflect.Type, key string) (string, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if mapKeyOf(f) == key {
			return f.Name, true
		}
	}
	return "", false
}

// registerDefaults walks the section type and registers `default` tags
// as viper defaults under the section prefix.
func registerDefaults(v *viper.Viper, prefix string, t reflect.Type) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		key := mapKeyOf(field)
		if prefix != "" {
			key = prefix + "." + key
		}
		ft := field.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && !isAtomicStruct(ft) {
			registerDefaults(v, key, ft)
			continue
		}
		if def := field.Tag.Get("default"); def != "" {
			v.SetDefault(key, def)
		}
	}
}

// bindEnvs binds env vars for every static leaf. Maps are skipped —
// their dynamic keys are handled by bindMapEnvOverrides after the
// yaml is loaded (same division of labour as v1).
func bindEnvs(v *viper.Viper, prefix string, t reflect.Type) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		key := mapKeyOf(field)
		if prefix != "" {
			key = prefix + "." + key
		}
		ft := field.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && !isAtomicStruct(ft) {
			bindEnvs(v, key, ft)
			continue
		}
		if ft.Kind() == reflect.Map {
			continue
		}
		_ = v.BindEnv(key)
	}
}

// bindMapEnvOverrides is the imperative pass closing env coverage for
// dynamic keys: every key viper discovered (mostly from yaml) gets its
// derived env name checked; set values are written back so Unmarshal
// (which reads AllSettings) sees the override win.
func bindMapEnvOverrides(v *viper.Viper, envPrefix string) {
	prefix := strings.ToUpper(envPrefix)
	for _, k := range v.AllKeys() {
		envKey := strings.ToUpper(strings.ReplaceAll(k, ".", "_"))
		if prefix != "" {
			envKey = prefix + "_" + envKey
		}
		if val, ok := os.LookupEnv(envKey); ok {
			v.Set(k, val)
		}
	}
}

// isAtomicStruct reports struct types treated as scalar leaves
// (no recursion, whole-value comparison): time.Time and friends.
func isAtomicStruct(t reflect.Type) bool {
	switch t.String() {
	case "time.Time":
		return true
	}
	// Types with no exported fields carry no addressable config shape.
	for i := range t.NumField() {
		if t.Field(i).IsExported() {
			return false
		}
	}
	return true
}

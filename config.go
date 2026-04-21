package chok

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/zynthara/chok/config"
)

// loadConfig loads configuration: default tags → file → env → unmarshal → validate.
func (a *App) loadConfig() error {
	if a.configPtr == nil {
		return nil
	}

	v := viper.New()

	// Register default tags from the typed config struct.
	registerDefaults(v, "", reflect.TypeOf(a.configPtr))

	// Determine config path.
	path, explicit := a.resolveConfigPath()

	if path != "" {
		// Persist the resolved path so WithConfigWatch and Reload find
		// the right file even when the user didn't pass WithConfig(&cfg, path)
		// and instead relied on {PREFIX}_CONFIG or a default lookup.
		a.configPath = path

		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			if explicit {
				return fmt.Errorf("chok: read config %s: %w", path, err)
			}
			// Default path not found — skip silently.
			if !os.IsNotExist(err) {
				return fmt.Errorf("chok: read config %s: %w", path, err)
			}
		}
	}

	// Bind environment variables.
	v.SetEnvPrefix(a.envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	bindEnvs(v, "", reflect.TypeOf(a.configPtr))
	v.AutomaticEnv()

	// Bind CLI flags (highest priority).
	if fs, ok := a.flagSet.(*pflag.FlagSet); ok && fs != nil {
		if err := v.BindPFlags(fs); err != nil {
			return fmt.Errorf("chok: bind flags: %w", err)
		}
	}

	// Unmarshal into the typed config struct.
	if err := v.Unmarshal(a.configPtr); err != nil {
		return fmt.Errorf("chok: unmarshal config: %w", err)
	}

	// Reject pointer-typed Options fields in the config struct. Pointer
	// fields break the immutable reload contract: reflect.Value.Set
	// replaces the struct content in-place, but a resolver caching a
	// *PointerField would still hold the old pointer after reload.
	// Value-embedded fields are safe because their address is stable.
	if err := validateNoPointerOptions(reflect.ValueOf(a.configPtr)); err != nil {
		return fmt.Errorf("chok: %w", err)
	}

	// Validate sub-Options, then root config.
	return a.validateConfig()
}

// reloadConfigImmutable performs a two-phase config reload:
//  1. Allocate a fresh config struct of the same type as a.configPtr.
//  2. Load file + env + flag + defaults into the fresh struct.
//  3. Validate the fresh struct.
//  4. If all OK, shallow-copy the struct value into a.configPtr.
//
// This ensures the live config is never partially updated.
// reloadConfigImmutable returns (changed, changedSections, error).
// changed is true when the freshly-loaded config differs from the live config.
// changedSections maps top-level mapstructure keys to whether they changed.
func (a *App) reloadConfigImmutable() (bool, map[string]bool, error) {
	// Create a zero-value copy of the config type.
	origVal := reflect.ValueOf(a.configPtr) // *Config
	if origVal.Kind() != reflect.Ptr {
		return false, nil, fmt.Errorf("chok: configPtr must be a pointer, got %T", a.configPtr)
	}
	elemType := origVal.Elem().Type() // Config
	freshPtr := reflect.New(elemType) // *Config (fresh zero)
	freshCfg := freshPtr.Interface()  // any(*Config)

	v := viper.New()
	registerDefaults(v, "", reflect.TypeOf(freshCfg))

	path, explicit := a.resolveConfigPath()
	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			if explicit {
				return false, nil, fmt.Errorf("chok: reload config %s: %w", path, err)
			}
			if !os.IsNotExist(err) {
				return false, nil, fmt.Errorf("chok: reload config %s: %w", path, err)
			}
		}
	}

	v.SetEnvPrefix(a.envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	bindEnvs(v, "", reflect.TypeOf(freshCfg))
	v.AutomaticEnv()

	if fs, ok := a.flagSet.(*pflag.FlagSet); ok && fs != nil {
		if err := v.BindPFlags(fs); err != nil {
			return false, nil, fmt.Errorf("chok: reload bind flags: %w", err)
		}
	}

	if err := v.Unmarshal(freshCfg); err != nil {
		return false, nil, fmt.Errorf("chok: reload unmarshal config: %w", err)
	}

	// Validate on the fresh copy — live config untouched so far.
	if err := a.validateConfigPtr(freshCfg, freshPtr.Elem()); err != nil {
		return false, nil, err
	}

	// Phase 2: atomic swap — detect diff and copy the validated struct value
	// into the live config under a write lock. This prevents concurrent
	// readers (HTTP handlers, health probes) from observing a partially-written
	// struct during the multi-field copy. The diff is computed inside the lock
	// so the live config is stable during comparison.
	a.configMu.Lock()
	changed := !reflect.DeepEqual(origVal.Elem().Interface(), freshPtr.Elem().Interface())
	sections := diffConfigSections(origVal.Elem(), freshPtr.Elem())
	origVal.Elem().Set(freshPtr.Elem())
	// Publish an atomic snapshot before releasing the lock so readers
	// using Kernel.ConfigSnapshot() never observe the intermediate
	// state between Set() and the snapshot swap. Users that read the
	// live configPtr directly still need an external guarantee that
	// their reads happen under ConfigSnapshotRLock() or happen outside
	// reload windows — the framework contract is that ConfigSnapshot()
	// is the only safe multi-field read.
	if reg := a.Registry(); reg != nil {
		reg.PublishConfigSnapshot()
	}
	a.configMu.Unlock()
	return changed, sections, nil
}

// ConfigSnapshotRLock exposes the reader side of the config mutex so
// callers that absolutely must read live config fields directly (rather
// than via Kernel.ConfigSnapshot) can serialize with reload writes.
// Prefer ConfigSnapshot for new code — this is an escape hatch for
// legacy integrations.
func (a *App) ConfigSnapshotRLock() (unlock func()) {
	a.configMu.RLock()
	return a.configMu.RUnlock
}

// diffConfigSections compares two config struct values field by field and
// returns a map of mapstructure keys (top-level section names) to whether
// they changed. Components use component.SectionChanged(ctx, key) during
// Reload to skip expensive re-initialization when their section is unchanged.
func diffConfigSections(old, fresh reflect.Value) map[string]bool {
	t := old.Type()
	sections := make(map[string]bool, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		key := f.Tag.Get("mapstructure")
		if key == "" {
			key = strings.ToLower(f.Name)
		}
		sections[key] = !reflect.DeepEqual(old.Field(i).Interface(), fresh.Field(i).Interface())
	}
	return sections
}

// validateConfigPtr validates an arbitrary config pointer the same way
// validateConfig does for a.configPtr. Used by reloadConfigImmutable.
func (a *App) validateConfigPtr(cfgPtr any, cfgValue reflect.Value) error {
	var errs []error
	if err := validateFields(cfgValue, ""); err != nil {
		errs = append(errs, err)
	}
	if val, ok := cfgPtr.(config.Validatable); ok {
		if err := val.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("chok: validate config: %w", err))
		}
	}
	return errors.Join(errs...)
}

// auditSensitiveConfig checks for placeholder values in sensitive config fields.
// Called before the logger is available, so it returns warnings to be emitted later.
func (a *App) auditSensitiveConfig() []config.SensitiveWarning {
	if a.configPtr == nil {
		return nil
	}
	return config.AuditSensitiveDefaults(a.configPtr, a.envPrefix)
}

// resolveConfigPath determines the config file path.
// Returns (path, explicit). explicit=true means missing file is an error.
func (a *App) resolveConfigPath() (string, bool) {
	// 1. WithConfig explicit path
	if a.configExplicit {
		return a.configPath, true
	}
	// 2. {PREFIX}_CONFIG env
	envKey := a.envPrefix + "_CONFIG"
	if ep := os.Getenv(envKey); ep != "" {
		return ep, true
	}
	// 3. Default lookup: prefer ./{name}.yaml, fall back to
	//    ./configs/{name}.yaml — the layout the `chok init`
	//    scaffold emits, so users get auto-loading without
	//    having to pass WithConfig(..., "configs/foo.yaml").
	primary := "./" + a.name + ".yaml"
	if _, err := os.Stat(primary); err == nil {
		return primary, false
	}
	alt := "./configs/" + a.name + ".yaml"
	if _, err := os.Stat(alt); err == nil {
		return alt, false
	}
	return primary, false
}

// registerDefaults recursively walks the config struct and registers `default` tags
// as Viper defaults. It does NOT modify the struct.
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

		key := field.Tag.Get("mapstructure")
		if key == "" {
			key = strings.ToLower(field.Name)
		}
		if prefix != "" {
			key = prefix + "." + key
		}

		ft := field.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}

		if ft.Kind() == reflect.Struct {
			registerDefaults(v, key, ft)
			continue
		}

		if def := field.Tag.Get("default"); def != "" {
			v.SetDefault(key, def)
		}
	}
}

// bindEnvs recursively binds env vars for all leaf fields.
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

		key := field.Tag.Get("mapstructure")
		if key == "" {
			key = strings.ToLower(field.Name)
		}
		if prefix != "" {
			key = prefix + "." + key
		}

		ft := field.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}

		if ft.Kind() == reflect.Struct {
			bindEnvs(v, key, ft)
			continue
		}

		_ = v.BindEnv(key)
	}
}

// knownOptionsTypes lists the config.Options types that the framework
// discovers via discoverOne for auto-registration. Pointer fields of
// these types break the immutable reload contract.
var knownOptionsTypes = []reflect.Type{
	reflect.TypeFor[config.HTTPOptions](),
	reflect.TypeFor[config.DatabaseOptions](),
	reflect.TypeFor[config.MySQLOptions](),
	reflect.TypeFor[config.SQLiteOptions](),
	reflect.TypeFor[config.RedisOptions](),
	reflect.TypeFor[config.SlogOptions](),
	reflect.TypeFor[config.CacheMemoryOptions](),
	reflect.TypeFor[config.CacheFileOptions](),
	reflect.TypeFor[config.SwaggerOptions](),
	reflect.TypeFor[config.AccountOptions](),
	reflect.TypeFor[config.HealthOptions](),
	reflect.TypeFor[config.MetricsOptions](),
	reflect.TypeFor[config.DebugOptions](),
}

// validateNoPointerOptions checks that no field in the config tree is a
// pointer to a known Options type. Pointer fields break the immutable
// reload contract because reflect.Value.Set replaces the struct content
// in-place; a resolver caching a *PointerField pointer would still hold
// the old object after reload. Value-embedded fields are safe because
// their address is stable within the parent struct.
//
// Since discoverOne now walks nested structs to find Options types, this
// check must also walk the tree — otherwise a user could bury
// `*CacheMemoryOptions` inside a sub-struct and silently bypass the
// invariant. SelfValidating types are opaque to the framework and are
// not descended into, symmetric with discoverOne and validateFields.
func validateNoPointerOptions(rv reflect.Value) error {
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	return walkForPointerOptions(rv, rv.Type().Name())
}

func walkForPointerOptions(rv reflect.Value, pathPrefix string) error {
	t := rv.Type()
	for i := range t.NumField() {
		fv := rv.Field(i)
		ft := t.Field(i)
		if !ft.IsExported() {
			continue
		}
		if ft.Type.Kind() == reflect.Ptr {
			elemType := ft.Type.Elem()
			for _, known := range knownOptionsTypes {
				if elemType == known {
					return fmt.Errorf("config field %s.%s must be value-embedded (not a pointer) for reload safety; use %s instead of *%s",
						pathPrefix, ft.Name, known.Name(), known.Name())
				}
			}
			continue
		}
		if ft.Type.Kind() != reflect.Struct {
			continue
		}
		if isAtomicStruct(ft.Type) {
			continue
		}
		if fv.CanAddr() {
			if _, ok := fv.Addr().Interface().(config.SelfValidating); ok {
				continue
			}
		}
		childPrefix := pathPrefix + "." + ft.Name
		if err := walkForPointerOptions(fv, childPrefix); err != nil {
			return err
		}
	}
	return nil
}

// validateConfig calls Validate() on sub-Options first (recursively), then root config.
// All errors are collected so the caller sees every invalid field at once.
func (a *App) validateConfig() error {
	rv := reflect.ValueOf(a.configPtr)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}

	var errs []error

	// Recursively validate all nested Validatable fields.
	if err := validateFields(rv, ""); err != nil {
		errs = append(errs, err)
	}

	// Validate root config if it implements Validatable (last, for cross-module checks).
	if val, ok := a.configPtr.(config.Validatable); ok {
		if err := val.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("chok: validate config: %w", err))
		}
	}

	return errors.Join(errs...)
}

// validateFields recursively walks struct fields, calling Validate() on each
// Validatable and recursing into nested structs. All errors are collected so
// the caller sees every invalid field in one pass rather than fixing them
// one at a time.
func validateFields(rv reflect.Value, prefix string) error {
	if rv.Kind() != reflect.Struct {
		return nil
	}
	var errs []error
	for i := range rv.NumField() {
		fv := rv.Field(i)
		ft := rv.Type().Field(i)
		if !ft.IsExported() {
			continue
		}

		path := ft.Name
		if prefix != "" {
			path = prefix + "." + ft.Name
		}

		// Dereference pointer fields so *Options types are validated and recursed.
		if fv.Kind() == reflect.Ptr {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
		}

		// Check if the field itself implements Validatable. Track whether
		// the field was a discriminator (SelfValidating) so we can skip
		// the recursive descent below — a discriminator type validates
		// only the nested branch its selector picks, and recursing would
		// trip the unselected branch's Validate (which typically has
		// `enabled: true` defaults demanding fields the user deliberately
		// left blank).
		var skipRecurse bool
		if fv.CanAddr() {
			addr := fv.Addr().Interface()
			if val, ok := addr.(config.Validatable); ok {
				if err := val.Validate(); err != nil {
					errs = append(errs, fmt.Errorf("chok: validate config field %s: %w", path, err))
				}
			}
			if _, ok := addr.(config.SelfValidating); ok {
				skipRecurse = true
			}
		}

		// Recurse into nested structs (even if the parent was Validatable,
		// its children may independently implement Validatable too) unless
		// the parent is a SelfValidating discriminator.
		if !skipRecurse && fv.Kind() == reflect.Struct {
			if err := validateFields(fv, path); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

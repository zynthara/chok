package chok

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

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

	// Validate sub-Options, then root config.
	return a.validateConfig()
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
	// 3. Default path
	return "./" + a.name + ".yaml", false
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

		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Duration(0)) {
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

		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Duration(0)) {
			bindEnvs(v, key, ft)
			continue
		}

		_ = v.BindEnv(key)
	}
}

// validateConfig calls Validate() on sub-Options first (recursively), then root config.
func (a *App) validateConfig() error {
	rv := reflect.ValueOf(a.configPtr)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}

	// Recursively validate all nested Validatable fields.
	if err := validateFields(rv, ""); err != nil {
		return err
	}

	// Validate root config if it implements Validatable (last, for cross-module checks).
	if val, ok := a.configPtr.(config.Validatable); ok {
		if err := val.Validate(); err != nil {
			return fmt.Errorf("chok: validate config: %w", err)
		}
	}

	return nil
}

// validateFields recursively walks struct fields, calling Validate() on each
// Validatable and recursing into nested structs.
func validateFields(rv reflect.Value, prefix string) error {
	if rv.Kind() != reflect.Struct {
		return nil
	}
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

		// Check if the field itself implements Validatable.
		if fv.CanAddr() {
			if val, ok := fv.Addr().Interface().(config.Validatable); ok {
				if err := val.Validate(); err != nil {
					return fmt.Errorf("chok: validate config field %s: %w", path, err)
				}
			}
		}

		// Recurse into nested structs (even if the parent was Validatable,
		// its children may independently implement Validatable too).
		if fv.Kind() == reflect.Struct {
			if err := validateFields(fv, path); err != nil {
				return err
			}
		}
	}
	return nil
}

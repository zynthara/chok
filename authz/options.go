package authz

import (
	"fmt"

	"github.com/zynthara/chok/v2/authz/casbin"
	"github.com/zynthara/chok/v2/conf"
)

// Options is the "authz" yaml section. Every field is restart-only
// (untagged = restart): policy hot-sync is the Redis Watcher's job,
// not config reload's.
type Options struct {
	// Enabled defaults to true — v2 assembly is intent (chok.Use of
	// the module says you want authorization; yaml is the kill
	// switch). Note the flip from v1's default-false.
	Enabled bool `mapstructure:"enabled" default:"true"`

	// Driver names the blessed implementation. Only "casbin" exists;
	// the field is reserved so a future second driver can land without
	// reshaping the section, and Validate refuses anything else so a
	// typo doesn't silently disable authorization.
	Driver string `mapstructure:"driver" default:"casbin"`

	// Casbin configures the casbin driver branch.
	Casbin casbin.Options `mapstructure:"casbin"`
}

// Validate implements conf.Validatable.
func (o *Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if o.Driver != "" && o.Driver != "casbin" {
		return fmt.Errorf("authz: driver %q not supported (only %q)", o.Driver, "casbin")
	}
	return nil
}

// optionsRaw is the method-less twin (conf.Redact godoc pattern).
type optionsRaw Options

// GoString masks sensitive fields (bootstrap_admin_user_id) so %#v
// logging cannot leak them.
func (o Options) GoString() string { return fmt.Sprintf("%#v", conf.Redact(optionsRaw(o))) }

// String implements fmt.Stringer with the same redaction as GoString.
func (o Options) String() string { return o.GoString() }

package parts

import (
	"context"
	"fmt"

	"github.com/zynthara/chok/auth/jwt"
	"github.com/zynthara/chok/component"
)

// JWTBuilder constructs a *jwt.Manager from the app config. Typically
// reads a key/expiration pair out of the user's config struct.
type JWTBuilder func(k component.Kernel) (*jwt.Manager, error)

// JWTComponent owns a single jwt.Manager instance. Applications that
// need multiple managers (e.g. long-lived access tokens + short-lived
// reset tokens) should register multiple JWTComponents with distinct
// Names — the component accepts a name parameter at construction.
//
// No Reload/Health: token signing keys should not hot-swap (breaks
// outstanding tokens) and the manager has no external dependency to
// probe.
type JWTComponent struct {
	name  string
	build JWTBuilder
	mgr   *jwt.Manager
}

// NewJWTComponent constructs a JWT manager component. name defaults to
// "jwt" when empty and is used both as the component Name and
// ConfigKey.
func NewJWTComponent(name string, build JWTBuilder) *JWTComponent {
	if name == "" {
		name = "jwt"
	}
	return &JWTComponent{name: name, build: build}
}

// Name implements component.Component.
func (j *JWTComponent) Name() string { return j.name }

// ConfigKey implements component.Component.
func (j *JWTComponent) ConfigKey() string { return j.name }

// Init builds the manager. Nil managers are rejected as a programming
// error — a component registered but returning nil is a misconfigured
// setup, not a valid "disabled" mode (use UnregisterComponent for that).
func (j *JWTComponent) Init(ctx context.Context, k component.Kernel) error {
	mgr, err := j.build(k)
	if err != nil {
		return fmt.Errorf("jwt (%s) init: %w", j.name, err)
	}
	if mgr == nil {
		return fmt.Errorf("jwt (%s) init: builder returned nil *jwt.Manager", j.name)
	}
	j.mgr = mgr
	return nil
}

// Close is a no-op. jwt.Manager holds only in-memory state.
func (j *JWTComponent) Close(ctx context.Context) error { return nil }

// Manager returns the underlying *jwt.Manager. nil before Init.
func (j *JWTComponent) Manager() *jwt.Manager { return j.mgr }

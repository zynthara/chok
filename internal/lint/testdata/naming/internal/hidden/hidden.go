// Package hidden is internal, so the rules skip it.
package hidden

// WithAnything would break the With rule in a public package.
func WithAnything() {}

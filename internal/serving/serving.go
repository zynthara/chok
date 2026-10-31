// Package serving tells the batteries' Starters whether the App serves
// HTTP. A worker process (http.enabled false) and an App under choktest
// serve none, and a battery's own listener, such as telemetry's metrics
// listener, stays closed with them.
package serving

import "context"

type key struct{}

// With marks ctx with whether the App's HTTP server is on; the App marks
// the context its Starters run in.
func With(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, key{}, on)
}

// Off reports whether ctx says the App serves no HTTP. A context the App
// did not mark, as when a Starter runs on its own, does not say so.
func Off(ctx context.Context) bool {
	on, ok := ctx.Value(key{}).(bool)
	return ok && !on
}

// Package tracectx lets a package read the trace id a context carries
// without importing OpenTelemetry: package telemetry sets TraceID when it
// initializes, so in a binary that links telemetry identity's audit
// events record the request's trace.
package tracectx

import "context"

// TraceID returns the id of the trace ctx belongs to, "" when it belongs
// to none. It is nil until package telemetry initializes.
var TraceID func(ctx context.Context) string

// Package telemetry is chok's observability battery: Prometheus metrics,
// OpenTelemetry traces and, optionally, the App's logs copied to
// OpenTelemetry Logs, under one Config.
//
//	tel := telemetry.MustNew(cfg.Telemetry)
//	d := db.MustOpen(cfg.DB, ...)
//	tel.Instrument(d)
//	app.Use(tel, d, ...)
//
// Registered with app.Use, a Telemetry instruments every request (it is a
// chok.Instrumenter: its middleware runs between ClientIP and AccessLog)
// and serves its registry on telemetry.metrics.path (GET /metrics by
// default). Use it before the components whose shutdown it should still
// see: Close runs in reverse order, and telemetry's Close flushes the
// spans and log records the others' Close produced.
//
// # Names
//
// The names follow the OpenTelemetry semantic conventions (semconv
// v1.43.0), written the Prometheus way: dots become underscores and the
// unit is a suffix.
//
//   - http_server_request_duration_seconds{http_request_method, http_route,
//     http_response_status_code}: a histogram, http.server.request.duration.
//   - http_server_active_requests: requests in flight.
//   - db_client_operation_duration_seconds{db_system_name,
//     db_operation_name, db_collection_name, error_type}: a histogram,
//     db.client.operation.duration, after Instrument.
//   - db_client_slow_operations_total{db_system_name, db_operation_name,
//     db_collection_name}: statements at or over db.slow_threshold.
//   - go_sql_* by db_name: the connection pools, after Instrument.
//   - go_* and process_*: the Go runtime and the process.
//
// A server span is named "{method} {route}", as in "GET
// /api/v1/posts/{id}", and carries http.request.method, http.route,
// http.response.status_code, url.path, url.scheme, server.address,
// network.protocol.version, user_agent.original and client.address; a
// 5xx sets error.type and the error status. A request no route matched is
// "GET unmatched" (route label unmatched). A statement's span is named
// "{operation} {table}", as in "SELECT posts".
//
// # Traces
//
// Tracing is off by default. On, every request continues the trace of an
// incoming W3C traceparent header, or starts one for sample_ratio of
// them, and the spans go to stdout or, over OTLP/HTTP, to the collector
// at tracing.endpoint. The tracer provider and the propagator become the
// OpenTelemetry globals, so a client instrumented with otel's defaults
// joins the request's trace. A binary that links telemetry records the
// trace id in identity's audit events.
package telemetry

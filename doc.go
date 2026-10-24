// Package chok is an opinionated toolkit for Go applications that are
// assembled with plain code: one strict typed configuration, an App with
// a predictable lifecycle, typed HTTP handlers with a runtime OpenAPI
// document, and error values that describe their own HTTP response.
//
// The smallest application:
//
//	func main() {
//		chok.New(chok.MustLoad[chok.Base]("hello")).Execute()
//	}
//
// It serves /healthz, /livez and /readyz on :8080, reads chok.yaml (or
// hello.yaml, or configs/hello.yaml) when present, takes every key from
// HELLO_* environment variables, logs JSON to stdout and stops gracefully
// on SIGINT or SIGTERM.
//
// # Configuration
//
// An application declares its whole configuration as one struct that
// embeds Base and adds a section per battery:
//
//	type Config struct {
//		chok.Base
//		DB db.Config `yaml:"db"`
//	}
//
// Load fills it from defaults, the file, the environment and flags, and
// fails on anything it does not recognize. Only log.level changes while
// running, on SIGHUP.
//
// # Components
//
// Batteries are values: construct them, then register them with
// App.Use, which recognizes Starter, Closer, Healther, Mounter, Namer and
// Instrumenter. There is no registry to look anything up in.
//
// # Errors
//
// A handler returns an *Error or any error implementing Coder; the App
// renders it as {"code","reason","message","request_id","metadata"}.
// Any other error is a 500 whose cause is logged and never sent.
package chok

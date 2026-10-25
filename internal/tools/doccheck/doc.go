// Package doccheck holds the test that compiles the Go programs of the
// documentation: every go block of docs/quickstart.md, README.md,
// README_zh.md and docs/realtime.md is a complete program, built against
// this checkout, and the chok.yaml shown next to it loads strictly into
// the program's configuration. A block fenced as "go fragment" is a
// piece of a program, which the test leaves alone.
package doccheck

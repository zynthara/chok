// Package apptest lets choktest and the batteries' tests reach what a
// test needs without a public API: chok fills in the hooks when it
// initializes, and choktest sets the knobs.
package apptest

import "sync/atomic"

// Ready reports whether app, a *chok.App, has finished its start phase
// and is not stopping.
var Ready func(app any) bool

// PasswordCost, when positive, is the bcrypt cost identity hashes
// passwords with instead of its production cost. choktest sets it to
// bcrypt's minimum, so tests that register users stay fast; only test
// binaries import choktest.
var PasswordCost atomic.Int32

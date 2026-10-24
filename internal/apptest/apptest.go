// Package apptest lets choktest watch an App's lifecycle without a public
// API: chok fills in the hooks when it initializes.
package apptest

// Ready reports whether app, a *chok.App, has finished its start phase
// and is not stopping.
var Ready func(app any) bool

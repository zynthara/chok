// Package csrf lets a battery exempt a route of its own from the App's
// cross-origin protection without an exported API for it.
package csrf

// Exempt, set by package chok when it initializes, exempts the route of
// method and pattern below router — a chok.Router, whose prefix completes
// the pattern — from the CSRF check. A battery calls it in Mount for a
// route a third party posts to from another site, such as an OAuth
// provider's form_post callback, which defends itself another way. A
// router of no App is ignored.
var Exempt func(router any, method, pattern string)

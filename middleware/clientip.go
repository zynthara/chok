package middleware

import (
	"net/http"

	"github.com/zynthara/chok/v2/internal/clientip"
	"github.com/zynthara/chok/v2/internal/ctxval"
)

// ClientIP resolves the real client address through the configured
// trusted-proxy chain and stores it in the request context, where the
// access log and the account login rate limiter (via
// ctxval.ClientIPFrom) pick it up. In v1 this rode inside RequestID;
// it is its own middleware since M2 because the resolver depends on
// the http section's trusted_proxies (SPEC §4.2 item 4).
//
// An unresolvable address stores nothing — downstream consumers skip
// IP-keyed decisions rather than sharing an empty-string bucket.
func ClientIP(resolver *clientip.Resolver) func(http.Handler) http.Handler {
	if resolver == nil {
		panic("middleware: ClientIP resolver must not be nil")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip := resolver.ClientIP(r); ip != "" {
				r = r.WithContext(ctxval.WithClientIP(r.Context(), ip))
			}
			next.ServeHTTP(w, r)
		})
	}
}

package main

import (
	"net/http"

	"github.com/zynthara/chok/v3"
)

// devAuthn stands in for identity's Authn until WP6: the X-User-Id
// header names the caller. Without it the request stays anonymous and
// the store fails closed — writes are 401s, reads find nothing. Anyone
// can claim any id, so it never ships.
func devAuthn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-User-Id"); id != "" {
			r = r.WithContext(chok.WithPrincipal(r.Context(), chok.Principal{UserID: id}))
		}
		next.ServeHTTP(w, r)
	})
}

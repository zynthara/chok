package middleware

import (
	"context"
	"net/http"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/auth"
	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/log"
)

type authzCtxKey struct{}

// WithAuthorizer stores the active authz.Authorizer on the context —
// the v2 replacement for v1's gin-context key. AttachAuthz applies it
// per request; exported so application code can override the
// authorizer for a sub-router (per-tenant enforcement) and tests can
// exercise RequireAuthz without the full module wiring.
func WithAuthorizer(ctx context.Context, az authz.Authorizer) context.Context {
	return context.WithValue(ctx, authzCtxKey{}, az)
}

// AuthorizerFrom retrieves the Authorizer installed by AttachAuthz /
// WithAuthorizer. ok=false means no authz component was wired.
func AuthorizerFrom(ctx context.Context) (authz.Authorizer, bool) {
	az, ok := ctx.Value(authzCtxKey{}).(authz.Authorizer)
	return az, ok && az != nil
}

// AttachAuthz installs the supplied Authorizer onto every request
// context for downstream RequireAuthz / RequireAuthzInDomain calls.
// Production wiring is automatic: web.Module attaches it during
// assembly when an authz component is present (soft dependency —
// absent means no attach, no error). Manual use covers tests and
// sub-router overrides.
func AttachAuthz(az authz.Authorizer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(WithAuthorizer(r.Context(), az)))
		})
	}
}

// RequireAuthz is the chok-blessed authorization middleware for
// global / single-tenant routes. It calls
//
//	authz.Authorize(ctx, principal.Subject, obj, act)
//
// against the Authorizer attached to the request context.
//
// Outcomes:
//   - allowed=true  → next
//   - allowed=false → 403 PermissionDenied
//   - error         → 500 InternalError (logged with subject/obj/act)
//   - no Principal  → 401 Unauthenticated (Authn middleware missing)
//   - no Authorizer → 500 InternalError ("authz not wired" — fix wiring)
//
// The URL-based form (object=route pattern, action=HTTP method) was
// removed in v0.3 because (a) policy DBs filled with URL strings are
// fragile under refactors and (b) Casbin idiom is business-code
// (sub, obj, act) tuples decoupled from transport.
func RequireAuthz(obj, act string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			p, ok := auth.PrincipalFrom(ctx)
			if !ok {
				handler.WriteResponse(w, r, 0, nil, apierr.ErrUnauthenticated)
				return
			}
			az, ok := AuthorizerFrom(ctx)
			if !ok {
				logAuthzWiringError(ctx, "RequireAuthz", obj, act, p.Subject)
				handler.WriteResponse(w, r, 0, nil, apierr.ErrInternal.WithMessage("authz not wired"))
				return
			}
			allowed, err := az.Authorize(ctx, p.Subject, obj, act)
			if err != nil {
				logAuthzError(ctx, "RequireAuthz", err, p.Subject, "", obj, act)
				handler.WriteResponse(w, r, 0, nil, apierr.ErrInternal)
				return
			}
			if !allowed {
				handler.WriteResponse(w, r, 0, nil, apierr.ErrPermissionDenied)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAuthzInDomain is the multi-tenant cousin of RequireAuthz.
// It reads the tenant id from the named path parameter (typically
// "wsid", "orgid", "tenant") via r.PathValue and calls
//
//	authz.AuthorizeInDomain(ctx, principal.Subject, dom, obj, act)
//
// requiring the configured Authorizer to satisfy authz.DomainAuthorizer.
//
// Fail-closed posture (SPEC §0.3): when the active Authorizer does
// NOT implement DomainAuthorizer the middleware refuses the request
// with 500 — silent degradation to Authorize would drop the domain
// constraint and let cross-tenant requests through. Wire the right
// Authorizer (chok's authz/casbin satisfies it natively) or this
// route is unsafe to expose.
//
// domainParam is consulted via r.PathValue; an empty value yields
// 400 InvalidArgument (the route declared {wsid} but the request didn't
// match — almost certainly a routing bug in the application).
func RequireAuthzInDomain(obj, act, domainParam string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			p, ok := auth.PrincipalFrom(ctx)
			if !ok {
				handler.WriteResponse(w, r, 0, nil, apierr.ErrUnauthenticated)
				return
			}
			az, ok := AuthorizerFrom(ctx)
			if !ok {
				logAuthzWiringError(ctx, "RequireAuthzInDomain", obj, act, p.Subject)
				handler.WriteResponse(w, r, 0, nil, apierr.ErrInternal.WithMessage("authz not wired"))
				return
			}
			dz, ok := az.(authz.DomainAuthorizer)
			if !ok {
				// Fail-closed: refuse rather than silently degrade.
				if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok && l != nil {
					l.ErrorContext(ctx,
						"authz: RequireAuthzInDomain used but Authorizer is not a DomainAuthorizer; refusing to avoid silently bypassing domain check",
						"obj", obj, "act", act, "domain_param", domainParam)
				}
				handler.WriteResponse(w, r, 0, nil, apierr.ErrInternal.WithMessage("authz domain not supported"))
				return
			}
			dom := r.PathValue(domainParam)
			if dom == "" {
				handler.WriteResponse(w, r, 0, nil,
					apierr.ErrInvalidArgument.WithMessage("missing domain path parameter: "+domainParam))
				return
			}
			allowed, err := dz.AuthorizeInDomain(ctx, p.Subject, dom, obj, act)
			if err != nil {
				logAuthzError(ctx, "RequireAuthzInDomain", err, p.Subject, dom, obj, act)
				handler.WriteResponse(w, r, 0, nil, apierr.ErrInternal)
				return
			}
			if !allowed {
				handler.WriteResponse(w, r, 0, nil, apierr.ErrPermissionDenied)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// logAuthzError emits a structured log entry for an Authorize-side
// failure. Best-effort — if the request context never had a logger
// stamped, drops silently rather than panicking.
func logAuthzError(ctx context.Context, where string, err error, subject, domain, obj, act string) {
	if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok && l != nil {
		l.ErrorContext(ctx, "authorization failed",
			"where", where,
			"error", err,
			"subject", subject,
			"domain", domain,
			"obj", obj,
			"act", act,
		)
	}
}

// logAuthzWiringError reports that AttachAuthz never ran for this
// request (no authz component assembled, or it initialized without an
// Authorizer).
func logAuthzWiringError(ctx context.Context, where, obj, act, subject string) {
	if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok && l != nil {
		l.ErrorContext(ctx, "authz not wired; assemble the authz module (or attach one manually via middleware.AttachAuthz)",
			"where", where, "obj", obj, "act", act, "subject", subject)
	}
}

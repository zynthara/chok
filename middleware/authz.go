package middleware

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/v2/apierr"
	"github.com/zynthara/chok/v2/auth"
	"github.com/zynthara/chok/v2/authz"
	"github.com/zynthara/chok/v2/handler"
	"github.com/zynthara/chok/v2/internal/ctxval"
	"github.com/zynthara/chok/v2/log"
)

// ContextKeyAuthz is the gin.Context key under which the active
// authz.Authorizer lives. AttachAuthz writes it; RequireAuthz /
// RequireAuthzInDomain read it.
//
// Exported as a string constant so application code can override the
// gin context (e.g. mount a per-tenant Authorizer on a sub-router)
// without depending on this package's internals.
const ContextKeyAuthz = "chok.authz"

// AttachAuthz installs the supplied Authorizer onto the gin.Context
// for downstream RequireAuthz / RequireAuthzInDomain calls. Production
// chok wiring (parts/http.go OptionalDependencies + Init) calls this
// automatically when an AuthzComponent is registered, so application
// code rarely needs to invoke it directly.
//
// Tests that exercise RequireAuthz without spinning up the full
// component registry should mount AttachAuthz on their gin engine
// before the protected routes.
func AttachAuthz(az authz.Authorizer) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(ContextKeyAuthz, az)
		c.Next()
	}
}

// RequireAuthz is the chok-blessed authorization middleware for
// global / single-tenant routes. It calls
//
//	authz.Authorize(ctx, principal.Subject, obj, act)
//
// against the Authorizer attached to the gin.Context.
//
// Outcomes:
//   - allowed=true  → c.Next()
//   - allowed=false → 403 PermissionDenied
//   - error         → 500 InternalError (logged with subject/obj/act)
//   - no Principal  → 401 Unauthenticated (Authn middleware missing)
//   - no Authorizer → 500 InternalError ("authz not wired" — fix wiring)
//
// The previous URL-based form (Authz(az), object=route pattern,
// action=HTTP method) was removed in v0.3 because (a) policy DBs
// filled with URL strings are fragile under refactors and (b) Casbin
// idiom is business-code (sub, obj, act) tuples decoupled from
// transport.
func RequireAuthz(obj, act string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			handler.WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
			c.Abort()
			return
		}
		az, ok := authzFromContext(c)
		if !ok {
			logAuthzWiringError(ctx, "RequireAuthz", obj, act, p.Subject)
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal.WithMessage("authz not wired"))
			c.Abort()
			return
		}
		allowed, err := az.Authorize(ctx, p.Subject, obj, act)
		if err != nil {
			logAuthzError(ctx, "RequireAuthz", err, p.Subject, "", obj, act)
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal)
			c.Abort()
			return
		}
		if !allowed {
			handler.WriteResponse(c, 0, nil, apierr.ErrPermissionDenied)
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireAuthzInDomain is the multi-tenant cousin of RequireAuthz.
// It reads the tenant id from the named gin path parameter
// (typically "wsid", "orgid", "tenant") and calls
//
//	authz.AuthorizeInDomain(ctx, principal.Subject, dom, obj, act)
//
// requiring the configured Authorizer to satisfy authz.DomainAuthorizer.
//
// Fail-closed posture (SPEC v0.3.2): when the active Authorizer does
// NOT implement DomainAuthorizer the middleware refuses the request
// with 500 — silent degradation to Authorize would drop the domain
// constraint and let cross-tenant requests through. Wire the right
// Authorizer (chok's authz/casbin satisfies it natively) or this
// route is unsafe to expose.
//
// domainParam name is consulted via c.Param; an empty value yields
// 400 InvalidArgument (the route declared :wsid but the request didn't
// match — almost certainly a routing bug in the application).
func RequireAuthzInDomain(obj, act, domainParam string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			handler.WriteResponse(c, 0, nil, apierr.ErrUnauthenticated)
			c.Abort()
			return
		}
		az, ok := authzFromContext(c)
		if !ok {
			logAuthzWiringError(ctx, "RequireAuthzInDomain", obj, act, p.Subject)
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal.WithMessage("authz not wired"))
			c.Abort()
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
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal.WithMessage("authz domain not supported"))
			c.Abort()
			return
		}
		dom := c.Param(domainParam)
		if dom == "" {
			handler.WriteResponse(c, 0, nil,
				apierr.ErrInvalidArgument.WithMessage("missing domain path parameter: "+domainParam))
			c.Abort()
			return
		}
		allowed, err := dz.AuthorizeInDomain(ctx, p.Subject, dom, obj, act)
		if err != nil {
			logAuthzError(ctx, "RequireAuthzInDomain", err, p.Subject, dom, obj, act)
			handler.WriteResponse(c, 0, nil, apierr.ErrInternal)
			c.Abort()
			return
		}
		if !allowed {
			handler.WriteResponse(c, 0, nil, apierr.ErrPermissionDenied)
			c.Abort()
			return
		}
		c.Next()
	}
}

// authzFromContext retrieves the Authorizer that AttachAuthz stored.
// Returns ok=false when AttachAuthz didn't run (deployments that
// forgot to register an AuthzComponent or didn't wire it through
// HTTPComponent.OptionalDependencies).
func authzFromContext(c *gin.Context) (authz.Authorizer, bool) {
	v, ok := c.Get(ContextKeyAuthz)
	if !ok {
		return nil, false
	}
	az, ok := v.(authz.Authorizer)
	return az, ok
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
// request (no AuthzComponent registered, or HTTPComponent didn't
// pull it via OptionalDependencies).
func logAuthzWiringError(ctx context.Context, where, obj, act, subject string) {
	if l, ok := ctxval.LoggerFrom(ctx).(log.Logger); ok && l != nil {
		l.ErrorContext(ctx, "authz not wired; register parts.AuthzComponent or include it in HTTPComponent OptionalDependencies",
			"where", where, "obj", obj, "act", act, "subject", subject)
	}
}

// Package identity is the identity battery: accounts with a password or
// an OAuth provider, the access tokens that prove them, the middleware
// that checks those tokens, the administration of users and an audit log.
//
//	type Config struct {
//		chok.Base
//		DB   db.Config       `yaml:"db"`
//		Auth identity.Config `yaml:"auth"`
//	}
//
//	d := db.MustOpen(cfg.DB, db.Tables(db.Table(&Post{})))
//	auth := identity.MustNew(cfg.Auth, d)
//	app.Use(d, auth)                           // mounts /auth/*
//	api := app.Group("/api/v1", auth.Authn())  // admits valid tokens only
//
// # Routes
//
// Mount registers register, login, refresh-token, change-password and me
// under auth.prefix (/auth), forgot-password and reset-password with a
// Sender, and the OAuth routes with Providers. The public ones are
// documented without security in the OpenAPI document. Request and
// response bodies are v2's: register takes {email, password, name} and
// answers 201 {token, expires_at}; login takes {email, password}.
//
// # Mail and passwordless sign-in
//
// With Sender(s), identity asks the application to send mail: a
// password-reset token from forgot-password, and — with
// auth.passwordless.enabled — a sign-in code. Each is a Mail whose Kind
// picks the application's template; the send runs in the background and
// Close waits for it. POST {prefix}/passwordless/start {email} always
// answers 202 and mails a code and a magic-link token, unless one went out
// within auth.passwordless.resend_after; POST {prefix}/passwordless/verify
// with {email, code} or {token} answers what login does, creating the
// account of an unknown address unless auto_register is off. Codes are
// kept as HMACs in auth_login_codes, tried at most max_attempts times.
//
// # Tokens and the principal
//
// A token is an HS256 JWT signed with auth.signing_key, valid for
// auth.token_ttl; refresh-token trades it for a fresh one until the
// session is auth.refresh_ttl old. Authn checks the token and the account
// behind it on every request — a disabled user, or one whose password or
// roles changed, is refused at once — and stores a chok.Principal whose
// UserID is the user's id (usr_…). store's owner scope writes that id to
// owner_id.
//
// # Tables
//
// New registers users, identities, audit_logs and auth_login_codes under
// the owner "identity": migrate auto creates them from the models,
// versioned applies the SQL files embedded in this package (one directory
// per dialect), off checks they were applied.
//
// # OAuth
//
// identity/google, identity/github, identity/apple and identity/facebook
// each return a Provider from their New. A sign-in starts at
// {prefix}/{name}/start, comes back to the provider's callback and ends
// with the front end trading a one-time code for a token at
// {prefix}/exchange, from the browser that holds the binding cookie.
// The state of the sign-ins in flight lives in this process.
//
// # Audit
//
// With auth.audit.enabled, Audit().Log records who did what, in the
// background; records are only ever added, and purged after
// auth.audit.retention.
package identity

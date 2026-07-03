package account

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// allowedRedirect is the parsed form of a WithAllowedRedirectBacks entry.
// Stored on the Module so validateRedirectBack can compare scheme / host /
// path with strict semantics, not the naïve strings.HasPrefix that earlier
// versions used.
//
// Boundary rules enforced at parseAllowedRedirect time:
//   - scheme MUST be https (http only allowed in dev mode, see WithDevMode)
//   - host MUST be non-empty; userinfo MUST be empty
//   - path MUST start with "/" (entries without a path are normalized to "/")
//
// Match rules at validate time:
//   - scheme + host + port must match exactly (case-insensitive on host)
//   - path matches if input.path == entry.path OR
//     (entry.path ends with "/" AND input.path starts with entry.path) OR
//     (input.path starts with entry.path AND the next byte is '/'/'?'/'#').
//     This blocks the `https://app.example.com/post-login-evil` evader for
//     a `/post-login` entry, and the `https://app.example.com.evil.com/` evader
//     for a `https://app.example.com` entry.
type allowedRedirect struct {
	scheme       string
	host         string // includes :port if non-default
	path         string // always starts with "/", "" → "/"
	pathTrailing bool   // true if original path ended with "/" — site-wide match
}

// parseAllowedRedirect validates and decomposes a single entry. Returns
// an error rather than panicking so the caller (Module.New via
// WithAllowedRedirectBacks) can surface a startup failure with the
// offending value.
//
// Query and fragment are rejected outright: they have no role in defining
// "what URLs are allowed as a landing page" and silently ignoring them
// (or worse, mixing them into the suffix-of-/ check) makes the boundary
// rule fragile. An operator who needs query-aware matching should split
// it into multiple precise allowlist entries.
func parseAllowedRedirect(raw string) (allowedRedirect, error) {
	if raw == "" {
		return allowedRedirect{}, errors.New("empty entry")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return allowedRedirect{}, fmt.Errorf("parse: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return allowedRedirect{}, fmt.Errorf("scheme %q not allowed (must be https or http)", u.Scheme)
	}
	if u.Host == "" {
		return allowedRedirect{}, errors.New("host is empty")
	}
	if u.User != nil {
		return allowedRedirect{}, errors.New("userinfo is not allowed in redirect_back allowlist")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return allowedRedirect{}, errors.New("query string is not allowed in redirect_back allowlist entry")
	}
	if u.Fragment != "" {
		return allowedRedirect{}, errors.New("fragment is not allowed in redirect_back allowlist entry")
	}
	path := u.Path
	// pathTrailing is the source-of-truth signal for "site-wide entry".
	// It MUST come from the parsed path, not the raw string — otherwise
	// query/fragment ending in "/" could mis-flag /foo as site-wide.
	pathTrailing := strings.HasSuffix(path, "/")
	if path == "" {
		path = "/"
		pathTrailing = true
	}
	return allowedRedirect{
		scheme:       u.Scheme,
		host:         strings.ToLower(u.Host),
		path:         path,
		pathTrailing: pathTrailing,
	}, nil
}

// matches reports whether the parsed input URL satisfies this allowlist
// entry. Caller has already pre-parsed and rejected userinfo / non-https.
func (a allowedRedirect) matches(scheme, host, path string) bool {
	if a.scheme != scheme {
		return false
	}
	if a.host != host {
		return false
	}
	if a.pathTrailing {
		// Site-wide entry. path must start with the entry's path, which
		// itself ends with "/" — gives boundary-correct prefix.
		return strings.HasPrefix(path, a.path)
	}
	// Single-URL entry. Exact match, or input is entry + "/.../?.../#..."
	if path == a.path {
		return true
	}
	if strings.HasPrefix(path, a.path) {
		next := path[len(a.path)]
		return next == '/' || next == '?' || next == '#'
	}
	return false
}

// validateRedirectBack enforces SPEC §6.1: the post-login landing URL
// passed via ?redirect_back must be either a safe relative path or a
// boundary-strict match against the operator-supplied allow-list.
//
// Default policy is intentionally strict — empty list means relative
// paths only. The allow-list exists for genuine multi-front-end
// deployments (one chok back-end serving several SPA hosts).
//
// Anything else fails closed. The error message is descriptive but
// callers (Module.handleBegin / handleLinkIdentity) wrap it into
// ErrInvalidArgument so the final HTTP response stays opaque enough not
// to advertise the policy.
func (m *Service) validateRedirectBack(redirectBack string) error {
	if redirectBack == "" {
		return nil
	}

	if isSafeRelativePath(redirectBack) {
		return nil
	}

	if len(m.allowedRedirects) == 0 {
		return errors.New("redirect_back must be a relative path; no absolute URL allowlist configured")
	}

	u, err := url.Parse(redirectBack)
	if err != nil {
		return fmt.Errorf("redirect_back parse: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return errors.New("redirect_back must be a relative path or fully-qualified URL")
	}
	if u.User != nil {
		return errors.New("redirect_back must not contain userinfo")
	}
	host := strings.ToLower(u.Host)
	path := u.Path
	if path == "" {
		path = "/"
	}
	// Preserve query and fragment in the comparison input so an entry like
	// https://app.example.com/landing matches https://app.example.com/landing?x=1
	// (the boundary check on path-only is enough; query/fragment are user-data).
	for _, allow := range m.allowedRedirects {
		if allow.matches(u.Scheme, host, path) {
			return nil
		}
	}
	return errors.New("redirect_back not in allowlist")
}

// isSafeRelativePath returns true iff s is a "/..." style path that the
// browser will resolve relative to the current origin and cannot be
// reinterpreted as a network reference.
//
// The "//evil.com/x" form (protocol-relative URL) starts with "/" but
// resolves to https://evil.com/x; "\\\\evil.com" / unicode equivalents
// of "/" are accepted as path separators by some legacy browsers. Any
// non-ASCII or backslash makes the input unsafe — the rule is "ASCII,
// starts with one slash, second char is not slash".
func isSafeRelativePath(s string) bool {
	if len(s) < 1 || s[0] != '/' {
		return false
	}
	if len(s) >= 2 && (s[1] == '/' || s[1] == '\\') {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' {
			return false
		}
		// ASCII printable + tab; excludes control chars and high bytes
		// (Unicode look-alike slashes). 0x7F is DEL — exclude too.
		if c < 0x20 || c >= 0x7F {
			return false
		}
	}
	return true
}

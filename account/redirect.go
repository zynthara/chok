package account

import (
	"errors"
	"net/http"
	"strings"
)

// validateRedirectBack enforces SPEC §6.1: the post-login landing URL
// passed via ?redirect_back must be either a safe relative path or a
// prefix-match against the operator-supplied allow-list.
//
// Default policy is intentionally strict — empty list means relative
// paths only. The allow-list exists for genuine multi-front-end
// deployments (one chok back-end serving several SPA hosts); even then
// each entry must be HTTPS and exactly match scheme + host + port.
//
// Anything else fails closed. The error message is descriptive but
// callers (Module.handleBegin) wrap it into ErrInvalidArgument so the
// final HTTP response stays opaque enough not to advertise the policy.
func (m *Module) validateRedirectBack(_ *http.Request, redirectBack string) error {
	if redirectBack == "" {
		return nil
	}

	if isSafeRelativePath(redirectBack) {
		return nil
	}

	for _, allow := range m.allowedRedirectBacks {
		if strings.HasPrefix(redirectBack, allow) {
			return nil
		}
	}

	return errors.New("redirect_back must be a relative path or match an allowed prefix")
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

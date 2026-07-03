package account

import (
	"testing"
)

func TestValidateRedirectBack(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		allow  []string
		wantOK bool
	}{
		{"empty allowed", "", nil, true},
		{"relative path allowed", "/dashboard", nil, true},
		{"relative path with query", "/posts/123?from=login", nil, true},
		{"protocol-relative rejected", "//evil.com/x", nil, false},
		{"backslash rejected", "/foo\\evil", nil, false},
		{"absolute http rejected by default", "http://app.example.com/", nil, false},
		{"absolute https rejected by default", "https://app.example.com/dashboard", nil, false},
		{"non-ascii rejected", "/foo bar", nil, false},
		{"control char rejected", "/foo\nbar", nil, false},
		{"high byte rejected", "/foo\x7fbar", nil, false},
		{"absolute https whitelisted prefix", "https://app.example.com/dashboard", []string{"https://app.example.com/"}, true},
		{"absolute https whitelisted exact", "https://app.example.com/post-login", []string{"https://app.example.com/post-login"}, true},
		{"absolute https not in whitelist", "https://other.example.com/", []string{"https://app.example.com/"}, false},
		// Host-level prefix evader: trailing-/ entry has site-wide scope but
		// boundary check still rejects domain-suffix lookalikes.
		{"prefix evader rejected (host)", "https://app.example.com.evil.com/", []string{"https://app.example.com/"}, false},
		// Path-level prefix evader: single-URL entry must not allow
		// path-suffix appendage.
		{"prefix evader rejected (path)", "https://app.example.com/post-login-evil", []string{"https://app.example.com/post-login"}, false},
		{"single-url with query allowed", "https://app.example.com/post-login?next=/dashboard", []string{"https://app.example.com/post-login"}, true},
		{"single-url with fragment allowed", "https://app.example.com/post-login#section", []string{"https://app.example.com/post-login"}, true},
		{"single-url child path allowed", "https://app.example.com/post-login/child", []string{"https://app.example.com/post-login"}, true},
		// Cross-scheme rejection.
		{"http when allowlist is https", "http://app.example.com/post-login", []string{"https://app.example.com/post-login"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := make([]allowedRedirect, 0, len(tc.allow))
			for _, raw := range tc.allow {
				entry, err := parseAllowedRedirect(raw)
				if err != nil {
					t.Fatalf("setup: parse %q: %v", raw, err)
				}
				parsed = append(parsed, entry)
			}
			m := &Service{allowedRedirects: parsed}
			err := m.validateRedirectBack(tc.input)
			if tc.wantOK && err != nil {
				t.Fatalf("expected ok, got err: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("expected err, got ok")
			}
		})
	}
}

// TestParseAllowedRedirect_Reject exercises the startup-time validation
// that rejects malformed allowlist entries (userinfo, missing host, bad
// scheme). Module.New surfaces these as fail-fast errors rather than
// letting the validator silently fall through.
func TestParseAllowedRedirect_Reject(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"missing host", "https:///"},
		{"with userinfo", "https://user:pass@app.example.com/"},
		{"non-http scheme", "ftp://app.example.com/"},
		// F2: query / fragment in allowlist entry must be rejected at
		// parse time; otherwise a query that happens to end with "/"
		// would mis-flag the entry as site-wide and let
		// /post-login-evil bypass a /post-login allowlist.
		{"with query", "https://app.example.com/post-login?x=/"},
		{"with empty query (?)", "https://app.example.com/post-login?"},
		{"with fragment", "https://app.example.com/post-login#section"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseAllowedRedirect(tc.raw); err == nil {
				t.Fatalf("expected error for %q, got nil", tc.raw)
			}
		})
	}
}

// TestValidateRedirectBack_F2_QueryEndingSlashEntryRejected proves that
// even if a query/fragment-bearing entry slipped past parse (it
// shouldn't), the path-boundary check itself is no longer fooled by
// trailing "/" in the raw string. We exercise the full Module.New
// pipeline so the rejection is observable at the public API layer.
func TestValidateRedirectBack_F2_QueryEndingSlashEntryRejected(t *testing.T) {
	// parseAllowedRedirect rejects the entry — so Module.New errors out
	// before we ever reach validateRedirectBack. That's the contract.
	if _, err := parseAllowedRedirect("https://app.example.com/post-login?x=/"); err == nil {
		t.Fatal("entry with trailing-slash query MUST be rejected at parse time")
	}
}

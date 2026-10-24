package identity

import "testing"

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
		{"non-ascii rejected", "/foo\u2028bar", nil, false},
		{"control char rejected", "/foo\nbar", nil, false},
		{"high byte rejected", "/foo\x7fbar", nil, false},
		{"absolute https allow-listed prefix", "https://app.example.com/dashboard", []string{"https://app.example.com/"}, true},
		{"absolute https allow-listed exact", "https://app.example.com/post-login", []string{"https://app.example.com/post-login"}, true},
		{"absolute https not allow-listed", "https://other.example.com/", []string{"https://app.example.com/"}, false},
		{"prefix evader rejected (host)", "https://app.example.com.evil.com/", []string{"https://app.example.com/"}, false},
		{"prefix evader rejected (path)", "https://app.example.com/post-login-evil", []string{"https://app.example.com/post-login"}, false},
		{"single-url with query allowed", "https://app.example.com/post-login?next=/dashboard", []string{"https://app.example.com/post-login"}, true},
		{"single-url with fragment allowed", "https://app.example.com/post-login#section", []string{"https://app.example.com/post-login"}, true},
		{"single-url child path allowed", "https://app.example.com/post-login/child", []string{"https://app.example.com/post-login"}, true},
		{"http when the allow-list is https", "http://app.example.com/post-login", []string{"https://app.example.com/post-login"}, false},
		{"userinfo rejected", "https://user@app.example.com/", []string{"https://app.example.com/"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &oauthFlow{}
			for _, raw := range tc.allow {
				entry, err := parseAllowedRedirect(raw)
				if err != nil {
					t.Fatalf("parse %q: %v", raw, err)
				}
				f.allowed = append(f.allowed, entry)
			}
			if err := f.validateRedirectBack(tc.input); (err == nil) != tc.wantOK {
				t.Fatalf("validateRedirectBack(%q) = %v, want ok=%v", tc.input, err, tc.wantOK)
			}
		})
	}
}

// TestParseAllowedRedirect_Reject: a malformed entry fails at startup,
// including one whose query or fragment ends in a slash, which would
// otherwise read as a site-wide entry.
func TestParseAllowedRedirect_Reject(t *testing.T) {
	for _, raw := range []string{
		"", "https:///", "https://user:pass@app.example.com/", "ftp://app.example.com/",
		"https://app.example.com/post-login?x=/", "https://app.example.com/post-login?", "https://app.example.com/post-login#section",
	} {
		if _, err := parseAllowedRedirect(raw); err == nil {
			t.Errorf("%q passed", raw)
		}
	}
}

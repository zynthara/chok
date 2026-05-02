package account

import (
	"net/http/httptest"
	"testing"
)

func TestValidateRedirectBack(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		allow      []string
		wantOK     bool
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
		{"prefix evader rejected", "https://app.example.com.evil.com/", []string{"https://app.example.com/"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Module{allowedRedirectBacks: tc.allow}
			err := m.validateRedirectBack(httptest.NewRequest("GET", "/", nil), tc.input)
			if tc.wantOK && err != nil {
				t.Fatalf("expected ok, got err: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("expected err, got ok")
			}
		})
	}
}

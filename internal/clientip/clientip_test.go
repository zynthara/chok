package clientip

import (
	"net/http/httptest"
	"testing"
)

func mustResolver(t *testing.T, trusted ...string) *Resolver {
	t.Helper()
	r, err := NewResolver(trusted)
	if err != nil {
		t.Fatalf("NewResolver(%v): %v", trusted, err)
	}
	return r
}

func TestClientIP_SpoofedXFFIgnoredWithoutTrustedProxies(t *testing.T) {
	// THE regression case (SPEC §4.2 item 4): default config trusts
	// nobody, so a client-supplied X-Forwarded-For must never bypass
	// IP-keyed rate limiting.
	r := mustResolver(t)
	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "203.0.113.7:41234"
	rq.Header.Set("X-Forwarded-For", "10.0.0.99")
	rq.Header.Set("X-Real-IP", "10.0.0.98")

	if got := r.ClientIP(rq); got != "203.0.113.7" {
		t.Fatalf("spoofed XFF must be ignored, got %q", got)
	}
}

func TestClientIP_SpoofedXFFIgnoredFromUntrustedPeer(t *testing.T) {
	// Trust is configured, but for a different network than the peer.
	r := mustResolver(t, "10.0.0.0/8")
	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "203.0.113.7:41234"
	rq.Header.Set("X-Forwarded-For", "198.51.100.1")

	if got := r.ClientIP(rq); got != "203.0.113.7" {
		t.Fatalf("XFF from untrusted peer must be ignored, got %q", got)
	}
}

func TestClientIP_TrustedProxyHonoursXFF(t *testing.T) {
	r := mustResolver(t, "127.0.0.1")
	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "127.0.0.1:9000"
	rq.Header.Set("X-Forwarded-For", "203.0.113.7")

	if got := r.ClientIP(rq); got != "203.0.113.7" {
		t.Fatalf("trusted peer ⇒ XFF client, got %q", got)
	}
}

func TestClientIP_WalksChainSkippingTrustedHops(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8", "127.0.0.1")
	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "127.0.0.1:9000"
	// client → 10.0.0.5 (our LB) → 127.0.0.1 (local proxy)
	rq.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.5")

	if got := r.ClientIP(rq); got != "203.0.113.7" {
		t.Fatalf("first untrusted hop from the right is the client, got %q", got)
	}
}

func TestClientIP_ClientCannotPrependFakeHop(t *testing.T) {
	// The client itself sends "X-Forwarded-For: 1.1.1.1"; the trusted
	// LB appends the real client address. The rightmost untrusted hop
	// (the real client) must win, not the client-chosen leftmost.
	r := mustResolver(t, "10.0.0.0/8")
	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "10.0.0.5:9000"
	rq.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.7")

	if got := r.ClientIP(rq); got != "203.0.113.7" {
		t.Fatalf("rightmost untrusted hop must win, got %q", got)
	}
}

func TestClientIP_AllTrustedChainYieldsLeftmost(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8", "127.0.0.1")
	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "127.0.0.1:9000"
	rq.Header.Set("X-Forwarded-For", "10.0.0.9, 10.0.0.5")

	if got := r.ClientIP(rq); got != "10.0.0.9" {
		t.Fatalf("all-trusted chain returns leftmost (gin parity), got %q", got)
	}
}

func TestClientIP_MalformedXFFFallsBack(t *testing.T) {
	r := mustResolver(t, "127.0.0.1")

	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "127.0.0.1:9000"
	rq.Header.Set("X-Forwarded-For", "203.0.113.7, not-an-ip")
	if got := r.ClientIP(rq); got != "127.0.0.1" {
		t.Fatalf("malformed hop invalidates the header, want peer, got %q", got)
	}

	// Malformed XFF, valid X-Real-IP: the next header applies.
	rq.Header.Set("X-Real-IP", "203.0.113.9")
	if got := r.ClientIP(rq); got != "203.0.113.9" {
		t.Fatalf("X-Real-IP should apply after invalid XFF, got %q", got)
	}
}

func TestClientIP_XRealIPRequiresTrust(t *testing.T) {
	r := mustResolver(t)
	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "203.0.113.7:1"
	rq.Header.Set("X-Real-IP", "10.9.9.9")
	if got := r.ClientIP(rq); got != "203.0.113.7" {
		t.Fatalf("X-Real-IP from untrusted peer must be ignored, got %q", got)
	}
}

func TestClientIP_IPv6AndMappedForms(t *testing.T) {
	r := mustResolver(t, "::1", "10.0.0.0/8")

	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "[::1]:9000"
	rq.Header.Set("X-Forwarded-For", "2001:db8::7")
	if got := r.ClientIP(rq); got != "2001:db8::7" {
		t.Fatalf("IPv6 chain, got %q", got)
	}

	// IPv4-mapped IPv6 peer must match an IPv4 trust rule.
	rq2 := httptest.NewRequest("GET", "/", nil)
	rq2.RemoteAddr = "[::ffff:10.0.0.5]:9000"
	rq2.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := r.ClientIP(rq2); got != "203.0.113.7" {
		t.Fatalf("IPv4-mapped peer should be trusted via 10/8, got %q", got)
	}
}

func TestClientIP_BareRemoteAddrWithoutPort(t *testing.T) {
	r := mustResolver(t)
	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "203.0.113.7"
	if got := r.ClientIP(rq); got != "203.0.113.7" {
		t.Fatalf("bare RemoteAddr, got %q", got)
	}
}

func TestClientIP_UnparseableRemoteAddr(t *testing.T) {
	r := mustResolver(t)
	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "@"
	rq.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := r.ClientIP(rq); got != "" {
		t.Fatalf("unparseable peer ⇒ empty (callers skip IP keying), got %q", got)
	}
}

func TestNewResolver_RejectsMalformedEntries(t *testing.T) {
	for _, bad := range []string{"", "300.1.1.1", "10.0.0.0/40", "example.com"} {
		if _, err := NewResolver([]string{bad}); err == nil {
			t.Fatalf("NewResolver accepted %q", bad)
		}
	}
}

func TestClientIP_WhitespaceInChain(t *testing.T) {
	r := mustResolver(t, "127.0.0.1")
	rq := httptest.NewRequest("GET", "/", nil)
	rq.RemoteAddr = "127.0.0.1:9000"
	rq.Header.Set("X-Forwarded-For", " 203.0.113.7 , 127.0.0.1 ")
	if got := r.ClientIP(rq); got != "203.0.113.7" {
		t.Fatalf("whitespace-padded hops must parse, got %q", got)
	}
}

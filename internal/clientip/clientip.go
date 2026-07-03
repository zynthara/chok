// Package clientip resolves the real client address behind reverse
// proxies — the stdlib replacement for gin's ClientIP/SetTrustedProxies
// pair (SPEC §4.2 item 4, security-sensitive: this value keys the
// account login rate limiter).
//
// Fail-closed by construction: with no trusted proxies configured the
// resolver always returns the direct socket peer, so a client cannot
// spoof X-Forwarded-For to escape IP-keyed rate limiting.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// headers consulted, in order, when the direct peer is trusted. Same
// set and order as gin's default RemoteIPHeaders.
var headers = []string{"X-Forwarded-For", "X-Real-IP"}

// Resolver derives the client IP from RemoteAddr plus forwarding
// headers, honouring headers only when the direct peer is a trusted
// proxy. Immutable after construction; safe for concurrent use.
type Resolver struct {
	trusted []netip.Prefix
}

// NewResolver parses the trusted proxy list (single IPs or CIDR
// blocks). An empty list is valid and means "trust nobody" — the
// fail-closed default. Malformed entries error out so a typo can't
// silently widen or narrow the trust boundary.
func NewResolver(trustedProxies []string) (*Resolver, error) {
	r := &Resolver{}
	for _, p := range trustedProxies {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("clientip: empty trusted_proxies entry")
		}
		if pfx, err := netip.ParsePrefix(p); err == nil {
			r.trusted = append(r.trusted, pfx.Masked())
			continue
		}
		if addr, err := netip.ParseAddr(p); err == nil {
			addr = addr.Unmap()
			r.trusted = append(r.trusted, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		return nil, fmt.Errorf("clientip: invalid trusted_proxies entry %q: must be IP or CIDR", p)
	}
	return r, nil
}

// ClientIP resolves the client address for req.
//
// Algorithm (gin-compatible):
//  1. The direct peer comes from RemoteAddr. When it is not a trusted
//     proxy, forwarding headers are ignored entirely and the peer IP
//     is returned — a client-sent X-Forwarded-For never wins.
//  2. When the peer is trusted, X-Forwarded-For is walked right to
//     left, skipping trusted proxies; the first untrusted hop is the
//     client. An all-trusted chain yields the leftmost entry. A
//     malformed entry invalidates the whole header (next header, then
//     the peer, is used instead).
//  3. X-Real-IP is consulted the same way when X-Forwarded-For yields
//     nothing.
//
// Returns "" only when RemoteAddr itself is unparseable and no header
// applies. Callers should skip IP-keyed decisions on "" rather than
// treating it as one shared bucket.
func (r *Resolver) ClientIP(req *http.Request) string {
	peer, ok := parseAddr(req.RemoteAddr)
	peerStr := ""
	if ok {
		peerStr = peer.String()
	}
	if !ok || !r.isTrusted(peer) {
		return peerStr
	}
	for _, h := range headers {
		if ip, ok := r.fromHeader(req.Header.Get(h)); ok {
			return ip
		}
	}
	return peerStr
}

// fromHeader walks a comma-separated forwarding chain from the
// rightmost (nearest proxy) leftward and returns the first hop that is
// not a trusted proxy. Mirrors gin's validateHeader.
func (r *Resolver) fromHeader(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	items := strings.Split(value, ",")
	for i := len(items) - 1; i >= 0; i-- {
		ipStr := strings.TrimSpace(items[i])
		addr, ok := parseAddr(ipStr)
		if !ok {
			// Malformed hop: the header cannot be trusted at all.
			break
		}
		if i == 0 || !r.isTrusted(addr) {
			return addr.String(), true
		}
	}
	return "", false
}

func (r *Resolver) isTrusted(addr netip.Addr) bool {
	for _, pfx := range r.trusted {
		if pfx.Contains(addr) {
			return true
		}
	}
	return false
}

// parseAddr accepts "ip:port", bare "ip", and IPv6 forms with or
// without brackets/zones, normalizing to a comparable address
// (IPv4-mapped IPv6 unmapped, zone dropped).
func parseAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	host := s
	if h, _, err := net.SplitHostPort(s); err == nil {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}

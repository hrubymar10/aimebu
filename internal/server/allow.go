package server

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

const DefaultAllowList = "127.0.0.0/8,::1/128"

// ResolveAllow accepts CIDRs and bare IPs; the empty value uses loopback.
func ResolveAllow(raw string) ([]netip.Prefix, error) {
	if raw == "" {
		raw = DefaultAllowList
	}
	return parsePrefixList("AIMEBU_ALLOW", raw)
}

// ResolveTrustedProxies parses AIMEBU_TRUSTED_PROXIES. The empty value trusts
// no proxy, so X-Forwarded-For is ignored entirely.
func ResolveTrustedProxies(raw string) ([]netip.Prefix, error) {
	if raw == "" {
		return nil, nil
	}
	return parsePrefixList("AIMEBU_TRUSTED_PROXIES", raw)
}

func parsePrefixList(name, raw string) ([]netip.Prefix, error) {
	prefixes := []netip.Prefix{}
	seen := map[netip.Prefix]bool{}
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			return nil, fmt.Errorf("%s=%q: empty entry", name, raw)
		}
		var prefix netip.Prefix
		if strings.Contains(entry, "/") {
			parsed, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("%s %q: %w", name, entry, err)
			}
			if parsed.Addr().Is4In6() {
				return nil, fmt.Errorf("%s %q: IPv4-mapped CIDRs are unsupported; use an IPv4 CIDR", name, entry)
			}
			prefix = parsed.Masked()
		} else {
			address, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, fmt.Errorf("%s %q is not an IP or CIDR: %w", name, entry, err)
			}
			address = address.Unmap()
			bits := 32
			if address.Is6() {
				bits = 128
			}
			prefix = netip.PrefixFrom(address, bits)
		}
		if !seen[prefix] {
			seen[prefix] = true
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes, nil
}

func prefixesContain(prefixes []netip.Prefix, address netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// clientIP identifies a caller for login, invite and pairing throttling only.
// It is the direct peer address unless that peer is a trusted proxy
// (AIMEBU_TRUSTED_PROXIES); then it is the right-most X-Forwarded-For entry
// that is not itself a trusted proxy. Entries left of it are client-supplied
// and never trusted. The allowlist never consults X-Forwarded-For.
func (s *Server) clientIP(r *http.Request) string {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		host, _, splitErr := net.SplitHostPort(r.RemoteAddr)
		if splitErr != nil {
			return r.RemoteAddr
		}
		return host
	}
	peer := remote.Addr().Unmap()
	if !prefixesContain(s.TrustedProxies, peer) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		entry := strings.TrimSpace(hops[i])
		if entry == "" {
			continue
		}
		address, err := netip.ParseAddr(entry)
		if err != nil {
			withPort, portErr := netip.ParseAddrPort(entry)
			if portErr != nil {
				return peer.String()
			}
			address = withPort.Addr()
		}
		address = address.Unmap()
		if !prefixesContain(s.TrustedProxies, address) {
			return address.String()
		}
	}
	return peer.String()
}

// allowMiddleware gates the whole HTTP handler, including WebSocket upgrades.
// This is a direct-connection service; forwarded headers are not trusted.
func allowMiddleware(next http.Handler, allow []netip.Prefix) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			log.Printf("WARN: blocked request with unparseable RemoteAddr %q: %v", r.RemoteAddr, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ip := remote.Addr().Unmap()
		for _, prefix := range allow {
			if prefix.Contains(ip) {
				next.ServeHTTP(w, r)
				return
			}
		}
		log.Printf("WARN: blocked %s — not in AIMEBU_ALLOW", ip)
		http.Error(w, "forbidden", http.StatusForbidden)
	})
}

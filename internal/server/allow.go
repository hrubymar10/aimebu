package server

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
)

const defaultAllowList = "127.0.0.0/8,::1/128"

// resolveAllow parses AIMEBU_ALLOW into a list of CIDR prefixes. Bare IPs
// are normalised to /32 (v4) or /128 (v6). Whitespace is trimmed; exact
// duplicate prefixes are dropped; stray empty entries (",,", trailing
// comma) are rejected as malformed. Empty / unset → defaultAllowList.
func resolveAllow() ([]netip.Prefix, error) {
	raw := os.Getenv("AIMEBU_ALLOW")
	if raw == "" {
		raw = defaultAllowList
	}
	parts := strings.Split(raw, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	seen := map[netip.Prefix]struct{}{}
	for _, p := range parts {
		entry := strings.TrimSpace(p)
		if entry == "" {
			return nil, fmt.Errorf("AIMEBU_ALLOW=%q: empty entry — drop the stray comma", raw)
		}
		var pref netip.Prefix
		if strings.Contains(entry, "/") {
			parsed, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("AIMEBU_ALLOW: %q: %w", entry, err)
			}
			pref = parsed.Masked()
		} else {
			addr, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, fmt.Errorf("AIMEBU_ALLOW: %q is not an IP or CIDR: %w", entry, err)
			}
			addr = addr.Unmap()
			bits := 32
			if addr.Is6() {
				bits = 128
			}
			pref = netip.PrefixFrom(addr, bits)
		}
		if _, dup := seen[pref]; dup {
			continue
		}
		seen[pref] = struct{}{}
		prefixes = append(prefixes, pref)
	}
	return prefixes, nil
}

// validateBindAddr enforces that the assembled host:port is an IP literal
// + valid port. Hostnames are rejected so AIMEBU_BIND pins to one address
// rather than whatever the resolver currently returns.
func validateBindAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("AIMEBU_BIND/AIMEBU_PORT %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("AIMEBU_BIND empty; set AIMEBU_BIND=0.0.0.0 to bind all interfaces")
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return fmt.Errorf("AIMEBU_BIND must be an IP literal (got %q): %w", host, err)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("AIMEBU_PORT invalid port %q: %w", port, err)
	}
	return nil
}

// allowMiddleware drops any request whose RemoteAddr IP is not contained
// in one of the allow prefixes. Direct-connection service:
// X-Forwarded-For is intentionally not trusted. 403 responses intentionally
// omit CORS headers — the IP gate is the canonical check.
func allowMiddleware(next http.Handler, allow []netip.Prefix) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			log.Printf("WARN: blocked request with unparseable RemoteAddr %q: %v", r.RemoteAddr, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ip := ap.Addr().Unmap()
		for _, p := range allow {
			if p.Contains(ip) {
				next.ServeHTTP(w, r)
				return
			}
		}
		log.Printf("WARN: blocked %s — not in AIMEBU_ALLOW", ip)
		http.Error(w, "forbidden", http.StatusForbidden)
	})
}

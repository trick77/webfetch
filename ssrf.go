package webfetch

import (
	"context"
	"fmt"
	"net"
	"syscall"
)

// guardedControl is a net.Dialer Control hook that rejects connections to
// non-public IP addresses. It runs after DNS resolution with the concrete IP
// the socket is about to connect to, so it also blocks DNS-rebinding to a
// private target.
//
// This replaces the SSRF protection that the deployment previously got for
// free by isolating the fetch sidecar on its own Docker network: with fetch
// now running in-process on the app network, a model-chosen (or prompt-
// injected) URL could otherwise reach loopback, RFC1918 hosts, or the cloud
// metadata endpoint (169.254.169.254). Blocking is done here, in the dialer,
// because that is the only place the real destination IP is known.
func guardedControl(network, address string, _ syscall.RawConn) error {
	if network != "tcp4" && network != "tcp6" && network != "tcp" {
		return fmt.Errorf("webfetch: refusing non-tcp network %q", network)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("webfetch: cannot parse dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("webfetch: refusing to dial unresolved host %q", host)
	}
	if !isPublicIP(ip) {
		return fmt.Errorf("webfetch: refusing to connect to non-public address %s", ip)
	}
	return nil
}

// specialUseRanges are IANA special-use / non-globally-routable prefixes that
// net's stdlib predicates (used in isPublicIP) do not already cover. Membership
// in any of these makes an address non-public. This is a default-deny model:
// only globally-routable unicast addresses outside every special-use range are
// allowed.
var specialUseRanges = mustParseCIDRs(
	// IPv4
	"0.0.0.0/8",       // "this host on this network"
	"100.64.0.0/10",   // carrier-grade NAT (RFC 6598)
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1 (documentation)
	"192.88.99.0/24",  // 6to4 relay anycast (deprecated)
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2 (documentation)
	"203.0.113.0/24",  // TEST-NET-3 (documentation)
	"240.0.0.0/4",     // reserved / future use (incl. 255.255.255.255)
	// IPv6
	"::/96",          // IPv4-compatible IPv6 (deprecated; e.g. ::127.0.0.1)
	"64:ff9b::/96",   // NAT64
	"64:ff9b:1::/48", // local-use NAT64
	"100::/64",       // discard-only
	"2001:db8::/32",  // documentation
	"2002::/16",      // 6to4
	"3fff::/20",      // documentation
	"5f00::/16",      // segment routing (SRv6)
)

// isPublicIP reports whether ip is a globally-routable public unicast address
// the fetcher is allowed to reach. It is a strict allowlist: loopback, private
// (RFC1918 + ULA fc00::/7), link-local (incl. the 169.254.169.254 metadata
// endpoint), unspecified, broadcast, multicast, and every IANA special-use
// range are rejected — only public unicast passes.
func isPublicIP(ip net.IP) bool {
	// Normalize IPv4-mapped IPv6 (e.g. ::ffff:127.0.0.1) to its IPv4 form so the
	// checks below cannot be bypassed via the mapped representation.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	// IsGlobalUnicast rejects unspecified, loopback, broadcast, multicast, and
	// link-local; IsPrivate rejects RFC1918 and IPv6 ULA (fc00::/7).
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, r := range specialUseRanges {
		if r.Contains(ip) {
			return false
		}
	}
	return true
}

// mustParseCIDRs parses the given CIDRs at init time, panicking on a malformed
// entry (which would be a programming error in the constant list above).
func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("webfetch: invalid special-use CIDR " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// dialControl is the net.Dialer Control hook used for all outbound requests. It
// is a package variable (defaulting to the SSRF guard) solely so in-package
// tests can relax it to reach a loopback httptest server; production always
// uses guardedControl.
var dialControl = guardedControl

// newDialContext returns a DialContext that enforces the SSRF guard.
func newDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Control: dialControl}
	return d.DialContext
}

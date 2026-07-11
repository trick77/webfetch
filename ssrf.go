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

// isPublicIP reports whether ip is a routable, non-internal address that the
// fetcher is allowed to reach. Anything loopback, private, link-local
// (including the 169.254.169.254 metadata endpoint), unspecified, or multicast
// is rejected.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// IPv4 broadcast and the "this network" 0.0.0.0/8 range are not covered by
	// the stdlib predicates above.
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 || v4.Equal(net.IPv4bcast) {
			return false
		}
	}
	return true
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

package tun

import (
	"net"
	"net/netip"

	"go4.org/netipx"
)

// RouteManager manages OS-level routes for auto_route mode.
type RouteManager interface {
	// Apply installs the given CIDR routes and policy routing rules.
	Apply(routes []netip.Prefix, prefix4, prefix6 netip.Prefix) error
	// Close removes all routes and rules added by Apply.
	Close() error
}

var (
	defaultIPv4Excludes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		// Tailscale CGNAT range (RFC 6598). Excluding it lets traffic to
		// Tailscale peers fall through our auto-route table and reach
		// Tailscale's own table 52 via its catch-all ip rule.
		netip.MustParsePrefix("100.64.0.0/10"),
	}
	defaultIPv6Excludes = []netip.Prefix{
		netip.MustParsePrefix("::/8"),
		netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("ff00::/8"),
	}
)

// BuildAutoRoutes computes the minimal CIDR set that covers universes minus excludes.
func BuildAutoRoutes(universes, excludes []netip.Prefix) ([]netip.Prefix, error) {
	builder := &netipx.IPSetBuilder{}
	for _, p := range universes {
		builder.AddPrefix(p)
	}
	for _, p := range excludes {
		builder.RemovePrefix(p)
	}
	set, err := builder.IPSet()
	if err != nil {
		return nil, err
	}
	return set.Prefixes(), nil
}

func prefixToIPNet(p netip.Prefix) *net.IPNet {
	addr := p.Addr()
	bits := p.Bits()
	if addr.Is4() {
		b4 := addr.As4()
		return &net.IPNet{
			IP:   net.IP(b4[:]),
			Mask: net.CIDRMask(bits, 32),
		}
	}
	b16 := addr.As16()
	return &net.IPNet{
		IP:   net.IP(b16[:]),
		Mask: net.CIDRMask(bits, 128),
	}
}

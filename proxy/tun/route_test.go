package tun

import (
	"net"
	"net/netip"
	"testing"
)

func TestBuildAutoRoutes(t *testing.T) {
	t.Run("IPv4 with default excludes", func(t *testing.T) {
		universes := []netip.Prefix{
			netip.MustParsePrefix("0.0.0.0/0"),
		}
		routes, err := BuildAutoRoutes(universes, defaultIPv4Excludes)
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) == 0 {
			t.Fatal("expected non-empty routes")
		}
		// 0.0.0.0/8 should be excluded, so routes should start at 1.0.0.0
		for _, r := range routes {
			if r.Bits() == 0 {
				t.Fatal("full /0 prefix should not appear after excludes")
			}
			if r.Addr().Is4() && r.Addr().As4()[0] == 0 {
				t.Fatalf("0.0.0.0/8 should be excluded, got %v", r)
			}
		}
	})

	t.Run("IPv6 with default excludes", func(t *testing.T) {
		universes := []netip.Prefix{
			netip.MustParsePrefix("::/0"),
		}
		routes, err := BuildAutoRoutes(universes, defaultIPv6Excludes)
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) == 0 {
			t.Fatal("expected non-empty routes")
		}
		// Verify excluded ranges are not covered
		for _, r := range routes {
			if r.Overlaps(netip.MustParsePrefix("::/8")) {
				t.Fatalf("::/8 should be excluded, got %v", r)
			}
			if r.Overlaps(netip.MustParsePrefix("fe80::/10")) {
				t.Fatalf("fe80::/10 should be excluded, got %v", r)
			}
			if r.Overlaps(netip.MustParsePrefix("fc00::/7")) {
				t.Fatalf("fc00::/7 should be excluded, got %v", r)
			}
			if r.Overlaps(netip.MustParsePrefix("ff00::/8")) {
				t.Fatalf("ff00::/8 should be excluded, got %v", r)
			}
		}
	})

	t.Run("combined IPv4 and IPv6", func(t *testing.T) {
		universes := []netip.Prefix{
			netip.MustParsePrefix("0.0.0.0/0"),
			netip.MustParsePrefix("::/0"),
		}
		excludes := append(defaultIPv4Excludes, defaultIPv6Excludes...)
		routes, err := BuildAutoRoutes(universes, excludes)
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) < 2 {
			t.Fatalf("expected at least 2 routes for dual-stack, got %d", len(routes))
		}
		hasV4, hasV6 := false, false
		for _, r := range routes {
			if r.Addr().Is4() {
				hasV4 = true
			}
			if r.Addr().Is6() {
				hasV6 = true
			}
		}
		if !hasV4 {
			t.Fatal("expected IPv4 routes")
		}
		if !hasV6 {
			t.Fatal("expected IPv6 routes")
		}
	})

	t.Run("no excludes returns universes", func(t *testing.T) {
		universes := []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
		}
		routes, err := BuildAutoRoutes(universes, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) != 1 {
			t.Fatalf("expected 1 route, got %d", len(routes))
		}
		if routes[0] != universes[0] {
			t.Fatalf("expected %v, got %v", universes[0], routes[0])
		}
	})

	t.Run("fully excluded universe returns empty", func(t *testing.T) {
		universes := []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
		}
		excludes := []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
		}
		routes, err := BuildAutoRoutes(universes, excludes)
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) != 0 {
			t.Fatalf("expected 0 routes, got %d", len(routes))
		}
	})

	t.Run("partially overlapping exclude", func(t *testing.T) {
		universes := []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
		}
		excludes := []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/16"),
		}
		routes, err := BuildAutoRoutes(universes, excludes)
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) == 0 {
			t.Fatal("expected non-empty routes after partial exclude")
		}
		for _, r := range routes {
			if r.Overlaps(netip.MustParsePrefix("10.0.0.0/16")) {
				t.Fatalf("10.0.0.0/16 should be excluded, got %v", r)
			}
		}
	})
}

func TestPrefixToIPNet(t *testing.T) {
	t.Run("IPv4 /24", func(t *testing.T) {
		p := netip.MustParsePrefix("192.168.1.0/24")
		ipNet := prefixToIPNet(p)
		expectedIP := net.IPv4(192, 168, 1, 0).To4()
		expectedMask := net.CIDRMask(24, 32)
		if !ipNet.IP.Equal(expectedIP) {
			t.Fatalf("expected IP %v, got %v", expectedIP, ipNet.IP)
		}
		if ipNet.Mask.String() != expectedMask.String() {
			t.Fatalf("expected mask %v, got %v", expectedMask, ipNet.Mask)
		}
	})

	t.Run("IPv4 /32", func(t *testing.T) {
		p := netip.MustParsePrefix("10.0.0.1/32")
		ipNet := prefixToIPNet(p)
		expectedIP := net.IPv4(10, 0, 0, 1).To4()
		expectedMask := net.CIDRMask(32, 32)
		if !ipNet.IP.Equal(expectedIP) {
			t.Fatalf("expected IP %v, got %v", expectedIP, ipNet.IP)
		}
		if ipNet.Mask.String() != expectedMask.String() {
			t.Fatalf("expected mask %v, got %v", expectedMask, ipNet.Mask)
		}
	})

	t.Run("IPv6 /64", func(t *testing.T) {
		p := netip.MustParsePrefix("2001:db8::/64")
		ipNet := prefixToIPNet(p)
		expectedIP := net.ParseIP("2001:db8::")
		expectedMask := net.CIDRMask(64, 128)
		if !ipNet.IP.Equal(expectedIP) {
			t.Fatalf("expected IP %v, got %v", expectedIP, ipNet.IP)
		}
		if ipNet.Mask.String() != expectedMask.String() {
			t.Fatalf("expected mask %v, got %v", expectedMask, ipNet.Mask)
		}
	})

	t.Run("IPv6 /128", func(t *testing.T) {
		p := netip.MustParsePrefix("::1/128")
		ipNet := prefixToIPNet(p)
		expectedIP := net.ParseIP("::1")
		expectedMask := net.CIDRMask(128, 128)
		if !ipNet.IP.Equal(expectedIP) {
			t.Fatalf("expected IP %v, got %v", expectedIP, ipNet.IP)
		}
		if ipNet.Mask.String() != expectedMask.String() {
			t.Fatalf("expected mask %v, got %v", expectedMask, ipNet.Mask)
		}
	})
}

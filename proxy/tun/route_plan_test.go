package tun

import (
	"bufio"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func TestPlanAutomaticSystemRoutes(t *testing.T) {
	tests := []struct {
		name     string
		includes []string
		excludes []string
		want4    []string
		want6    []string
	}{
		{name: "empty"},
		{name: "canonical and duplicate", includes: []string{"10.0.0.1/8", "10.0.0.0/9", "10.0.0.0/8"}, want4: []string{"10.0.0.0/8"}},
		{name: "adjacent merge", includes: []string{"10.0.0.0/9", "10.128.0.0/9"}, want4: []string{"10.0.0.0/8"}},
		{name: "nested exclusion", includes: []string{"10.0.0.0/8"}, excludes: []string{"10.64.0.0/10"}, want4: []string{"10.0.0.0/10", "10.128.0.0/9"}},
		{name: "covering exclusion", includes: []string{"10.0.0.0/8"}, excludes: []string{"0.0.0.0/0"}},
		{name: "disjoint exclusion", includes: []string{"10.0.0.0/8"}, excludes: []string{"192.0.2.0/24"}, want4: []string{"10.0.0.0/8"}},
		{name: "family isolation", includes: []string{"10.0.0.0/8", "2001:db8::/32"}, excludes: []string{"10.0.0.0/9", "2001:db8:8000::/33"}, want4: []string{"10.128.0.0/9"}, want6: []string{"2001:db8::/33"}},
		{name: "overlapping exclusions", includes: []string{"10.0.0.0/8"}, excludes: []string{"10.0.0.0/10", "10.32.0.0/11", "10.64.0.0/10"}, want4: []string{"10.128.0.0/9"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := PlanAutomaticSystemRoutes(test.includes, test.excludes, RouteCapacity{IPv4: 65536, IPv6: 65536})
			if err != nil {
				t.Fatal(err)
			}
			assertPrefixes(t, plan.IPv4, test.want4)
			assertPrefixes(t, plan.IPv6, test.want6)
			assertNoExcludedOverlap(t, append(plan.IPv4, plan.IPv6...), test.excludes)
		})
	}
}

func TestPlanAutomaticSystemRoutesRejectsInvalidCIDR(t *testing.T) {
	for _, input := range []struct {
		includes []string
		excludes []string
	}{
		{includes: []string{"not-a-cidr"}},
		{includes: []string{"10.0.0.0/8"}, excludes: []string{"bad"}},
	} {
		if _, err := PlanAutomaticSystemRoutes(input.includes, input.excludes, RouteCapacity{}); err == nil {
			t.Fatal("expected invalid CIDR error")
		}
	}
}

func TestPlanAutomaticSystemRoutesChecksFinalCapacity(t *testing.T) {
	plan, err := PlanAutomaticSystemRoutes([]string{"10.0.0.0/8", "10.0.0.0/9"}, nil, RouteCapacity{IPv4: 1, IPv6: 1})
	if err != nil || len(plan.IPv4) != 1 {
		t.Fatalf("normalized plan = %v, %v", plan, err)
	}

	_, err = PlanAutomaticSystemRoutes([]string{"10.0.0.0/8"}, []string{"10.0.0.1/32"}, RouteCapacity{IPv4: 8, IPv6: 8})
	if err == nil {
		t.Fatal("expected capacity error")
	}
	for _, fragment := range []string{"IPv4", "includes=1", "excludes=1", "generated=24", "limit=8"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("capacity error %q missing %q", err, fragment)
		}
	}
}

func FuzzPlanAutomaticSystemRoutesIPv4(f *testing.F) {
	f.Add(uint8(8), uint8(10), uint8(64))
	f.Fuzz(func(t *testing.T, includeBits, excludeBits, excludeThird uint8) {
		includeBits = 8 + includeBits%17
		excludeBits = includeBits + excludeBits%uint8(33-int(includeBits))
		include := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, 0, 0}), int(includeBits)).Masked()
		excludeAddr := netip.AddrFrom4([4]byte{10, 0, excludeThird, 0})
		exclude := netip.PrefixFrom(excludeAddr, int(excludeBits)).Masked()
		plan, err := PlanAutomaticSystemRoutes([]string{include.String()}, []string{exclude.String()}, RouteCapacity{IPv4: 65536, IPv6: 65536})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 1<<16; i += 257 {
			addr := netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1})
			want := include.Contains(addr) && !exclude.Contains(addr)
			got := prefixSetContains(plan.IPv4, addr)
			if got != want {
				t.Fatalf("membership %s = %v, want %v; plan=%v include=%s exclude=%s", addr, got, want, plan.IPv4, include, exclude)
			}
		}
	})
}

func FuzzPlanAutomaticSystemRoutesIPv6(f *testing.F) {
	f.Add(uint8(32), uint8(48), uint16(0x1234))
	f.Fuzz(func(t *testing.T, includeBits, excludeBits uint8, excludeSegment uint16) {
		includeBits = 32 + includeBits%17
		excludeBits = includeBits + excludeBits%uint8(65-int(includeBits))
		include := netip.PrefixFrom(netip.MustParseAddr("2001:db8::"), int(includeBits)).Masked()
		excludeAddress := [16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 4: byte(excludeSegment >> 8), 5: byte(excludeSegment)}
		exclude := netip.PrefixFrom(netip.AddrFrom16(excludeAddress), int(excludeBits)).Masked()
		plan, err := PlanAutomaticSystemRoutes([]string{include.String()}, []string{exclude.String()}, RouteCapacity{IPv4: 65536, IPv6: 65536})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 1<<16; i += 257 {
			address := netip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 4: byte(i >> 8), 5: byte(i), 15: 1})
			want := include.Contains(address) && !exclude.Contains(address)
			got := prefixSetContains(plan.IPv6, address)
			if got != want {
				t.Fatalf("membership %s = %v, want %v; plan=%v include=%s exclude=%s", address, got, want, plan.IPv6, include, exclude)
			}
		}
	})
}

func BenchmarkPlanAutomaticSystemRoutesAdversarial(b *testing.B) {
	for b.Loop() {
		_, err := PlanAutomaticSystemRoutes([]string{"0.0.0.0/0"}, []string{"10.0.0.1/32", "172.16.0.1/32", "192.168.0.1/32"}, RouteCapacity{IPv4: 65536, IPv6: 65536})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestPlanAutomaticSystemRoutesLargeFixture(t *testing.T) {
	file, err := os.Open("testdata/chn-iplist/chnroute.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var routes []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		routes = append(routes, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 9206 {
		t.Fatalf("fixture entries = %d, want 9206", len(routes))
	}

	plan, err := PlanAutomaticSystemRoutes(routes, nil, RouteCapacity{IPv4: 65536, IPv6: 65536})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.IPv4) == 0 || len(plan.IPv6) == 0 || len(plan.IPv4)+len(plan.IPv6) > len(routes) {
		t.Fatalf("unexpected large plan counts: IPv4=%d IPv6=%d", len(plan.IPv4), len(plan.IPv6))
	}
	t.Logf("large route fixture normalized from %d to IPv4=%d IPv6=%d routes", len(routes), len(plan.IPv4), len(plan.IPv6))
}

func BenchmarkPlanAutomaticSystemRoutesLargeFixture(b *testing.B) {
	data, err := os.ReadFile("testdata/chn-iplist/chnroute.txt")
	if err != nil {
		b.Fatal(err)
	}
	routes := strings.Fields(string(data))
	b.ResetTimer()
	for b.Loop() {
		if _, err := PlanAutomaticSystemRoutes(routes, nil, RouteCapacity{IPv4: 65536, IPv6: 65536}); err != nil {
			b.Fatal(err)
		}
	}
}

func assertPrefixes(t *testing.T, got []netip.Prefix, want []string) {
	t.Helper()
	gotStrings := make([]string, len(got))
	for i, prefix := range got {
		gotStrings[i] = prefix.String()
	}
	if strings.Join(gotStrings, ",") != strings.Join(want, ",") {
		t.Fatalf("prefixes = %v, want %v", gotStrings, want)
	}
}

func assertNoExcludedOverlap(t *testing.T, routes []netip.Prefix, excludes []string) {
	t.Helper()
	for _, value := range excludes {
		excluded, err := netip.ParsePrefix(value)
		if err != nil {
			continue
		}
		excluded = excluded.Masked()
		for _, route := range routes {
			if prefixesOverlap(route, excluded) {
				t.Fatalf("route %s overlaps exclusion %s", route, excluded)
			}
		}
	}
}

func prefixSetContains(prefixes []netip.Prefix, address netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

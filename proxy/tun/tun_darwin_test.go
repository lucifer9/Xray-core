//go:build darwin

package tun

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

func TestSelectDarwinGatewayDefault(t *testing.T) {
	gateway, err := selectDarwinGateway(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := gateway.String(); got != defaultDarwinGateway {
		t.Fatalf("unexpected default gateway: got %s, want %s", got, defaultDarwinGateway)
	}
}

func TestApplyDarwinSystemRoutesRollsBackInstalledRoutesInReverse(t *testing.T) {
	routes := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
	var deleted []netip.Prefix
	addCount := 0
	execute := func(messageType, _ int, destination, _ netip.Prefix) error {
		if messageType == unix.RTM_ADD {
			addCount++
			if addCount == 3 {
				return errors.New("install failed")
			}
		} else {
			deleted = append(deleted, destination)
		}
		return nil
	}
	installed, err := applyDarwinSystemRoutes(42, routes, netip.MustParsePrefix(defaultDarwinGateway), execute)
	if err == nil || len(installed) != 0 {
		t.Fatalf("installed=%v error=%v", installed, err)
	}
	if len(deleted) != 2 || deleted[0] != routes[1] || deleted[1] != routes[0] {
		t.Fatalf("rollback order = %v", deleted)
	}
}

func TestSetInterfaceBindsIPv6SocketWithoutIPv4Option(t *testing.T) {
	iface, err := net.InterfaceByName("lo0")
	if err != nil {
		t.Fatal(err)
	}

	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	if err := setinterface("udp6", "[::]:0", uintptr(fd), iface); err != nil {
		t.Fatal(err)
	}
}

func TestBuildDarwinSystemRoutesAppliesExclusionsAfterProtectedDefaultExpansion(t *testing.T) {
	routes, err := buildDarwinSystemRoutes([]string{"0.0.0.0/0", "::/0"}, []string{"100.64.0.0/10", "2001:db8::/32"})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) == 0 {
		t.Fatal("protected default expansion returned no routes")
	}
	for _, route := range routes {
		if strings.HasPrefix(route.String(), "0.0.0.0/0") || strings.HasPrefix(route.String(), "::/0") {
			t.Fatalf("unprotected default route returned: %s", route)
		}
		for _, excluded := range []string{"100.64.0.0/10", "2001:db8::/32"} {
			prefix := mustPrefix(t, excluded)
			if prefixesOverlap(route, prefix) {
				t.Fatalf("route %s overlaps exclusion %s", route, prefix)
			}
		}
	}
}

func TestBuildDarwinSystemRoutesEmptyExclusionsPreserveNonDefaultRoutes(t *testing.T) {
	routes, err := buildDarwinSystemRoutes([]string{"10.0.0.1/8", "2001:db8::/32"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0].String() != "10.0.0.0/8" || routes[1].String() != "2001:db8::/32" {
		t.Fatalf("routes = %v", routes)
	}
}

func TestSelectDarwinDefaultRouteSkipsUnusableEarlierRoute(t *testing.T) {
	messages := []route.Message{
		darwinDefaultRouteMessage(carrierIPv4, 10),
		darwinDefaultRouteMessage(carrierIPv4, 20),
	}
	lookup := func(index int) (*net.Interface, error) {
		if index == 10 {
			return &net.Interface{Index: index, Name: "down0"}, nil
		}
		return &net.Interface{Index: index, Name: "en0", Flags: net.FlagUp}, nil
	}

	iface, err := selectDarwinDefaultRoute(messages, carrierIPv4, 99, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if iface.Index != 20 {
		t.Fatalf("selected interface = %+v, want index 20", iface)
	}
}

func darwinDefaultRouteMessage(family carrierFamily, index int) *route.RouteMessage {
	message := &route.RouteMessage{Index: index, Flags: unix.RTF_UP | unix.RTF_GATEWAY}
	if family == carrierIPv4 {
		message.Addrs = []route.Addr{
			unix.RTAX_DST:     &route.Inet4Addr{},
			unix.RTAX_NETMASK: &route.Inet4Addr{},
		}
	} else {
		message.Addrs = []route.Addr{
			unix.RTAX_DST:     &route.Inet6Addr{},
			unix.RTAX_NETMASK: &route.Inet6Addr{},
		}
	}
	return message
}

func mustPrefix(t *testing.T, value string) netip.Prefix {
	t.Helper()
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		t.Fatal(err)
	}
	return prefix.Masked()
}

func TestSelectDarwinGatewayConfiguredIPv4(t *testing.T) {
	gateway, err := selectDarwinGateway([]string{"198.18.0.1/15"})
	if err != nil {
		t.Fatal(err)
	}
	if got := gateway.String(); got != "198.18.0.1/15" {
		t.Fatalf("unexpected gateway: got %s", got)
	}
}

func TestSelectDarwinGatewaySkipsIPv6(t *testing.T) {
	gateway, err := selectDarwinGateway([]string{"fc00::1/64", "198.18.0.1/15"})
	if err != nil {
		t.Fatal(err)
	}
	if got := gateway.String(); got != "198.18.0.1/15" {
		t.Fatalf("unexpected gateway: got %s", got)
	}
}

func TestSelectDarwinGatewayRequiresIPv4(t *testing.T) {
	if _, err := selectDarwinGateway([]string{"fc00::1/64"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestSelectDarwinGatewayRequiresUsableLocalAddress(t *testing.T) {
	if _, err := selectDarwinGateway([]string{"198.18.0.1/32"}); err == nil {
		t.Fatal("expected error")
	}
}

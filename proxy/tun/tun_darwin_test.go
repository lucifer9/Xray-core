//go:build darwin

package tun

import (
	"net"
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

//go:build linux && !android

package tun

import (
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

func TestLinuxDirectEchoSocketSmoke(t *testing.T) {
	if os.Getenv("XRAY_TUN_PRIVILEGED_SMOKE") != "1" {
		t.Skip("set XRAY_TUN_PRIVILEGED_SMOKE=1 and grant CAP_NET_RAW or run as root")
	}
	for _, family := range []carrierFamily{carrierIPv4, carrierIPv6} {
		iface, err := findOutboundInterface(family, -1, "")
		if err != nil {
			t.Fatalf("select %s Outbound carrier interface: %v", familyLabel(family), err)
		}
		echoFamily := echoIPv6
		if family == carrierIPv4 {
			echoFamily = echoIPv4
		}
		packet, err := openEchoPacketIO(echoFamily, carrierInterface{Name: iface.Name, Index: iface.Index})
		if err != nil {
			t.Fatalf("open %s Direct Echo transport: %v", familyLabel(family), err)
		}
		if err := packet.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLinuxDirectEchoRoundTripSmoke(t *testing.T) {
	if os.Getenv("XRAY_TUN_PRIVILEGED_SMOKE") != "1" {
		t.Skip("set XRAY_TUN_PRIVILEGED_SMOKE=1, grant CAP_NET_RAW, and provide reachable default gateways")
	}
	policy := &outboundCarrierPolicy{subscribers: make(map[uint64]func(carrierFamily, *carrierInterface))}
	targets := make(map[echoFamily]netip.Addr)
	for _, family := range []carrierFamily{carrierIPv4, carrierIPv6} {
		iface, err := findOutboundInterface(family, -1, "")
		if err != nil {
			t.Fatalf("select %s carrier: %v", familyLabel(family), err)
		}
		policy.update(family, iface)
		target := netip.MustParseAddr("1.1.1.1")
		if family == carrierIPv6 {
			target = netip.MustParseAddr("2606:4700:4700::1111")
		}
		routes, err := netlink.RouteGet(target.AsSlice())
		if err != nil || len(routes) == 0 || routes[0].Gw == nil {
			t.Fatalf("resolve %s default gateway: routes=%v error=%v", familyLabel(family), routes, err)
		}
		gateway, ok := netip.AddrFromSlice(routes[0].Gw)
		if !ok {
			t.Fatalf("invalid %s gateway %v", familyLabel(family), routes[0].Gw)
		}
		echoFamily := echoIPv6
		if family == carrierIPv4 {
			echoFamily = echoIPv4
		}
		targets[echoFamily] = gateway.Unmap()
	}

	engine, err := newDirectEchoEngineWithLimits(policy, newPlatformEchoTransportFactory(), directEchoLimits{Timeout: 3 * time.Second, MaxPending: 4, MaxPendingFamily: 2, RatePerSecond: 10, RateBurst: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	for _, family := range []echoFamily{echoIPv4, echoIPv6} {
		result := make(chan echoResult, 1)
		if err := engine.Probe(echoRequest{Family: family, Destination: targets[family], Identifier: 0x5842, Sequence: uint16(family), Payload: []byte("xray-direct-echo-smoke")}, func(value echoResult) { result <- value }); err != nil {
			t.Fatal(err)
		}
		select {
		case value := <-result:
			if value.Err != nil || value.Reply == nil {
				t.Fatalf("%d-bit Direct Echo prerequisite failed: %+v", family, value)
			}
		case <-time.After(4 * time.Second):
			t.Fatalf("%d-bit Direct Echo did not complete", family)
		}
	}
}

func TestLinuxAutomaticSystemRouteSmoke(t *testing.T) {
	if os.Getenv("XRAY_TUN_PRIVILEGED_SMOKE") != "1" {
		t.Skip("set XRAY_TUN_PRIVILEGED_SMOKE=1 and run as root")
	}
	tunInterface, err := NewTun(&Config{
		Name:                          "xraytst0",
		MTU:                           1500,
		AutoSystemRoutingTable:        []string{"198.18.0.0/24"},
		AutoSystemRoutingTableExclude: []string{"198.18.0.0/25"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tunInterface.Close()
	if err := tunInterface.Start(); err != nil {
		t.Fatal(err)
	}
	index, err := tunInterface.Index()
	if err != nil {
		t.Fatal(err)
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{LinkIndex: index}, netlink.RT_FILTER_OIF)
	if err != nil {
		t.Fatal(err)
	}
	want := netip.MustParsePrefix("198.18.0.128/25")
	for _, route := range routes {
		if route.Dst != nil {
			prefix, parseErr := netip.ParsePrefix(route.Dst.String())
			if parseErr == nil && prefix.Masked() == want {
				return
			}
		}
	}
	t.Fatalf("planned route %s not installed; routes=%v", want, routes)
}

func TestLinuxOutboundCarrierTrackingSmoke(t *testing.T) {
	if os.Getenv("XRAY_TUN_PRIVILEGED_SMOKE") != "1" {
		t.Skip("set XRAY_TUN_PRIVILEGED_SMOKE=1 and run as root")
	}
	tunInterface, err := NewTun(&Config{Name: "xraytrk0", MTU: 1500})
	if err != nil {
		t.Fatal(err)
	}
	defer tunInterface.Close()
	policy, err := acquireOutboundCarrierPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	tracker := tunInterface.(outboundCarrierTracker)
	if err := tracker.StartOutboundCarrierTracking(policy, ""); err != nil {
		t.Fatal(err)
	}
	defer tracker.StopOutboundCarrierTracking()
	if policy.carrier(carrierIPv4) == nil || policy.carrier(carrierIPv6) == nil {
		t.Fatalf("initial carrier snapshot incomplete: IPv4=%+v IPv6=%+v", policy.carrier(carrierIPv4), policy.carrier(carrierIPv6))
	}
	if err := tunInterface.Start(); err != nil {
		t.Fatal(err)
	}
}

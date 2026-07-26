package tun

import (
	"net"
	"testing"
)

func TestSelectWindowsDefaultRouteUsesCombinedMetric(t *testing.T) {
	selected, err := selectWindowsDefaultRoute([]windowsRouteCandidate{
		{Interface: net.Interface{Index: 1, Name: "Wi-Fi", Flags: net.FlagUp}, RouteMetric: 50, InterfaceMetric: 20},
		{Interface: net.Interface{Index: 2, Name: "Ethernet", Flags: net.FlagUp}, RouteMetric: 5, InterfaceMetric: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Index != 2 {
		t.Fatalf("selected interface = %v, want Ethernet route with lower combined metric", selected)
	}
}

func TestSelectWindowsDefaultRouteRejectsUnusableInterfaces(t *testing.T) {
	selected, err := selectWindowsDefaultRoute([]windowsRouteCandidate{
		{Interface: net.Interface{Index: 1, Name: "down"}, RouteMetric: 0},
		{Interface: net.Interface{Index: 2, Name: "loopback", Flags: net.FlagUp | net.FlagLoopback}, RouteMetric: 0},
		{Interface: net.Interface{Index: 3, Name: "usable", Flags: net.FlagUp}, RouteMetric: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Index != 3 {
		t.Fatalf("selected interface = %v, want usable interface", selected)
	}
}

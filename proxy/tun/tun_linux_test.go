//go:build linux && !android

package tun

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestLinuxSystemRouteRollbackDeletesOnlyInstalledRoutesInReverse(t *testing.T) {
	oldAdd, oldDelete := addLinuxRoute, deleteLinuxRoute
	t.Cleanup(func() { addLinuxRoute, deleteLinuxRoute = oldAdd, oldDelete })
	var added, deleted []string
	addLinuxRoute = func(route *netlink.Route) error {
		added = append(added, route.Dst.String())
		if len(added) == 3 {
			return errors.New("install failed")
		}
		return nil
	}
	deleteLinuxRoute = func(route *netlink.Route) error {
		deleted = append(deleted, route.Dst.String())
		return nil
	}

	tunInterface := &LinuxTun{
		tunLink: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42}},
		plannedRoutes: []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
			netip.MustParsePrefix("172.16.0.0/12"),
			netip.MustParsePrefix("192.168.0.0/16"),
		},
	}
	if err := tunInterface.setSystemRoutes(); err == nil {
		t.Fatal("expected route installation failure")
	}
	if len(tunInterface.systemRoutes) != 0 {
		t.Fatalf("recorded routes after rollback = %v", tunInterface.systemRoutes)
	}
	if len(deleted) != 2 || deleted[0] != "172.16.0.0/12" || deleted[1] != "10.0.0.0/8" {
		t.Fatalf("rollback order = %v", deleted)
	}
}

func TestLinuxDirectEchoBindingUsesCarrierName(t *testing.T) {
	var gotFD int
	var gotName string
	err := bindLinuxDirectEchoSocket(42, carrierInterface{Name: "eth-test", Index: 7}, func(fd int, name string) error {
		gotFD, gotName = fd, name
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotFD != 42 || gotName != "eth-test" {
		t.Fatalf("binding = fd %d name %q", gotFD, gotName)
	}
}

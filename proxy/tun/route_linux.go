//go:build linux && !android

package tun

import (
	"math/rand/v2"
	"net"
	"net/netip"

	xrayerrors "github.com/xtls/xray-core/common/errors"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

type linuxRouteManager struct {
	tunName  string
	tunIndex int
	table    int
	routes   []*netlink.Route
	rules    []*netlink.Rule
}

func NewRouteManager(tunName string, tunIndex int) (RouteManager, error) {
	table := 1000 + rand.IntN(60000)
	return &linuxRouteManager{
		tunName:  tunName,
		tunIndex: tunIndex,
		table:    table,
	}, nil
}

func (m *linuxRouteManager) Apply(routes []netip.Prefix, gateway4, gateway6 netip.Addr) error {
	for _, r := range routes {
		rt := &netlink.Route{
			LinkIndex: m.tunIndex,
			Dst:       prefixToIPNet(r),
			Table:     m.table,
		}
		if r.Addr().Is4() {
			if !gateway4.IsValid() {
				continue
			}
			b4 := gateway4.As4()
			rt.Gw = net.IP(b4[:])
			rt.Family = unix.AF_INET
		} else {
			if !gateway6.IsValid() {
				continue
			}
			b6 := gateway6.As16()
			rt.Gw = net.IP(b6[:])
			rt.Family = unix.AF_INET6
		}
		if err := netlink.RouteAdd(rt); err != nil {
			// Handle EEXIST: delete and re-add
			_ = netlink.RouteDel(rt)
			if err := netlink.RouteAdd(rt); err != nil {
				return xrayerrors.New("failed to add route").Base(err)
			}
		}
		m.routes = append(m.routes, rt)
	}

	// Rule 1: iif=tun → NOP (prevent routing loops)
	rule1 := netlink.NewRule()
	rule1.IifName = m.tunName
	rule1.Type = nl.FR_ACT_NOP
	rule1.Priority = 100
	if err := netlink.RuleAdd(rule1); err != nil {
		return xrayerrors.New("failed to add loopback rule").Base(err)
	}
	m.rules = append(m.rules, rule1)

	// Rule 2: iif=lo + src=tun_IP → custom table (locally generated traffic)
	if gateway4.IsValid() {
		rule2 := netlink.NewRule()
		rule2.IifName = "lo"
		b4 := gateway4.As4()
		rule2.Src = &net.IPNet{
			IP:   net.IP(b4[:]),
			Mask: net.CIDRMask(32, 32),
		}
		rule2.Table = m.table
		rule2.Priority = 150
		if err := netlink.RuleAdd(rule2); err != nil {
			return xrayerrors.New("failed to add lo IPv4 rule").Base(err)
		}
		m.rules = append(m.rules, rule2)
	}
	if gateway6.IsValid() {
		rule3 := netlink.NewRule()
		rule3.IifName = "lo"
		b6 := gateway6.As16()
		rule3.Src = &net.IPNet{
			IP:   net.IP(b6[:]),
			Mask: net.CIDRMask(128, 128),
		}
		rule3.Table = m.table
		rule3.Priority = 151
		if err := netlink.RuleAdd(rule3); err != nil {
			return xrayerrors.New("failed to add lo IPv6 rule").Base(err)
		}
		m.rules = append(m.rules, rule3)
	}

	// Rule 3: NOT iif=lo → custom table (all non-loopback outbound traffic)
	rule4 := netlink.NewRule()
	rule4.Invert = true
	rule4.IifName = "lo"
	rule4.Table = m.table
	rule4.Priority = 200
	if err := netlink.RuleAdd(rule4); err != nil {
		return xrayerrors.New("failed to add non-lo rule").Base(err)
	}
	m.rules = append(m.rules, rule4)

	return nil
}

func (m *linuxRouteManager) Close() error {
	var errs []error
	for _, rt := range m.routes {
		if err := netlink.RouteDel(rt); err != nil {
			errs = append(errs, err)
		}
	}
	for _, rule := range m.rules {
		if err := netlink.RuleDel(rule); err != nil {
			errs = append(errs, err)
		}
	}
	return xrayerrors.Combine(errs...)
}

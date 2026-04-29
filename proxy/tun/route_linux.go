//go:build linux && !android

package tun

import (
	"fmt"
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

func (m *linuxRouteManager) Apply(routes []netip.Prefix, prefix4, prefix6 netip.Prefix) error {
	// Add routes (LinkIndex only, no Gw)
	for _, r := range routes {
		rt := &netlink.Route{
			LinkIndex: m.tunIndex,
			Dst:       prefixToIPNet(r),
			Table:     m.table,
		}
		if r.Addr().Is4() {
			if !prefix4.IsValid() {
				continue
			}
			rt.Family = unix.AF_INET
		} else {
			if !prefix6.IsValid() {
				continue
			}
			rt.Family = unix.AF_INET6
		}
		if err := netlink.RouteAdd(rt); err != nil {
			_ = netlink.RouteDel(rt)
			if err := netlink.RouteAdd(rt); err != nil {
				return xrayerrors.New("failed to add route: dst=" + r.String() + " table=" + fmt.Sprint(m.table) + " link=" + fmt.Sprint(m.tunIndex)).Base(err)
			}
		}
		m.routes = append(m.routes, rt)
	}

	// NOTE: vishvananda/netlink checks `rule.Goto >= 0` and overrides
	// msg.Type with FR_ACT_GOTO. Go zero-value for int is 0, which
	// triggers this. All rules MUST set Goto: -1 to prevent the
	// library from hijacking the action field.

	// IPv4 policy routing rules
	if prefix4.IsValid() {
		// iif=tun → NOP (prevent routing loops)
		rule := &netlink.Rule{
			Priority: 100, IifName: m.tunName,
			Family: unix.AF_INET, Type: nl.FR_ACT_NOP, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv4 loop prevention rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// not iif=lo → custom table (forwarded / non-local traffic)
		rule = &netlink.Rule{
			Priority: 110, Invert: true, IifName: "lo",
			Table: m.table, Family: unix.AF_INET, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv4 forwarded traffic rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// iif=lo src=0.0.0.0/32 → custom table (unspecified source)
		rule = &netlink.Rule{
			Priority: 110, IifName: "lo",
			Src:   &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(32, 32)},
			Table: m.table, Family: unix.AF_INET, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv4 unspecified source rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// iif=lo src=tun_subnet → custom table
		rule = &netlink.Rule{
			Priority: 110, IifName: "lo",
			Src: prefixToIPNet(prefix4.Masked()),
			Table: m.table, Family: unix.AF_INET, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv4 tun subnet rule").Base(err)
		}
		m.rules = append(m.rules, rule)
	}

	// IPv6 policy routing rules
	if prefix6.IsValid() {
		// iif=tun → NOP (prevent routing loops)
		rule := &netlink.Rule{
			Priority: 100, IifName: m.tunName,
			Family: unix.AF_INET6, Type: nl.FR_ACT_NOP, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 loop prevention rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// iif=lo src=::/1 → NOP (skip 0000:: - 7fff:ffff...)
		rule = &netlink.Rule{
			Priority: 100, IifName: "lo",
			Src:    &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(1, 128)},
			Family: unix.AF_INET6, Type: nl.FR_ACT_NOP, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 ::/1 rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// iif=lo src=8000::/1 → NOP (skip 8000:: - ffff:ffff...)
		rule = &netlink.Rule{
			Priority: 100, IifName: "lo",
			Src: &net.IPNet{
				IP:   net.IP{0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				Mask: net.CIDRMask(1, 128),
			},
			Family: unix.AF_INET6, Type: nl.FR_ACT_NOP, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 8000::/1 rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// iif=lo src=tun_subnet → custom table
		rule = &netlink.Rule{
			Priority: 111, IifName: "lo",
			Src: prefixToIPNet(prefix6.Masked()),
			Table: m.table, Family: unix.AF_INET6, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 tun subnet rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// catch-all → custom table (all remaining IPv6 traffic)
		rule = &netlink.Rule{
			Priority: 112,
			Table:    m.table, Family: unix.AF_INET6, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 catch-all rule").Base(err)
		}
		m.rules = append(m.rules, rule)
	}

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

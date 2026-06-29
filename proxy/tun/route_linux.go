//go:build linux && !android

package tun

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	xrayerrors "github.com/xtls/xray-core/common/errors"
	"golang.org/x/sys/unix"
)

const autoRouteBypassPriority = 120

type linuxRouteManager struct {
	tunName       string
	tunIndex      int
	table         int
	routes        []*netlink.Route
	rules         []*netlink.Rule
	cleanupScript string
}

func NewRouteManager(tunName string, tunIndex int) (RouteManager, error) {
	table := 1000 + rand.IntN(60000)
	return &linuxRouteManager{
		tunName:  tunName,
		tunIndex: tunIndex,
		table:    table,
	}, nil
}

func newICMPBypassRule(family int) *netlink.Rule {
	mask := uint32(0xffffffff)
	rule := netlink.NewRule()
	rule.Priority = 90
	rule.Mark = icmpBypassMark
	rule.Mask = &mask
	rule.Table = unix.RT_TABLE_MAIN
	rule.Family = family
	return rule
}

// newMarkedBypassRule builds the rule that lets traffic already carrying a
// non-zero fwmark skip auto-route capture. Such traffic belongs to another
// fwmark-based policy router (notably Tailscale, which tags its own outbound
// with 0x80000 and escapes via its own "fwmark 0x80000 lookup main" rule) and
// must fall through to that tool's rules instead of being pulled into our tun
// table.
//
// "fwmark 0/0xffffffff" matches fwmark==0; Invert turns it into fwmark!=0.
// The goto jumps over all capture rules (pref 110-112) to the NOP target
// (pref 120), after which the lookup continues to lower-priority rules such
// as Tailscale's. This is what makes auto-route coexist with Tailscale
// regardless of ip-rule priority ordering or which daemon started first.
//
// We use a single bypass rule instead of fwmark-filtering each capture rule
// because the forwarded-traffic rule uses Invert ("not iif=lo"); adding a
// fwmark clause there would yield OR semantics ("iif!=lo OR mark!=0") and
// still capture marked traffic.
func newMarkedBypassRule(family int) *netlink.Rule {
	mask := uint32(0xffffffff)
	rule := netlink.NewRule()
	rule.Priority = 105
	rule.Mark = 0
	rule.Mask = &mask
	rule.Invert = true
	rule.Family = family
	rule.Goto = autoRouteBypassPriority
	return rule
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
		// ICMP forwarder sockets are marked so their real outbound echo
		// requests use the system routing table instead of being captured
		// again by auto-route.
		rule := newICMPBypassRule(unix.AF_INET)
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv4 ICMP bypass rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// iif=tun → skip auto-route rules (prevent routing loops)
		rule = &netlink.Rule{
			Priority: 100, IifName: m.tunName,
			Family: unix.AF_INET, Goto: autoRouteBypassPriority,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv4 loop prevention rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// fwmark!=0 (e.g. Tailscale's 0x80000) → skip auto-route so the
		// marking tool's own rules handle it. See newMarkedBypassRule.
		rule = newMarkedBypassRule(unix.AF_INET)
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv4 marked-bypass rule").Base(err)
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
			Src:   prefixToIPNet(prefix4.Masked()),
			Table: m.table, Family: unix.AF_INET, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv4 tun subnet rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// GOTO target used by rules that must bypass auto-route capture.
		rule = &netlink.Rule{
			Priority: autoRouteBypassPriority,
			Family:   unix.AF_INET, Type: nl.FR_ACT_NOP, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv4 bypass target rule").Base(err)
		}
		m.rules = append(m.rules, rule)
	}

	// IPv6 policy routing rules
	if prefix6.IsValid() {
		// ICMP forwarder sockets are marked so their real outbound echo
		// requests use the system routing table instead of being captured
		// again by auto-route.
		rule := newICMPBypassRule(unix.AF_INET6)
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 ICMP bypass rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// iif=tun → skip auto-route rules (prevent routing loops)
		rule = &netlink.Rule{
			Priority: 100, IifName: m.tunName,
			Family: unix.AF_INET6, Goto: autoRouteBypassPriority,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 loop prevention rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// iif=lo src=::/1 → skip auto-route rules (0000:: - 7fff:ffff...)
		rule = &netlink.Rule{
			Priority: 100, IifName: "lo",
			Src:    &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(1, 128)},
			Family: unix.AF_INET6, Goto: autoRouteBypassPriority,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 ::/1 rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// iif=lo src=8000::/1 → skip auto-route rules (8000:: - ffff:ffff...)
		rule = &netlink.Rule{
			Priority: 100, IifName: "lo",
			Src: &net.IPNet{
				IP:   net.IP{0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				Mask: net.CIDRMask(1, 128),
			},
			Family: unix.AF_INET6, Goto: autoRouteBypassPriority,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 8000::/1 rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// fwmark!=0 (e.g. Tailscale's 0x80000) → skip auto-route so the
		// marking tool's own rules handle it. See newMarkedBypassRule.
		rule = newMarkedBypassRule(unix.AF_INET6)
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 marked-bypass rule").Base(err)
		}
		m.rules = append(m.rules, rule)

		// iif=lo src=tun_subnet → custom table
		rule = &netlink.Rule{
			Priority: 111, IifName: "lo",
			Src:   prefixToIPNet(prefix6.Masked()),
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

		// GOTO target used by rules that must bypass auto-route capture.
		rule = &netlink.Rule{
			Priority: autoRouteBypassPriority,
			Family:   unix.AF_INET6, Type: nl.FR_ACT_NOP, Goto: -1,
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return xrayerrors.New("failed to add IPv6 bypass target rule").Base(err)
		}
		m.rules = append(m.rules, rule)
	}

	m.ensureCleanupScript(context.Background())
	return nil
}

func (m *linuxRouteManager) Close() error {
	ctx := context.Background()
	m.ensureCleanupScript(ctx)

	var errs []error
	for i := len(m.rules) - 1; i >= 0; i-- {
		if err := netlink.RuleDel(m.rules[i]); err != nil {
			errs = append(errs, err)
		}
	}
	for _, rt := range m.routes {
		if err := netlink.RouteDel(rt); err != nil {
			errs = append(errs, err)
		}
	}
	if err := xrayerrors.Combine(errs...); err != nil {
		logAutoRouteCleanupScript(ctx, m.cleanupScript)
		if m.cleanupScript != "" {
			return xrayerrors.New("auto_route cleanup may be incomplete; run manually if needed: ", autoRouteCleanupCommand(m.cleanupScript)).Base(err)
		}
		return err
	}
	removeAutoRouteCleanupScript(ctx, m.cleanupScript)
	m.cleanupScript = ""
	return nil
}

func (m *linuxRouteManager) ensureCleanupScript(ctx context.Context) {
	if m.cleanupScript != "" {
		return
	}
	commands := m.cleanupCommands()
	if len(commands) == 0 {
		return
	}
	path, err := writeAutoRouteCleanupScript(ctx, m.tunName, commands)
	if err != nil {
		xrayerrors.LogWarningInner(ctx, err, "[tun] failed to write auto_route cleanup script")
		return
	}
	m.cleanupScript = path
}

func (m *linuxRouteManager) cleanupCommands() []string {
	if len(m.routes) == 0 && len(m.rules) == 0 {
		return nil
	}

	commands := []string{"# Linux auto_route cleanup"}
	for i := len(m.rules) - 1; i >= 0; i-- {
		if command := linuxRuleDeleteCommand(m.rules[i]); command != "" {
			commands = append(commands, command)
		}
	}
	commands = append(commands,
		fmt.Sprintf("ip route flush table %d || true", m.table),
		fmt.Sprintf("ip -6 route flush table %d || true", m.table),
	)
	return commands
}

func linuxRuleDeleteCommand(rule *netlink.Rule) string {
	if rule == nil {
		return ""
	}

	family := ""
	switch rule.Family {
	case unix.AF_INET:
		family = "-4"
	case unix.AF_INET6:
		family = "-6"
	default:
		return ""
	}

	parts := []string{"ip", family, "rule", "del"}
	if rule.Priority >= 0 {
		parts = append(parts, "priority", fmt.Sprint(rule.Priority))
	}
	if rule.Invert {
		parts = append(parts, "not")
	}
	if rule.Mark != 0 || rule.Mask != nil {
		if rule.Mask != nil {
			parts = append(parts, "fwmark", fmt.Sprintf("0x%x/0x%x", rule.Mark, *rule.Mask))
		} else {
			parts = append(parts, "fwmark", fmt.Sprintf("0x%x", rule.Mark))
		}
	}
	if rule.IifName != "" {
		parts = append(parts, "iif", shellQuote(rule.IifName))
	}
	if rule.Src != nil {
		parts = append(parts, "from", rule.Src.String())
	}
	if rule.Goto >= 0 {
		parts = append(parts, "goto", fmt.Sprint(rule.Goto))
	}
	if rule.Table != 0 {
		parts = append(parts, "table", fmt.Sprint(rule.Table))
	}

	return strings.Join(parts, " ") + " || true"
}

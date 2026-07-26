package tun

import "net/netip"

type windowsAutomaticRoute struct {
	Destination netip.Prefix
	NextHop     netip.Addr
}

func buildWindowsAutomaticRoutes(prefixes []netip.Prefix) ([]windowsAutomaticRoute, bool, bool) {
	routes := make([]windowsAutomaticRoute, 0, len(prefixes))
	var hasIPv4, hasIPv6 bool
	for _, prefix := range prefixes {
		route := windowsAutomaticRoute{Destination: prefix.Masked()}
		if prefix.Addr().Is4() {
			hasIPv4 = true
			route.NextHop = netip.IPv4Unspecified()
		} else {
			hasIPv6 = true
			route.NextHop = netip.IPv6Unspecified()
		}
		routes = append(routes, route)
	}
	return routes, hasIPv4, hasIPv6
}

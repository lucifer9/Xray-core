package tun

import (
	"fmt"
	"net/netip"
	"sort"
)

const maxAutomaticSystemRoutesPerFamily = 65536

// RouteCapacity limits the final normalized route count for each address family.
// A zero limit disables the capacity check for that family.
type RouteCapacity struct {
	IPv4 int
	IPv6 int
}

// AutomaticSystemRoutePlan is the normalized set of routes Xray should install.
type AutomaticSystemRoutePlan struct {
	IPv4 []netip.Prefix
	IPv6 []netip.Prefix
}

// PlanAutomaticSystemRoutes computes configured Automatic system routes minus
// Excluded route prefixes.
func PlanAutomaticSystemRoutes(includes, excludes []string, capacity RouteCapacity) (AutomaticSystemRoutePlan, error) {
	include4, include6, err := parseRoutePrefixes("Automatic system route", includes)
	if err != nil {
		return AutomaticSystemRoutePlan{}, err
	}
	exclude4, exclude6, err := parseRoutePrefixes("Excluded route prefix", excludes)
	if err != nil {
		return AutomaticSystemRoutePlan{}, err
	}

	plan := AutomaticSystemRoutePlan{
		IPv4: subtractPrefixSets(normalizePrefixes(include4), normalizePrefixes(exclude4)),
		IPv6: subtractPrefixSets(normalizePrefixes(include6), normalizePrefixes(exclude6)),
	}
	if err := checkRouteCapacity("IPv4", len(include4), len(exclude4), len(plan.IPv4), capacity.IPv4); err != nil {
		return AutomaticSystemRoutePlan{}, err
	}
	if err := checkRouteCapacity("IPv6", len(include6), len(exclude6), len(plan.IPv6), capacity.IPv6); err != nil {
		return AutomaticSystemRoutePlan{}, err
	}
	return plan, nil
}

func parseRoutePrefixes(kind string, values []string) ([]netip.Prefix, []netip.Prefix, error) {
	ipv4 := make([]netip.Prefix, 0, len(values))
	ipv6 := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid %s %q: %w", kind, value, err)
		}
		prefix = prefix.Masked()
		if prefix.Addr().Is4() {
			ipv4 = append(ipv4, prefix)
		} else {
			ipv6 = append(ipv6, prefix)
		}
	}
	return ipv4, ipv6, nil
}

func checkRouteCapacity(family string, includeCount, excludeCount, generatedCount, limit int) error {
	if limit > 0 && generatedCount > limit {
		return fmt.Errorf("Automatic system route capacity exceeded for %s: includes=%d excludes=%d generated=%d limit=%d", family, includeCount, excludeCount, generatedCount, limit)
	}
	return nil
}

func normalizePrefixes(prefixes []netip.Prefix) []netip.Prefix {
	if len(prefixes) == 0 {
		return nil
	}
	prefixes = append([]netip.Prefix(nil), prefixes...)
	for i := range prefixes {
		prefixes[i] = prefixes[i].Masked()
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if comparison := prefixes[i].Addr().Compare(prefixes[j].Addr()); comparison != 0 {
			return comparison < 0
		}
		return prefixes[i].Bits() < prefixes[j].Bits()
	})

	result := make([]netip.Prefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		if len(result) > 0 && result[len(result)-1].Contains(prefix.Addr()) {
			continue
		}
		result = append(result, prefix)
		for len(result) >= 2 {
			left := result[len(result)-2]
			right := result[len(result)-1]
			parent, ok := siblingParent(left, right)
			if !ok {
				break
			}
			result = result[:len(result)-2]
			if len(result) > 0 && result[len(result)-1].Contains(parent.Addr()) {
				break
			}
			result = append(result, parent)
		}
	}
	return result
}

func siblingParent(left, right netip.Prefix) (netip.Prefix, bool) {
	if left.Addr().BitLen() != right.Addr().BitLen() || left.Bits() == 0 || left.Bits() != right.Bits() {
		return netip.Prefix{}, false
	}
	leftParent := netip.PrefixFrom(left.Addr(), left.Bits()-1).Masked()
	rightParent := netip.PrefixFrom(right.Addr(), right.Bits()-1).Masked()
	return leftParent, leftParent == rightParent && left != right
}

func subtractPrefixSets(includes, excludes []netip.Prefix) []netip.Prefix {
	if len(includes) == 0 || len(excludes) == 0 {
		return includes
	}
	result := make([]netip.Prefix, 0, len(includes))
	for _, include := range includes {
		result = append(result, subtractPrefix(include, excludes)...)
	}
	return normalizePrefixes(result)
}

func subtractPrefix(prefix netip.Prefix, excludes []netip.Prefix) []netip.Prefix {
	relevant := make([]netip.Prefix, 0, len(excludes))
	for _, excluded := range excludes {
		if !prefixesOverlap(prefix, excluded) {
			continue
		}
		if excluded.Bits() <= prefix.Bits() && excluded.Contains(prefix.Addr()) {
			return nil
		}
		relevant = append(relevant, excluded)
	}
	if len(relevant) == 0 {
		return []netip.Prefix{prefix}
	}

	left, right := splitPrefix(prefix)
	result := subtractPrefix(left, relevant)
	result = append(result, subtractPrefix(right, relevant)...)
	return result
}

func prefixesOverlap(left, right netip.Prefix) bool {
	if left.Addr().BitLen() != right.Addr().BitLen() {
		return false
	}
	return left.Contains(right.Addr()) || right.Contains(left.Addr())
}

func splitPrefix(prefix netip.Prefix) (netip.Prefix, netip.Prefix) {
	childBits := prefix.Bits() + 1
	left := netip.PrefixFrom(prefix.Addr(), childBits).Masked()
	if prefix.Addr().Is4() {
		address := prefix.Addr().As4()
		setAddressBit(address[:], prefix.Bits())
		return left, netip.PrefixFrom(netip.AddrFrom4(address), childBits).Masked()
	}
	address := prefix.Addr().As16()
	setAddressBit(address[:], prefix.Bits())
	return left, netip.PrefixFrom(netip.AddrFrom16(address), childBits).Masked()
}

func setAddressBit(address []byte, bit int) {
	address[bit/8] |= 1 << (7 - bit%8)
}

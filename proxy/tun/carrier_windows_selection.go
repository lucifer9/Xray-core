package tun

import (
	"net"
	"sort"
)

type windowsRouteCandidate struct {
	Interface       net.Interface
	RouteMetric     uint32
	InterfaceMetric uint32
}

func selectWindowsDefaultRoute(candidates []windowsRouteCandidate) (*net.Interface, error) {
	usable := candidates[:0]
	for _, candidate := range candidates {
		if candidate.Interface.Flags&net.FlagUp == 0 || candidate.Interface.Flags&net.FlagLoopback != 0 {
			continue
		}
		usable = append(usable, candidate)
	}
	if len(usable) == 0 {
		return nil, errNoOutboundCarrier
	}
	sort.Slice(usable, func(i, j int) bool {
		left := uint64(usable[i].RouteMetric) + uint64(usable[i].InterfaceMetric)
		right := uint64(usable[j].RouteMetric) + uint64(usable[j].InterfaceMetric)
		if left != right {
			return left < right
		}
		return usable[i].Interface.Index < usable[j].Interface.Index
	})
	selected := usable[0].Interface
	return &selected, nil
}

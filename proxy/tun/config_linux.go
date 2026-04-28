//go:build linux && !android

package tun

import (
	"context"
	"net"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Update finds the best outbound interface.
// When fixedName is empty (auto mode), it reads the main routing table to find the default gateway.
// Falls back to common traversal logic if no default route is found.
func (updater *InterfaceUpdater) Update() {
	updater.Lock()
	defer updater.Unlock()

	if updater.iface != nil {
		iface, err := net.InterfaceByIndex(updater.iface.Index)
		if err == nil && iface.Name == updater.iface.Name {
			return
		}
	}

	updater.iface = nil

	if updater.fixedName == "" {
		// Auto mode: try to find default gateway via netlink
		if updater.findDefaultRoute() {
			return
		}
	}

	// Fallback: traverse interfaces
	updater.updateFallback()
}

func (updater *InterfaceUpdater) findDefaultRoute() bool {
	// Try IPv4 first, then IPv6
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{
			Table: unix.RT_TABLE_MAIN,
		}, netlink.RT_FILTER_TABLE)
		if err != nil {
			continue
		}
		for _, route := range routes {
			// Default route: Dst == nil (or 0.0.0.0/0), Gateway != nil
			if route.Dst == nil && route.Gw != nil && route.LinkIndex != updater.tunIndex {
				iface, err := net.InterfaceByIndex(route.LinkIndex)
				if err == nil {
					updater.iface = iface
					errors.LogInfo(context.Background(), "[tun] update interface (via route) ", iface.Name, " ", iface.Index)
					return true
				}
			}
		}
	}
	return false
}

func (updater *InterfaceUpdater) updateFallback() {
	interfaces, err := net.Interfaces()
	if err != nil {
		errors.LogInfoInner(context.Background(), err, "[tun] failed to update interface")
		return
	}

	var got *net.Interface
	for _, iface := range interfaces {
		if iface.Index == updater.tunIndex {
			continue
		}
		if updater.fixedName != "" {
			if iface.Name == updater.fixedName {
				got = &iface
				break
			}
		} else {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			if (iface.Flags&net.FlagUp != 0) &&
				(iface.Flags&net.FlagLoopback == 0) &&
				len(addrs) > 0 {
				got = &iface
				break
			}
		}
	}

	if got == nil {
		errors.LogInfo(context.Background(), "[tun] failed to update interface > got == nil")
		return
	}

	updater.iface = got
	errors.LogInfo(context.Background(), "[tun] update interface ", got.Name, " ", got.Index)
}

// StartMonitor subscribes to netlink route and link updates and triggers Update() after a 1s debounce.
func (updater *InterfaceUpdater) StartMonitor() {
	updater.monitorStop = make(chan struct{})

	routeCh := make(chan netlink.RouteUpdate, 16)
	linkCh := make(chan netlink.LinkUpdate, 16)

	if err := netlink.RouteSubscribe(routeCh, updater.monitorStop); err != nil {
		errors.LogInfoInner(context.Background(), err, "[tun] failed to subscribe route updates")
		return
	}
	if err := netlink.LinkSubscribe(linkCh, updater.monitorStop); err != nil {
		errors.LogInfoInner(context.Background(), err, "[tun] failed to subscribe link updates")
		return
	}

	go func() {
		timer := time.NewTimer(0)
		timer.Stop()
		defer timer.Stop()

		for {
			select {
			case <-updater.monitorStop:
				return
			case <-routeCh:
				timer.Reset(time.Second)
			case <-linkCh:
				timer.Reset(time.Second)
			case <-timer.C:
				updater.Update()
			}
		}
	}()

	errors.LogInfo(context.Background(), "[tun] network monitor started")
}

// StopMonitor stops the network change monitor.
func (updater *InterfaceUpdater) StopMonitor() {
	if updater.monitorStop != nil {
		close(updater.monitorStop)
		updater.monitorStop = nil
	}
}

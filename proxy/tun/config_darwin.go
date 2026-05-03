//go:build darwin

package tun

import (
	"context"
	"net"
	"syscall"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// Update finds the best outbound interface.
// When fixedName is empty (auto mode), it reads the kernel RIB to find the default gateway.
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
		// Auto mode: try to find default gateway via RIB
		if updater.findDefaultRoute() {
			return
		}
	}

	// Fallback: traverse interfaces
	updater.updateFallback()
}

func (updater *InterfaceUpdater) findDefaultRoute() bool {
	data, err := route.FetchRIB(syscall.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return false
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, data)
	if err != nil {
		return false
	}
	index := defaultRouteInterfaceIndex(msgs, updater.tunIndex)
	if index == 0 {
		return false
	}
	iface, err := net.InterfaceByIndex(index)
	if err == nil {
		updater.iface = iface
		errors.LogInfo(context.Background(), "[tun] update interface (via RIB) ", iface.Name, " ", iface.Index)
		return true
	}
	return false
}

func defaultRouteInterfaceIndex(msgs []route.Message, tunIndex int) int {
	ipv6Index := 0
	for _, msg := range msgs {
		rm, ok := msg.(*route.RouteMessage)
		if !ok {
			continue
		}
		// Skip tun interface
		if rm.Index == tunIndex {
			continue
		}
		// Check for default route: UP + GATEWAY flags
		if rm.Flags&syscall.RTF_UP == 0 || rm.Flags&syscall.RTF_GATEWAY == 0 {
			continue
		}
		// Check destination is default (0.0.0.0 or ::)
		if len(rm.Addrs) == 0 || rm.Addrs[0] == nil || !isDefaultAddr(rm.Addrs[0]) {
			continue
		}
		if _, ok := rm.Addrs[0].(*route.Inet4Addr); ok {
			return rm.Index
		}
		if ipv6Index == 0 {
			ipv6Index = rm.Index
		}
	}
	return ipv6Index
}

func isDefaultAddr(addr route.Addr) bool {
	switch a := addr.(type) {
	case *route.Inet4Addr:
		return a.IP == [4]byte{0, 0, 0, 0}
	case *route.Inet6Addr:
		return a.IP == [16]byte{}
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

// StartMonitor opens an AF_ROUTE socket and triggers Update() on route changes with 1s debounce.
func (updater *InterfaceUpdater) StartMonitor() {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		errors.LogInfoInner(context.Background(), err, "[tun] failed to open route socket")
		return
	}

	updater.routeFd = fd
	updater.monitorStop = make(chan struct{})

	go func() {
		timer := time.NewTimer(0)
		timer.Stop()
		defer timer.Stop()

		buf := make([]byte, 4096)
		for {
			n, err := unix.Read(fd, buf)
			if err != nil {
				select {
				case <-updater.monitorStop:
					return
				default:
					time.Sleep(100 * time.Millisecond)
					continue
				}
			}
			if n > 0 {
				timer.Reset(time.Second)
			}
			select {
			case <-timer.C:
				updater.Update()
			case <-updater.monitorStop:
				return
			}
		}
	}()

	errors.LogInfo(context.Background(), "[tun] network monitor started")
}

// StopMonitor stops the network change monitor by closing the monitor channel and route socket.
func (updater *InterfaceUpdater) StopMonitor() {
	if updater.monitorStop != nil {
		close(updater.monitorStop)
		updater.monitorStop = nil
	}
	if updater.routeFd > 0 {
		unix.Close(updater.routeFd)
		updater.routeFd = 0
	}
}

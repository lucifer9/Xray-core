//go:build (!linux && !darwin) || android

package tun

import (
	"context"
	"net"

	"github.com/xtls/xray-core/common/errors"
)

// Update finds the best outbound interface by traversing system interfaces.
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

// StartMonitor is a no-op on this platform.
func (updater *InterfaceUpdater) StartMonitor() {}

// StopMonitor is a no-op on this platform.
func (updater *InterfaceUpdater) StopMonitor() {}

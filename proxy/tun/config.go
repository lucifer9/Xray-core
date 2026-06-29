package tun

import (
	"net"
	"sync"
)

type InterfaceUpdater struct {
	sync.Mutex

	tunIndex    int
	fixedName   string
	iface       *net.Interface
	monitorStop chan struct{}
	routeFd     int // AF_ROUTE socket fd (macOS only), closed by StopMonitor to unblock read
}

var updater *InterfaceUpdater

func (updater *InterfaceUpdater) Get() *net.Interface {
	updater.Lock()
	defer updater.Unlock()

	return updater.iface
}

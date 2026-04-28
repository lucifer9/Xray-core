//go:build darwin

package tun

import (
	"net"
	"net/netip"
	"syscall"

	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

type darwinRouteManager struct {
	fd     int
	routes []*route.RouteMessage
}

func NewRouteManager(tunName string, tunIndex int) (RouteManager, error) {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return nil, errors.New("failed to open route socket").Base(err)
	}
	return &darwinRouteManager{fd: fd}, nil
}

func (m *darwinRouteManager) Apply(routes []netip.Prefix, gateway4, gateway6 netip.Addr) error {
	for _, r := range routes {
		rm := &route.RouteMessage{
			Version: syscall.RTM_VERSION,
			Type:    syscall.RTM_ADD,
			Flags:   syscall.RTF_UP | syscall.RTF_GATEWAY | syscall.RTF_STATIC,
		}

		if r.Addr().Is4() {
			if !gateway4.IsValid() {
				continue
			}
			rm.Addrs = []route.Addr{
				&route.Inet4Addr{IP: r.Addr().As4()},
				&route.Inet4Addr{IP: gateway4.As4()},
				&route.Inet4Addr{IP: netmask4(r.Bits())},
			}
		} else {
			if !gateway6.IsValid() {
				continue
			}
			rm.Addrs = []route.Addr{
				&route.Inet6Addr{IP: r.Addr().As16()},
				&route.Inet6Addr{IP: gateway6.As16()},
				&route.Inet6Addr{IP: netmask6(r.Bits())},
			}
		}

		if err := m.writeRouteMessage(rm); err != nil {
			// Handle EEXIST: delete first, then re-add
			rm.Type = syscall.RTM_DELETE
			_ = m.writeRouteMessage(rm)
			rm.Type = syscall.RTM_ADD
			if err := m.writeRouteMessage(rm); err != nil {
				return errors.New("failed to add route").Base(err)
			}
		}

		m.routes = append(m.routes, rm)
	}

	return nil
}

func (m *darwinRouteManager) Close() error {
	for _, rm := range m.routes {
		rm.Type = syscall.RTM_DELETE
		_ = m.writeRouteMessage(rm) // Ignore ENOENT
	}
	unix.Close(m.fd)
	return nil
}

func (m *darwinRouteManager) writeRouteMessage(rm *route.RouteMessage) error {
	data, err := rm.Marshal()
	if err != nil {
		return err
	}
	_, err = unix.Write(m.fd, data)
	return err
}

func netmask4(bits int) [4]byte {
	m := net.CIDRMask(bits, 32)
	var mask [4]byte
	copy(mask[:], m)
	return mask
}

func netmask6(bits int) [16]byte {
	m := net.CIDRMask(bits, 128)
	var mask [16]byte
	copy(mask[:], m)
	return mask
}

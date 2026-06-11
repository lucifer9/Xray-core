//go:build darwin

package tun

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

type darwinRouteManager struct {
	fd            int
	routes        []*route.RouteMessage
	cleanupRoutes []darwinCleanupRoute
	cleanupScript string
}

type darwinCleanupRoute struct {
	prefix  netip.Prefix
	gateway netip.Addr
}

func NewRouteManager(tunName string, tunIndex int) (RouteManager, error) {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return nil, errors.New("failed to open route socket").Base(err)
	}
	return &darwinRouteManager{fd: fd}, nil
}

func (m *darwinRouteManager) Apply(routes []netip.Prefix, prefix4, prefix6 netip.Prefix) error {
	for _, r := range routes {
		rm := &route.RouteMessage{
			Version: syscall.RTM_VERSION,
			Type:    syscall.RTM_ADD,
			Flags:   syscall.RTF_UP | syscall.RTF_GATEWAY | syscall.RTF_STATIC,
		}

		var cleanupRoute darwinCleanupRoute
		if r.Addr().Is4() {
			if !prefix4.IsValid() {
				continue
			}
			cleanupRoute = darwinCleanupRoute{prefix: r.Masked(), gateway: prefix4.Addr()}
			rm.Addrs = []route.Addr{
				&route.Inet4Addr{IP: r.Addr().As4()},
				&route.Inet4Addr{IP: prefix4.Addr().As4()},
				&route.Inet4Addr{IP: netmask4(r.Bits())},
			}
		} else {
			if !prefix6.IsValid() {
				continue
			}
			cleanupRoute = darwinCleanupRoute{prefix: r.Masked(), gateway: prefix6.Addr()}
			rm.Addrs = []route.Addr{
				&route.Inet6Addr{IP: r.Addr().As16()},
				&route.Inet6Addr{IP: prefix6.Addr().As16()},
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
		m.cleanupRoutes = append(m.cleanupRoutes, cleanupRoute)
	}

	m.ensureCleanupScript(context.Background())
	return nil
}

func (m *darwinRouteManager) Close() error {
	ctx := context.Background()
	m.ensureCleanupScript(ctx)

	var errs []error
	for _, rm := range m.routes {
		rm.Type = syscall.RTM_DELETE
		if err := m.writeRouteMessage(rm); err != nil {
			errs = append(errs, err)
		}
	}
	_ = unix.Close(m.fd)
	if err := errors.Combine(errs...); err != nil {
		logAutoRouteCleanupScript(ctx, m.cleanupScript)
		if m.cleanupScript != "" {
			return errors.New("auto_route cleanup may be incomplete; run manually if needed: ", autoRouteCleanupCommand(m.cleanupScript)).Base(err)
		}
		return err
	}
	removeAutoRouteCleanupScript(ctx, m.cleanupScript)
	m.cleanupScript = ""
	return nil
}

func (m *darwinRouteManager) ensureCleanupScript(ctx context.Context) {
	if m.cleanupScript != "" {
		return
	}
	commands := m.cleanupCommands()
	if len(commands) == 0 {
		return
	}
	path, err := writeAutoRouteCleanupScript(ctx, "utun", commands)
	if err != nil {
		errors.LogWarningInner(ctx, err, "[tun] failed to write auto_route cleanup script")
		return
	}
	m.cleanupScript = path
}

func (m *darwinRouteManager) cleanupCommands() []string {
	if len(m.cleanupRoutes) == 0 {
		return nil
	}

	commands := []string{"# macOS auto_route cleanup"}
	for i := len(m.cleanupRoutes) - 1; i >= 0; i-- {
		route := m.cleanupRoutes[i]
		family := "-inet6"
		if route.prefix.Addr().Is4() {
			family = "-inet"
		}
		commands = append(commands, fmt.Sprintf("route -n delete %s -net %s %s || true", family, shellQuote(route.prefix.String()), shellQuote(route.gateway.String())))
	}
	return commands
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

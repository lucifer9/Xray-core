//go:build windows

package tun

import (
	"context"
	"net/netip"
	"syscall"

	"github.com/xtls/xray-core/common/errors"
)

// Windows ICMP socket constants not exported by syscall.
const (
	windowsIPPROTO_ICMP   = 1
	windowsIPPROTO_ICMPV6 = 58
	windowsSO_RCVTIMEO    = 0x1006
	windowsWSAETIMEDOUT   = 10060
)

type windowsRawSocket struct {
	fd syscall.Handle
}

func createRawSocket(dst netip.Addr, ctx context.Context) (rawSocket, error) {
	var domain, proto int
	if dst.Is4() {
		domain = syscall.AF_INET
		proto = windowsIPPROTO_ICMP
	} else {
		domain = syscall.AF_INET6
		proto = windowsIPPROTO_ICMPV6
	}

	fd, err := syscall.Socket(domain, syscall.SOCK_RAW, proto)
	if err != nil {
		return nil, err
	}

	// Bind to physical interface to prevent routing loop through TUN
	if updater != nil {
		iface := updater.Get()
		if iface != nil {
			network := "ip4"
			if dst.Is6() {
				network = "ip6"
			}
			if bindErr := setinterface(network, "", uintptr(fd), iface); bindErr != nil {
				errors.LogInfo(ctx, "icmp: failed to bind to interface: ", bindErr)
			}
		}
	}

	// Connect to destination so recv() only receives packets from this host
	if dst.Is4() {
		sa := &syscall.SockaddrInet4{}
		sa.Addr = dst.As4()
		err = syscall.Connect(fd, sa)
	} else {
		sa := &syscall.SockaddrInet6{}
		sa.Addr = dst.As16()
		err = syscall.Connect(fd, sa)
	}
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}

	// Windows SO_RCVTIMEO takes milliseconds
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, windowsSO_RCVTIMEO, int(icmpReadTimeout*1000))

	return &windowsRawSocket{fd: fd}, nil
}

func (s *windowsRawSocket) send(data []byte) error {
	return syscall.Sendto(s.fd, data, 0, nil)
}

func (s *windowsRawSocket) recv(buf []byte) (int, error) {
	for {
		n, _, err := syscall.Recvfrom(s.fd, buf, 0)
		if err != nil {
			if err == syscall.Errno(windowsWSAETIMEDOUT) {
				continue
			}
			return 0, err
		}
		return n, nil
	}
}

func (s *windowsRawSocket) close() {
	syscall.Close(s.fd)
}

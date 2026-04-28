//go:build !windows

package tun

import (
	"context"
	"net/netip"

	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/sys/unix"
)

type unixRawSocket struct {
	fd int
}

func createRawSocket(dst netip.Addr, ctx context.Context) (rawSocket, error) {
	var domain, proto int
	if dst.Is4() {
		domain = unix.AF_INET
		proto = unix.IPPROTO_ICMP
	} else {
		domain = unix.AF_INET6
		proto = unix.IPPROTO_ICMPV6
	}

	fd, err := unix.Socket(domain, unix.SOCK_RAW, proto)
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
		sa := &unix.SockaddrInet4{}
		sa.Addr = dst.As4()
		err = unix.Connect(fd, sa)
	} else {
		sa := &unix.SockaddrInet6{}
		sa.Addr = dst.As16()
		err = unix.Connect(fd, sa)
	}
	if err != nil {
		unix.Close(fd)
		return nil, err
	}

	// Set read timeout to avoid blocking forever
	tv := unix.Timeval{Sec: icmpReadTimeout}
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)

	return &unixRawSocket{fd: fd}, nil
}

func (s *unixRawSocket) send(data []byte) error {
	_, err := unix.Write(s.fd, data)
	return err
}

func (s *unixRawSocket) recv(buf []byte) (int, error) {
	for {
		n, err := unix.Read(s.fd, buf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return 0, err
		}
		return n, nil
	}
}

func (s *unixRawSocket) close() {
	unix.Close(s.fd)
}

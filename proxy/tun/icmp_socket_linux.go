//go:build linux && !android

package tun

import (
	"context"
	"net/netip"

	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

type linuxICMPSocket struct {
	fd         int
	isDatagram bool
	dst        netip.Addr
	ident      uint16
}

func createRawSocket(dst netip.Addr, ctx context.Context) (rawSocket, error) {
	domain := unix.AF_INET
	proto := unix.IPPROTO_ICMP
	if dst.Is6() {
		domain = unix.AF_INET6
		proto = unix.IPPROTO_ICMPV6
	}

	fd, err := unix.Socket(domain, unix.SOCK_DGRAM, proto)
	isDatagram := true
	if err != nil {
		fd, err = unix.Socket(domain, unix.SOCK_RAW, proto)
		isDatagram = false
		if err != nil {
			return nil, err
		}
	}

	if err = enableICMPBypassRouting(fd); err != nil {
		unix.Close(fd)
		return nil, err
	}

	// Bind to physical interface to prevent routing loop through TUN.
	if updater != nil {
		if iface := updater.Get(); iface != nil {
			network := "ip4"
			if dst.Is6() {
				network = "ip6"
			}
			if bindErr := setinterface(network, "", uintptr(fd), iface); bindErr != nil {
				errors.LogInfo(ctx, "icmp: failed to bind to interface: ", bindErr)
			}
		}
	}

	if dst.Is4() {
		sa := &unix.SockaddrInet4{Addr: dst.As4()}
		err = unix.Connect(fd, sa)
	} else {
		sa := &unix.SockaddrInet6{Addr: dst.As16()}
		err = unix.Connect(fd, sa)
	}
	if err != nil {
		unix.Close(fd)
		return nil, err
	}

	tv := unix.Timeval{Sec: icmpReadTimeout}
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)

	return &linuxICMPSocket{fd: fd, isDatagram: isDatagram, dst: dst}, nil
}

func enableICMPBypassRouting(fd int) error {
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_MARK, icmpBypassMark); err != nil && err != unix.EPERM {
		return err
	}
	return nil
}

func (s *linuxICMPSocket) send(data []byte) error {
	if s.isDatagram {
		if s.dst.Is4() && len(data) >= header.ICMPv4MinimumSize {
			s.ident = header.ICMPv4(data).Ident()
		} else if s.dst.Is6() && len(data) >= header.ICMPv6MinimumSize {
			s.ident = header.ICMPv6(data).Ident()
		}
	}
	_, err := unix.Write(s.fd, data)
	return err
}

func (s *linuxICMPSocket) recv(buf []byte) (int, error) {
	if s.isDatagram {
		return s.recvDatagram(buf)
	}
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

func (s *linuxICMPSocket) recvDatagram(buf []byte) (int, error) {
	headerSize := header.IPv4MinimumSize
	if s.dst.Is6() {
		headerSize = header.IPv6MinimumSize
	}
	if len(buf) <= headerSize {
		return 0, unix.ENOBUFS
	}

	for {
		n, err := unix.Read(s.fd, buf[headerSize:])
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return 0, err
		}
		message := buf[headerSize : headerSize+n]
		if s.dst.Is4() {
			if len(message) < header.ICMPv4MinimumSize {
				return 0, unix.EINVAL
			}
			icmpHdr := header.ICMPv4(message)
			icmpHdr.SetIdent(s.ident)

			ipHdr := header.IPv4(buf[:header.IPv4MinimumSize])
			ipHdr.Encode(&header.IPv4Fields{
				TotalLength: uint16(header.IPv4MinimumSize + n),
				TTL:         64,
				Protocol:    uint8(header.ICMPv4ProtocolNumber),
				SrcAddr:     tcpip.AddrFrom4(s.dst.As4()),
			})
			ipHdr.SetChecksum(^ipHdr.CalculateChecksum())
		} else {
			if len(message) < header.ICMPv6MinimumSize {
				return 0, unix.EINVAL
			}
			icmpHdr := header.ICMPv6(message)
			icmpHdr.SetIdent(s.ident)

			ipHdr := header.IPv6(buf[:header.IPv6MinimumSize])
			ipHdr.Encode(&header.IPv6Fields{
				PayloadLength:     uint16(n),
				TransportProtocol: header.ICMPv6ProtocolNumber,
				HopLimit:          64,
				SrcAddr:           tcpip.AddrFrom16(s.dst.As16()),
			})
		}
		return headerSize + n, nil
	}
}

func (s *linuxICMPSocket) close() {
	unix.Close(s.fd)
}

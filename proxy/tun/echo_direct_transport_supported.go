//go:build (linux && !android) || (darwin && !ios)

package tun

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type systemEchoPacketIO struct {
	family  echoFamily
	carrier carrierInterface
	conn    net.PacketConn
	ipv4    *ipv4.PacketConn
	ipv6    *ipv6.PacketConn
}

func openEchoPacketIO(family echoFamily, carrier carrierInterface) (echoPacketIO, error) {
	if (carrier.Name == "") != (carrier.Index == 0) {
		return nil, errors.New("invalid Outbound carrier interface for Direct Echo")
	}
	network, address := "ip6:ipv6-icmp", "::"
	if family == echoIPv4 {
		network, address = "ip4:icmp", "0.0.0.0"
	}
	var bindErr error
	listenConfig := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		if carrier.Name == "" {
			return nil
		}
		if err := raw.Control(func(fd uintptr) {
			bindErr = bindDirectEchoSocket(family, fd, carrier)
		}); err != nil {
			return err
		}
		return bindErr
	}}
	conn, err := listenConfig.ListenPacket(context.Background(), network, address)
	if err != nil {
		return nil, errors.New("failed to open bound Direct Echo socket for ", family).Base(err)
	}
	packet := &systemEchoPacketIO{family: family, carrier: carrier, conn: conn}
	if family == echoIPv4 {
		packet.ipv4 = ipv4.NewPacketConn(conn)
		if err := packet.ipv4.SetControlMessage(ipv4.FlagTTL|ipv4.FlagDst, true); err != nil {
			_ = conn.Close()
			return nil, err
		}
	} else {
		packet.ipv6 = ipv6.NewPacketConn(conn)
		if err := packet.ipv6.SetControlMessage(ipv6.FlagHopLimit|ipv6.FlagDst, true); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return packet, nil
}

func (p *systemEchoPacketIO) Write(packet []byte, destination netip.Addr) error {
	zone := ""
	if destination.Is6() && destination.IsLinkLocalUnicast() {
		zone = p.carrier.Name
	}
	address := &net.IPAddr{IP: net.IP(destination.AsSlice()), Zone: zone}
	if p.family == echoIPv4 {
		_, err := p.ipv4.WriteTo(packet, nil, address)
		return err
	}
	_, err := p.ipv6.WriteTo(packet, nil, address)
	return err
}

func (p *systemEchoPacketIO) Read(buffer []byte) (int, netip.Addr, netip.Addr, uint8, error) {
	if p.family == echoIPv4 {
		n, control, source, err := p.ipv4.ReadFrom(buffer)
		if err != nil {
			return 0, netip.Addr{}, netip.Addr{}, 0, err
		}
		ipSource, sourceOK := source.(*net.IPAddr)
		if !sourceOK {
			return 0, netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("%w: invalid IPv4 source address", errInvalidEchoReplyMetadata)
		}
		address, ok := netip.AddrFromSlice(ipSource.IP)
		if control == nil {
			return 0, netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("%w: missing IPv4 metadata", errInvalidEchoReplyMetadata)
		}
		destination, destinationOK := netip.AddrFromSlice(control.Dst)
		if !ok || !destinationOK || control.TTL <= 0 || control.TTL > 255 {
			return 0, netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("%w: invalid IPv4 TTL or destination metadata", errInvalidEchoReplyMetadata)
		}
		return n, address.Unmap(), destination.Unmap(), uint8(control.TTL), nil
	}
	n, control, source, err := p.ipv6.ReadFrom(buffer)
	if err != nil {
		return 0, netip.Addr{}, netip.Addr{}, 0, err
	}
	ipSource, sourceOK := source.(*net.IPAddr)
	if !sourceOK {
		return 0, netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("%w: invalid IPv6 source address", errInvalidEchoReplyMetadata)
	}
	address, ok := netip.AddrFromSlice(ipSource.IP)
	if control == nil {
		return 0, netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("%w: missing IPv6 metadata", errInvalidEchoReplyMetadata)
	}
	destination, destinationOK := netip.AddrFromSlice(control.Dst)
	if !ok || !destinationOK || control.HopLimit <= 0 || control.HopLimit > 255 {
		return 0, netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("%w: invalid IPv6 Hop Limit or destination metadata", errInvalidEchoReplyMetadata)
	}
	return n, address, destination, uint8(control.HopLimit), nil
}

func (p *systemEchoPacketIO) Close() error {
	return p.conn.Close()
}

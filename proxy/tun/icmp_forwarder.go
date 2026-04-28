package tun

import (
	"context"
	"net/netip"

	"github.com/xtls/xray-core/common/errors"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
)

// icmpXORIdent is XORed with the ICMP identifier to avoid conflicts
// with system ICMP traffic using the same identifiers.
const icmpXORIdent uint16 = 0xFFFF

// icmpReadTimeout is the timeout (in seconds) for reading ICMP replies.
const icmpReadTimeout int64 = 5

func init() {
	icmpForwarderSetup = func(ctx context.Context, ipStack *stack.Stack, tunName string) {
		f := &icmpForwarder{ctx: ctx, gStack: ipStack}
		ipStack.SetTransportProtocolHandler(icmp.ProtocolNumber4, f.handlePacket)
		ipStack.SetTransportProtocolHandler(icmp.ProtocolNumber6, f.handlePacket)
	}
}

// rawSocket abstracts platform-specific ICMP socket operations.
type rawSocket interface {
	send(data []byte) error
	recv(buf []byte) (int, error)
	close()
}

// icmpForwarder handles ICMP Echo packets by forwarding them through
// the physical network interface via raw sockets. Replies are injected
// back into the gVisor stack so the application receives them.
type icmpForwarder struct {
	ctx    context.Context
	gStack *stack.Stack
}

func (f *icmpForwarder) handlePacket(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	if pkt.NetworkProtocolNumber == header.IPv4ProtocolNumber {
		return f.handleIPv4(pkt)
	}
	return f.handleIPv6(pkt)
}

func (f *icmpForwarder) handleIPv4(pkt *stack.PacketBuffer) bool {
	transportSlice := pkt.TransportHeader().Slice()
	if len(transportSlice) < header.ICMPv4MinimumSize {
		return false
	}
	icmpHdr := header.ICMPv4(transportSlice)

	// Only handle Echo Request (type 8, code 0)
	if icmpHdr.Type() != header.ICMPv4Echo || icmpHdr.Code() != 0 {
		return false
	}

	srcAddr := netip.AddrFrom4(header.IPv4(pkt.NetworkHeader().Slice()).SourceAddress().As4())
	dstAddr := netip.AddrFrom4(header.IPv4(pkt.NetworkHeader().Slice()).DestinationAddress().As4())

	// Build complete ICMP payload (header + data)
	icmpPayload := f.buildPayload(pkt)

	// XOR identifier to avoid conflicts with system ICMP
	hdr := header.ICMPv4(icmpPayload)
	hdr.SetIdent(hdr.Ident() ^ icmpXORIdent)
	hdr.SetChecksum(header.ICMPv4Checksum(hdr, 0))

	go f.sendAndReceive(srcAddr, dstAddr, icmpPayload, header.IPv4ProtocolNumber)
	return true
}

func (f *icmpForwarder) handleIPv6(pkt *stack.PacketBuffer) bool {
	transportSlice := pkt.TransportHeader().Slice()
	if len(transportSlice) < header.ICMPv6MinimumSize {
		return false
	}
	icmpHdr := header.ICMPv6(transportSlice)

	// Only handle Echo Request (type 128, code 0)
	if icmpHdr.Type() != header.ICMPv6EchoRequest || icmpHdr.Code() != 0 {
		return false
	}

	srcAddr := netip.AddrFrom16(header.IPv6(pkt.NetworkHeader().Slice()).SourceAddress().As16())
	dstAddr := netip.AddrFrom16(header.IPv6(pkt.NetworkHeader().Slice()).DestinationAddress().As16())

	icmpPayload := f.buildPayload(pkt)

	// XOR identifier
	hdr := header.ICMPv6(icmpPayload)
	hdr.SetIdent(hdr.Ident() ^ icmpXORIdent)
	hdr.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
		Header: hdr,
		Src:    tcpip.AddrFrom16(srcAddr.As16()),
		Dst:    tcpip.AddrFrom16(dstAddr.As16()),
	}))

	go f.sendAndReceive(srcAddr, dstAddr, icmpPayload, header.IPv6ProtocolNumber)
	return true
}

// buildPayload extracts the ICMP header + data from a gVisor packet buffer.
func (f *icmpForwarder) buildPayload(pkt *stack.PacketBuffer) []byte {
	transportData := pkt.TransportHeader().Slice()
	payloadData := pkt.Data().AsRange().ToSlice()
	icmpPayload := make([]byte, len(transportData)+len(payloadData))
	copy(icmpPayload, transportData)
	copy(icmpPayload[len(transportData):], payloadData)
	return icmpPayload
}

// sendAndReceive sends an ICMP packet through a raw socket bound to the
// physical interface, reads the reply, and injects it back into gVisor.
func (f *icmpForwarder) sendAndReceive(src, dst netip.Addr, icmpPayload []byte, protocol tcpip.NetworkProtocolNumber) {
	sock, err := createRawSocket(dst, f.ctx)
	if err != nil {
		errors.LogError(f.ctx, "icmp: failed to create socket: ", err)
		return
	}
	defer sock.close()

	// Send the ICMP payload
	err = sock.send(icmpPayload)
	if err != nil {
		errors.LogError(f.ctx, "icmp: failed to send: ", err)
		return
	}

	// Read reply
	buf := make([]byte, 1500)
	for {
		n, err := sock.recv(buf)
		if err != nil {
			// Timeout or other error - silently drop
			return
		}

		// Process the first valid reply and return
		if f.processReply(src, buf[:n], protocol) {
			return
		}
	}
}

// processReply reads an IP packet from the raw socket, checks if it's
// an ICMP Echo Reply, XORs the identifier back, modifies the destination
// to the original source (TUN IP), fixes checksums, and injects into gVisor.
// Returns true if the reply was successfully processed and injected.
func (f *icmpForwarder) processReply(originalSrc netip.Addr, data []byte, protocol tcpip.NetworkProtocolNumber) bool {
	if protocol == header.IPv4ProtocolNumber {
		return f.processReplyIPv4(originalSrc, data)
	}
	return f.processReplyIPv6(originalSrc, data)
}

func (f *icmpForwarder) processReplyIPv4(originalSrc netip.Addr, data []byte) bool {
	if len(data) < header.IPv4MinimumSize+header.ICMPv4MinimumSize {
		return false
	}
	ipHdr := header.IPv4(data)
	icmpHdr := header.ICMPv4(ipHdr.Payload())

	// Only process Echo Reply (type 0)
	if icmpHdr.Type() != header.ICMPv4EchoReply {
		return false
	}

	// XOR identifier back to the original value
	icmpHdr.SetIdent(icmpHdr.Ident() ^ icmpXORIdent)

	// Modify destination to the original source (TUN's IP / application's IP)
	ipHdr.SetDestinationAddress(tcpip.AddrFrom4(originalSrc.As4()))

	// Recalculate ICMP checksum
	icmpHdr.SetChecksum(header.ICMPv4Checksum(icmpHdr, 0))

	// Recalculate IP checksum
	ipHdr.SetChecksum(0)
	ipHdr.SetChecksum(^ipHdr.CalculateChecksum())

	// Inject into gVisor stack → routes back through TUN to the application
	view := buffer.MakeWithData(data)
	err := f.gStack.WriteRawPacket(defaultNIC, header.IPv4ProtocolNumber, view)
	if err != nil {
		errors.LogInfo(f.ctx, "icmp: failed to inject reply: ", err)
		return false
	}
	return true
}

func (f *icmpForwarder) processReplyIPv6(originalSrc netip.Addr, data []byte) bool {
	if len(data) < header.IPv6MinimumSize+header.ICMPv6MinimumSize {
		return false
	}
	ipHdr := header.IPv6(data)
	icmpHdr := header.ICMPv6(ipHdr.Payload())

	// Only process Echo Reply (type 129)
	if icmpHdr.Type() != header.ICMPv6EchoReply {
		return false
	}

	// XOR identifier back
	icmpHdr.SetIdent(icmpHdr.Ident() ^ icmpXORIdent)

	// Modify destination to original source
	ipHdr.SetDestinationAddress(tcpip.AddrFrom16(originalSrc.As16()))

	// Recalculate ICMPv6 checksum (includes pseudo-header)
	icmpHdr.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
		Header: icmpHdr,
		Src:    ipHdr.SourceAddress(),
		Dst:    ipHdr.DestinationAddress(),
	}))

	// Inject into gVisor
	view := buffer.MakeWithData(data)
	err := f.gStack.WriteRawPacket(defaultNIC, header.IPv6ProtocolNumber, view)
	if err != nil {
		errors.LogInfo(f.ctx, "icmp: failed to inject reply: ", err)
		return false
	}
	return true
}

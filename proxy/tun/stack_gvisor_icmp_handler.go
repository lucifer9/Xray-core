package tun

import (
	stderrors "errors"
	"net/netip"

	"github.com/xtls/xray-core/common/errors"
	tunicmp "github.com/xtls/xray-core/proxy/tun/icmp"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func (t *stackGVisor) handleICMPv4Packet(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	return t.handleICMPEchoPacket(header.IPv4ProtocolNumber, id, pkt)
}

func (t *stackGVisor) handleICMPv6Packet(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	return t.handleICMPEchoPacket(header.IPv6ProtocolNumber, id, pkt)
}

func (t *stackGVisor) handleICMPEchoPacket(netProto tcpip.NetworkProtocolNumber, id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	srcIP := id.RemoteAddress
	dstIP := id.LocalAddress
	if srcIP.Len() == 0 || dstIP.Len() == 0 {
		return true
	}

	message := transportPacketBytes(pkt)
	ident, sequence, ok := tunicmp.ParseEchoRequest(netProto, message)
	if !ok {
		return true
	}

	family := echoIPv6
	if netProto == header.IPv4ProtocolNumber {
		family = echoIPv4
	}
	source, sourceOK := netip.AddrFromSlice(srcIP.AsSlice())
	destination, destinationOK := netip.AddrFromSlice(dstIP.AsSlice())
	if !sourceOK || !destinationOK {
		return true
	}
	request := echoRequest{
		Family:      family,
		Source:      source.Unmap(),
		Destination: destination.Unmap(),
		Identifier:  ident,
		Sequence:    sequence,
		Payload:     append([]byte(nil), message[header.ICMPv4MinimumSize:]...),
	}
	if err := t.echoProber.Probe(request, func(result echoResult) {
		if result.Reply == nil || !t.beginEchoCompletion() {
			return
		}
		defer t.completionWait.Done()
		reply, err := tunicmp.BuildEchoReply(netProto, result.Reply.Identifier, result.Reply.Sequence, result.Reply.Payload, dstIP, srcIP)
		if err != nil {
			errors.LogInfoInner(t.ctx, err, "[tun] failed to build ICMP Echo reply")
			return
		}
		errors.LogDebug(t.ctx, "[tun][icmp] ", tunicmp.ProtocolLabel(netProto), " echo reply ", dstIP, " -> ", srcIP, " id=", ident, " seq=", sequence)
		if err := t.emitRawICMPPacket(netProto, reply, dstIP, srcIP, result.Reply.TTL); err != nil {
			errors.LogInfoInner(t.ctx, err, "[tun] failed to write ICMP Echo reply")
		}
	}); err != nil {
		if stderrors.Is(err, errEchoCarrierUnknown) {
			t.emitEchoUnreachable(netProto, message, srcIP, dstIP)
		} else {
			errors.LogInfoInner(t.ctx, err, "[tun] failed to submit ICMP Echo probe")
		}
	}

	return true
}

// emitEchoUnreachable reports an ICMP network unreachable error back to the
// TUN client, mirroring what a real router returns when the probe's address
// family has no Outbound carrier.
func (t *stackGVisor) emitEchoUnreachable(netProto tcpip.NetworkProtocolNumber, originalMessage []byte, srcIP, dstIP tcpip.Address) {
	if !t.beginEchoCompletion() {
		return
	}
	defer t.completionWait.Done()

	from := t.gateway6
	if netProto == header.IPv4ProtocolNumber {
		from = t.gateway4
	}
	reporter := dstIP
	if from.IsValid() {
		reporter = tcpip.AddrFromSlice(from.AsSlice())
	}
	message, err := tunicmp.BuildDestinationUnreachable(netProto, originalMessage, srcIP, dstIP, reporter, srcIP)
	if err != nil {
		errors.LogInfoInner(t.ctx, err, "[tun] failed to build ICMP Destination Unreachable")
		return
	}
	errors.LogDebug(t.ctx, "[tun][icmp] ", tunicmp.ProtocolLabel(netProto), " network unreachable ", reporter, " -> ", srcIP)
	if err := t.emitRawICMPPacket(netProto, message, reporter, srcIP, 64); err != nil {
		errors.LogInfoInner(t.ctx, err, "[tun] failed to write ICMP Destination Unreachable")
	}
}

func (t *stackGVisor) writeRawICMPPacket(netProto tcpip.NetworkProtocolNumber, message []byte, srcIP, dstIP tcpip.Address, ttl uint8) error {
	ipHeaderSize := header.IPv6MinimumSize
	ipProtocol := header.IPv6ProtocolNumber
	transportProtocol := header.ICMPv6ProtocolNumber
	if netProto == header.IPv4ProtocolNumber {
		ipHeaderSize = header.IPv4MinimumSize
		ipProtocol = header.IPv4ProtocolNumber
		transportProtocol = header.ICMPv4ProtocolNumber
	}

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: ipHeaderSize,
		Payload:            buffer.MakeWithData(message),
	})
	defer pkt.DecRef()

	if netProto == header.IPv4ProtocolNumber {
		ipHdr := header.IPv4(pkt.NetworkHeader().Push(header.IPv4MinimumSize))
		ipHdr.Encode(&header.IPv4Fields{
			TotalLength: uint16(header.IPv4MinimumSize + len(message)),
			TTL:         ttl,
			Protocol:    uint8(transportProtocol),
			SrcAddr:     srcIP,
			DstAddr:     dstIP,
		})
		ipHdr.SetChecksum(^ipHdr.CalculateChecksum())
	} else {
		ipHdr := header.IPv6(pkt.NetworkHeader().Push(header.IPv6MinimumSize))
		ipHdr.Encode(&header.IPv6Fields{
			PayloadLength:     uint16(len(message)),
			TransportProtocol: transportProtocol,
			HopLimit:          ttl,
			SrcAddr:           srcIP,
			DstAddr:           dstIP,
		})
	}

	if err := t.stack.WriteRawPacket(defaultNIC, ipProtocol, buffer.MakeWithView(pkt.ToView())); err != nil {
		return errors.New("failed to write raw icmp packet back to stack", err)
	}

	return nil
}

func transportPacketBytes(pkt *stack.PacketBuffer) []byte {
	headerBytes := pkt.TransportHeader().Slice()
	payloadBytes := pkt.Data().AsRange().ToSlice()
	message := make([]byte, len(headerBytes)+len(payloadBytes))
	copy(message, headerBytes)
	copy(message[len(headerBytes):], payloadBytes)
	return message
}

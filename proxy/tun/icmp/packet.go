package icmp

import (
	"github.com/xtls/xray-core/common/errors"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func ProtocolLabel(netProto tcpip.NetworkProtocolNumber) string {
	switch netProto {
	case header.IPv4ProtocolNumber:
		return "ipv4"
	case header.IPv6ProtocolNumber:
		return "ipv6"
	default:
		return "unknown"
	}
}

func ParseEchoRequest(netProto tcpip.NetworkProtocolNumber, message []byte) (uint16, uint16, bool) {
	switch netProto {
	case header.IPv4ProtocolNumber:
		if len(message) < header.ICMPv4MinimumSize {
			return 0, 0, false
		}
		icmpHdr := header.ICMPv4(message)
		if icmpHdr.Type() != header.ICMPv4Echo || icmpHdr.Code() != header.ICMPv4UnusedCode {
			return 0, 0, false
		}
		return icmpHdr.Ident(), icmpHdr.Sequence(), true
	case header.IPv6ProtocolNumber:
		if len(message) < header.ICMPv6MinimumSize {
			return 0, 0, false
		}
		icmpHdr := header.ICMPv6(message)
		if icmpHdr.Type() != header.ICMPv6EchoRequest || icmpHdr.Code() != header.ICMPv6UnusedCode {
			return 0, 0, false
		}
		return icmpHdr.Ident(), icmpHdr.Sequence(), true
	default:
		return 0, 0, false
	}
}

func RewriteChecksum(netProto tcpip.NetworkProtocolNumber, message []byte, srcIP, dstIP tcpip.Address) error {
	switch netProto {
	case header.IPv4ProtocolNumber:
		if len(message) < header.ICMPv4MinimumSize {
			return errors.New("invalid icmpv4 packet")
		}
		icmpHdr := header.ICMPv4(message)
		icmpHdr.SetChecksum(0)
		icmpHdr.SetChecksum(header.ICMPv4Checksum(icmpHdr[:header.ICMPv4MinimumSize], checksum.Checksum(icmpHdr.Payload(), 0)))
		return nil
	case header.IPv6ProtocolNumber:
		if len(message) < header.ICMPv6MinimumSize {
			return errors.New("invalid icmpv6 packet")
		}
		icmpHdr := header.ICMPv6(message)
		icmpHdr.SetChecksum(0)
		icmpHdr.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
			Header:      icmpHdr[:header.ICMPv6MinimumSize],
			Src:         srcIP,
			Dst:         dstIP,
			PayloadCsum: checksum.Checksum(icmpHdr.Payload(), 0),
			PayloadLen:  len(icmpHdr.Payload()),
		}))
		return nil
	default:
		return errors.New("unsupported icmp network protocol")
	}
}

func BuildLocalEchoReply(netProto tcpip.NetworkProtocolNumber, request []byte, srcIP, dstIP tcpip.Address) ([]byte, error) {
	identifier, sequence, ok := ParseEchoRequest(netProto, request)
	if !ok {
		return nil, errors.New("not an ICMP Echo request")
	}
	return BuildEchoReply(netProto, identifier, sequence, request[header.ICMPv4MinimumSize:], srcIP, dstIP)
}

func BuildEchoReply(netProto tcpip.NetworkProtocolNumber, identifier, sequence uint16, payload []byte, srcIP, dstIP tcpip.Address) ([]byte, error) {
	reply := make([]byte, header.ICMPv4MinimumSize+len(payload))
	copy(reply[header.ICMPv4MinimumSize:], payload)

	switch netProto {
	case header.IPv4ProtocolNumber:
		icmpHdr := header.ICMPv4(reply)
		icmpHdr.SetType(header.ICMPv4EchoReply)
		icmpHdr.SetCode(header.ICMPv4UnusedCode)
		icmpHdr.SetIdent(identifier)
		icmpHdr.SetSequence(sequence)
	case header.IPv6ProtocolNumber:
		icmpHdr := header.ICMPv6(reply)
		icmpHdr.SetType(header.ICMPv6EchoReply)
		icmpHdr.SetCode(header.ICMPv6UnusedCode)
		icmpHdr.SetIdent(identifier)
		icmpHdr.SetSequence(sequence)
	default:
		return nil, errors.New("unsupported icmp network protocol")
	}

	if err := RewriteChecksum(netProto, reply, srcIP, dstIP); err != nil {
		return nil, err
	}

	return reply, nil
}

// Maximum original-packet bytes quoted in a Destination Unreachable error
// message: enough for the sender to match the error, bounded by the minimum
// MTU each stack must accept.
const (
	maxIPv4UnreachableQuote = 576 - header.IPv4MinimumSize - header.ICMPv4MinimumSize
	maxIPv6UnreachableQuote = 1280 - header.IPv6MinimumSize - header.ICMPv6MinimumSize
)

// BuildDestinationUnreachable builds an ICMP Destination Unreachable
// (network unreachable) error message reporting that the packet from origSrc
// to origDst, whose transport message is originalMessage, could not be
// forwarded. The error is addressed from srcIP (the reporting router) to
// dstIP (the original sender) and quotes the original packet like a router
// would.
func BuildDestinationUnreachable(netProto tcpip.NetworkProtocolNumber, originalMessage []byte, origSrc, origDst, srcIP, dstIP tcpip.Address) ([]byte, error) {
	var quote []byte
	switch netProto {
	case header.IPv4ProtocolNumber:
		quoted := min(len(originalMessage), maxIPv4UnreachableQuote-header.IPv4MinimumSize)
		quote = make([]byte, header.IPv4MinimumSize+quoted)
		ipHdr := header.IPv4(quote)
		ipHdr.Encode(&header.IPv4Fields{
			TotalLength: uint16(header.IPv4MinimumSize + len(originalMessage)),
			TTL:         64,
			Protocol:    uint8(header.ICMPv4ProtocolNumber),
			SrcAddr:     origSrc,
			DstAddr:     origDst,
		})
		ipHdr.SetChecksum(^ipHdr.CalculateChecksum())
		copy(quote[header.IPv4MinimumSize:], originalMessage[:quoted])
	case header.IPv6ProtocolNumber:
		quoted := min(len(originalMessage), maxIPv6UnreachableQuote-header.IPv6MinimumSize)
		quote = make([]byte, header.IPv6MinimumSize+quoted)
		ipHdr := header.IPv6(quote)
		ipHdr.Encode(&header.IPv6Fields{
			PayloadLength:     uint16(len(originalMessage)),
			TransportProtocol: header.ICMPv6ProtocolNumber,
			HopLimit:          64,
			SrcAddr:           origSrc,
			DstAddr:           origDst,
		})
		copy(quote[header.IPv6MinimumSize:], originalMessage[:quoted])
	default:
		return nil, errors.New("unsupported icmp network protocol")
	}

	message := make([]byte, header.ICMPv4MinimumSize+len(quote))
	copy(message[header.ICMPv4MinimumSize:], quote)
	switch netProto {
	case header.IPv4ProtocolNumber:
		icmpHdr := header.ICMPv4(message)
		icmpHdr.SetType(header.ICMPv4DstUnreachable)
		icmpHdr.SetCode(header.ICMPv4NetUnreachable)
	case header.IPv6ProtocolNumber:
		icmpHdr := header.ICMPv6(message)
		icmpHdr.SetType(header.ICMPv6DstUnreachable)
		icmpHdr.SetCode(header.ICMPv6NetworkUnreachable)
	}

	if err := RewriteChecksum(netProto, message, srcIP, dstIP); err != nil {
		return nil, err
	}

	return message, nil
}

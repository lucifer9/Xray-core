package tun

import (
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/header/parse"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func newIPv6EchoProtocolFactory(intercept func(*stack.PacketBuffer) bool) stack.NetworkProtocolFactory {
	return func(ipStack *stack.Stack) stack.NetworkProtocol {
		return &ipv6EchoProtocol{
			NetworkProtocol: ipv6.NewProtocol(ipStack),
			intercept:       intercept,
		}
	}
}

type ipv6EchoProtocol struct {
	stack.NetworkProtocol
	intercept func(*stack.PacketBuffer) bool
}

func (p *ipv6EchoProtocol) NewEndpoint(nic stack.NetworkInterface, dispatcher stack.TransportDispatcher) stack.NetworkEndpoint {
	endpoint := p.NetworkProtocol.NewEndpoint(nic, dispatcher)
	return &ipv6EchoEndpoint{
		MulticastForwardingNetworkEndpoint: endpoint.(stack.MulticastForwardingNetworkEndpoint),
		AddressableEndpoint:                endpoint.(stack.AddressableEndpoint),
		linkResolvable:                     endpoint.(stack.LinkResolvableNetworkEndpoint),
		intercept:                          p.intercept,
	}
}

type ipv6EchoEndpoint struct {
	stack.MulticastForwardingNetworkEndpoint
	stack.AddressableEndpoint
	linkResolvable stack.LinkResolvableNetworkEndpoint
	intercept      func(*stack.PacketBuffer) bool
}

func (e *ipv6EchoEndpoint) HandlePacket(packet *stack.PacketBuffer) {
	// gVisor handles ICMPv6 Echo Requests inside its IPv6 network endpoint and
	// never dispatches them to the registered ICMP transport handler. Intercept
	// Echo here so both IPv4 and IPv6 are governed by the configured Echo prober.
	if e.intercept != nil && e.intercept(packet) {
		return
	}
	e.MulticastForwardingNetworkEndpoint.HandlePacket(packet)
}

func (e *ipv6EchoEndpoint) HandleLinkResolutionFailure(packet *stack.PacketBuffer) {
	e.linkResolvable.HandleLinkResolutionFailure(packet)
}

func (t *stackGVisor) interceptIPv6EchoRequest(packet *stack.PacketBuffer) bool {
	fixedHeader, ok := packet.Data().PullUp(header.IPv6MinimumSize)
	if !ok {
		return false
	}
	ipHeader := header.IPv6(fixedHeader)
	if !ipHeader.IsValid(packet.Data().Size()) || !ipv6PayloadMayContainICMP(ipHeader.NextHeader()) {
		return false
	}

	clone := packet.Clone()
	defer clone.DecRef()
	transportProtocol, _, fragmentOffset, fragmentMore, ok := parse.IPv6(clone)
	if !ok || fragmentOffset != 0 || fragmentMore || transportProtocol != header.ICMPv6ProtocolNumber || !parse.ICMPv6(clone) {
		return false
	}

	icmpHeader := header.ICMPv6(clone.TransportHeader().Slice())
	if icmpHeader.Type() != header.ICMPv6EchoRequest || icmpHeader.Code() != header.ICMPv6UnusedCode {
		return false
	}
	ipHeader = header.IPv6(clone.NetworkHeader().Slice())
	payload := clone.Data()
	if icmpHeader.Checksum() != header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
		Header:      icmpHeader,
		Src:         ipHeader.SourceAddress(),
		Dst:         ipHeader.DestinationAddress(),
		PayloadCsum: payload.Checksum(),
		PayloadLen:  payload.Size(),
	}) {
		return true
	}

	return t.handleICMPv6Packet(stack.TransportEndpointID{
		RemoteAddress: ipHeader.SourceAddress(),
		LocalAddress:  ipHeader.DestinationAddress(),
	}, clone)
}

func ipv6PayloadMayContainICMP(nextHeader uint8) bool {
	if tcpip.TransportProtocolNumber(nextHeader) == header.ICMPv6ProtocolNumber {
		return true
	}
	switch header.IPv6ExtensionHeaderIdentifier(nextHeader) {
	case header.IPv6HopByHopOptionsExtHdrIdentifier,
		header.IPv6RoutingExtHdrIdentifier,
		header.IPv6FragmentExtHdrIdentifier,
		header.IPv6DestinationOptionsExtHdrIdentifier,
		header.IPv6ExperimentExtHdrIdentifier:
		return true
	default:
		return false
	}
}

var _ stack.NetworkProtocol = (*ipv6EchoProtocol)(nil)
var _ stack.MulticastForwardingNetworkEndpoint = (*ipv6EchoEndpoint)(nil)
var _ stack.AddressableEndpoint = (*ipv6EchoEndpoint)(nil)
var _ stack.LinkResolvableNetworkEndpoint = (*ipv6EchoEndpoint)(nil)

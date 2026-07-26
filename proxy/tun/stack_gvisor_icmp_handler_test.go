package tun

import (
	"bytes"
	"net/netip"
	"sync"
	"testing"

	tunicmp "github.com/xtls/xray-core/proxy/tun/icmp"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestICMPEchoHandlerWaitsForProberCompletion(t *testing.T) {
	prober := &delayedEchoProber{}
	written := make(chan writtenEchoPacket, 1)
	gvisor := &stackGVisor{echoProber: prober, rawICMPWriter: func(protocol tcpip.NetworkProtocolNumber, message []byte, source, destination tcpip.Address, ttl uint8) error {
		written <- writtenEchoPacket{protocol: protocol, message: append([]byte(nil), message...), source: source, destination: destination, ttl: ttl}
		return nil
	}}
	request := testICMPv4EchoRequest(t)
	packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(request)})
	defer packet.DecRef()
	id := stack.TransportEndpointID{
		RemoteAddress: tcpip.AddrFrom4([4]byte{10, 0, 0, 2}),
		LocalAddress:  tcpip.AddrFrom4([4]byte{198, 51, 100, 1}),
	}
	gvisor.handleICMPv4Packet(id, packet)
	select {
	case got := <-written:
		t.Fatalf("handler wrote before probe completion: %+v", got)
	default:
	}

	prober.complete(echoResult{Reply: &echoReply{Identifier: 0x1234, Sequence: 0x5678, Payload: []byte("payload"), TTL: 39}})
	got := <-written
	if got.protocol != header.IPv4ProtocolNumber || got.ttl != 39 || got.source != id.LocalAddress || got.destination != id.RemoteAddress {
		t.Fatalf("written packet metadata = %+v", got)
	}
	ident, sequence, ok := parseEchoReply(got.message)
	if !ok || ident != 0x1234 || sequence != 0x5678 {
		t.Fatalf("written Echo reply id/seq = %x/%x, ok=%v", ident, sequence, ok)
	}
}

func TestICMPEchoHandlerCompletesIPv6WithRemoteHopLimit(t *testing.T) {
	prober := &delayedEchoProber{}
	written := make(chan writtenEchoPacket, 1)
	gvisor := &stackGVisor{echoProber: prober, rawICMPWriter: func(protocol tcpip.NetworkProtocolNumber, message []byte, source, destination tcpip.Address, ttl uint8) error {
		written <- writtenEchoPacket{protocol: protocol, message: append([]byte(nil), message...), source: source, destination: destination, ttl: ttl}
		return nil
	}}
	request := testICMPv6EchoRequest(t)
	packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(request)})
	defer packet.DecRef()
	id := stack.TransportEndpointID{
		RemoteAddress: tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 2}),
		LocalAddress:  tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 1}),
	}
	gvisor.handleICMPv6Packet(id, packet)
	prober.complete(echoResult{Reply: &echoReply{Identifier: 0x1234, Sequence: 0x5678, Payload: []byte("payload"), TTL: 31}})

	got := <-written
	if got.protocol != header.IPv6ProtocolNumber || got.ttl != 31 || got.source != id.LocalAddress || got.destination != id.RemoteAddress {
		t.Fatalf("written packet metadata = %+v", got)
	}
	icmpHeader := header.ICMPv6(got.message)
	if icmpHeader.Type() != header.ICMPv6EchoReply || icmpHeader.Ident() != 0x1234 || icmpHeader.Sequence() != 0x5678 || !bytes.Equal(icmpHeader.Payload(), []byte("payload")) {
		t.Fatalf("written IPv6 Echo reply = %x", got.message)
	}
	wantChecksum := header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
		Header:      icmpHeader[:header.ICMPv6MinimumSize],
		Src:         got.source,
		Dst:         got.destination,
		PayloadCsum: checksum.Checksum(icmpHeader.Payload(), 0),
		PayloadLen:  len(icmpHeader.Payload()),
	})
	if icmpHeader.Checksum() != wantChecksum {
		t.Fatalf("IPv6 checksum = %x, want %x", icmpHeader.Checksum(), wantChecksum)
	}
}

func TestICMPEchoHandlerInterceptsIPv6BeforeNetworkLocalEcho(t *testing.T) {
	prober := &delayedEchoProber{}
	linkEndpoint := channel.New(2, 1500, "")
	gvisor := &stackGVisor{echoProber: prober, endpoint: linkEndpoint}
	ipStack, err := createStack(linkEndpoint, gvisor.interceptIPv6EchoRequest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		linkEndpoint.Close()
		ipStack.Close()
	})
	gvisor.stack = ipStack
	ipStack.SetTransportProtocolHandler(header.ICMPv6ProtocolNumber, gvisor.handleICMPv6Packet)

	packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(testIPv6EchoPacket(t))})
	linkEndpoint.InjectInbound(header.IPv6ProtocolNumber, packet)
	packet.DecRef()

	probes, request := prober.snapshot()
	localReply := linkEndpoint.Read()
	if localReply != nil {
		localReply.DecRef()
	}
	if probes != 1 || request.Family != echoIPv6 || request.Destination != netip.MustParseAddr("2001:db8::1") || localReply != nil {
		t.Fatalf("IPv6 network path probes=%d request=%+v immediate reply=%v", probes, request, localReply != nil)
	}

	prober.complete(echoResult{Reply: &echoReply{Identifier: 0x1234, Sequence: 0x5678, Payload: []byte("payload"), TTL: 31}})
	remoteReply := linkEndpoint.Read()
	if remoteReply == nil {
		t.Fatal("IPv6 network path did not emit reply after probe completion")
	}
	defer remoteReply.DecRef()
	view := remoteReply.ToView()
	defer view.Release()
	replyPacket := view.AsSlice()
	if len(replyPacket) < header.IPv6MinimumSize+header.ICMPv6MinimumSize {
		t.Fatalf("IPv6 network reply length = %d", len(replyPacket))
	}
	ipHeader := header.IPv6(replyPacket)
	icmpHeader := header.ICMPv6(replyPacket[header.IPv6MinimumSize:])
	if ipHeader.HopLimit() != 31 || icmpHeader.Type() != header.ICMPv6EchoReply || icmpHeader.Ident() != 0x1234 || icmpHeader.Sequence() != 0x5678 {
		t.Fatalf("IPv6 network reply hop limit=%d packet=%x", ipHeader.HopLimit(), replyPacket)
	}
}

func TestIPv6EchoEndpointSupportsSpoofedRoute(t *testing.T) {
	linkEndpoint := channel.New(1, 1500, "")
	ipStack, err := createStack(linkEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		linkEndpoint.Close()
		ipStack.Close()
	})

	local := tcpip.AddrFrom16([16]byte{0: 0x24, 1: 0x04, 2: 0x68, 3: 0x00, 15: 1})
	remote := tcpip.AddrFrom16([16]byte{0: 0x24, 1: 0x0a, 2: 0x42, 3: 0xb8, 15: 2})
	route, routeErr := ipStack.FindRoute(defaultNIC, local, remote, header.IPv6ProtocolNumber, false)
	if routeErr != nil {
		t.Fatal(routeErr.String())
	}
	route.Release()
}

func TestICMPEchoHandlerUsesImmediateLocalEchoByDefault(t *testing.T) {
	written := make(chan writtenEchoPacket, 1)
	gvisor := &stackGVisor{echoProber: newLocalEchoProber(), rawICMPWriter: func(protocol tcpip.NetworkProtocolNumber, message []byte, source, destination tcpip.Address, ttl uint8) error {
		written <- writtenEchoPacket{protocol: protocol, message: append([]byte(nil), message...), source: source, destination: destination, ttl: ttl}
		return nil
	}}
	request := testICMPv4EchoRequest(t)
	packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(request)})
	defer packet.DecRef()
	id := stack.TransportEndpointID{
		RemoteAddress: tcpip.AddrFrom4([4]byte{10, 0, 0, 2}),
		LocalAddress:  tcpip.AddrFrom4([4]byte{198, 51, 100, 1}),
	}
	gvisor.handleICMPv4Packet(id, packet)

	select {
	case got := <-written:
		icmpHeader := header.ICMPv4(got.message)
		if got.ttl != 64 || icmpHeader.Ident() != 0x1234 || icmpHeader.Sequence() != 0x5678 || !bytes.Equal(icmpHeader.Payload(), []byte("payload")) {
			t.Fatalf("Local Echo reply = %+v, packet=%x", got, got.message)
		}
	default:
		t.Fatal("Local Echo did not reply immediately")
	}
}

func TestICMPEchoHandlerDoesNotFallbackAfterFailureOrShutdown(t *testing.T) {
	for _, test := range []struct {
		name   string
		result echoResult
		close  bool
	}{
		{name: "failure", result: echoResult{Err: errEchoTimeout}},
		{name: "shutdown", result: echoResult{Reply: &echoReply{TTL: 64}}, close: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			prober := &delayedEchoProber{}
			writes := 0
			gvisor := &stackGVisor{echoProber: prober, rawICMPWriter: func(tcpip.NetworkProtocolNumber, []byte, tcpip.Address, tcpip.Address, uint8) error {
				writes++
				return nil
			}}
			packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(testICMPv4EchoRequest(t))})
			defer packet.DecRef()
			gvisor.handleICMPv4Packet(stack.TransportEndpointID{RemoteAddress: tcpip.AddrFrom4([4]byte{10, 0, 0, 2}), LocalAddress: tcpip.AddrFrom4([4]byte{198, 51, 100, 1})}, packet)
			if test.close {
				_ = gvisor.Close()
			}
			prober.complete(test.result)
			if writes != 0 {
				t.Fatalf("writes = %d, want 0", writes)
			}
		})
	}
}

func TestICMPEchoHandlerReportsUnreachableWithoutCarrier(t *testing.T) {
	for _, test := range []struct {
		name    string
		v6      bool
		gateway netip.Addr
	}{
		{name: "ipv4 with gateway", gateway: netip.MustParseAddr("172.18.0.1")},
		{name: "ipv4 without gateway"},
		{name: "ipv6 with gateway", v6: true, gateway: netip.MustParseAddr("fdfe:dcba:8964::1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			written := make(chan writtenEchoPacket, 1)
			gvisor := &stackGVisor{
				echoProber: &failingEchoProber{err: errEchoCarrierUnknown},
				rawICMPWriter: func(protocol tcpip.NetworkProtocolNumber, message []byte, source, destination tcpip.Address, ttl uint8) error {
					written <- writtenEchoPacket{protocol: protocol, message: append([]byte(nil), message...), source: source, destination: destination, ttl: ttl}
					return nil
				},
			}
			client := tcpip.AddrFrom4([4]byte{10, 0, 0, 2})
			target := tcpip.AddrFrom4([4]byte{198, 51, 100, 1})
			request := testICMPv4EchoRequest(t)
			if test.v6 {
				gvisor.gateway6 = test.gateway
				client = tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 2})
				target = tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 1})
				request = testICMPv6EchoRequest(t)
			} else {
				gvisor.gateway4 = test.gateway
			}
			packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(request)})
			defer packet.DecRef()
			id := stack.TransportEndpointID{RemoteAddress: client, LocalAddress: target}
			if test.v6 {
				gvisor.handleICMPv6Packet(id, packet)
			} else {
				gvisor.handleICMPv4Packet(id, packet)
			}

			got := <-written
			wantSource := target
			if test.gateway.IsValid() {
				wantSource = tcpip.AddrFromSlice(test.gateway.AsSlice())
			}
			if got.source != wantSource || got.destination != client {
				t.Fatalf("unreachable metadata = %+v, want from %v to %v", got, wantSource, client)
			}
			quote := got.message[header.ICMPv4MinimumSize:]
			if test.v6 {
				icmpHeader := header.ICMPv6(got.message)
				if icmpHeader.Type() != header.ICMPv6DstUnreachable || icmpHeader.Code() != header.ICMPv6NetworkUnreachable {
					t.Fatalf("unreachable type/code = %v/%v", icmpHeader.Type(), icmpHeader.Code())
				}
				ipHeader := header.IPv6(quote)
				if ipHeader.SourceAddress() != client || ipHeader.DestinationAddress() != target {
					t.Fatalf("quoted IPv6 header = %v -> %v", ipHeader.SourceAddress(), ipHeader.DestinationAddress())
				}
				inner := header.ICMPv6(quote[header.IPv6MinimumSize:])
				if inner.Type() != header.ICMPv6EchoRequest || inner.Ident() != 0x1234 || inner.Sequence() != 0x5678 {
					t.Fatalf("quoted echo = %x", quote[header.IPv6MinimumSize:])
				}
			} else {
				icmpHeader := header.ICMPv4(got.message)
				if icmpHeader.Type() != header.ICMPv4DstUnreachable || icmpHeader.Code() != header.ICMPv4NetUnreachable {
					t.Fatalf("unreachable type/code = %v/%v", icmpHeader.Type(), icmpHeader.Code())
				}
				ipHeader := header.IPv4(quote)
				if ipHeader.SourceAddress() != client || ipHeader.DestinationAddress() != target || !ipHeader.IsValid(len(quote)) {
					t.Fatalf("quoted IPv4 header = %v -> %v", ipHeader.SourceAddress(), ipHeader.DestinationAddress())
				}
				inner := header.ICMPv4(quote[header.IPv4MinimumSize:])
				if inner.Type() != header.ICMPv4Echo || inner.Ident() != 0x1234 || inner.Sequence() != 0x5678 {
					t.Fatalf("quoted echo = %x", quote[header.IPv4MinimumSize:])
				}
			}
		})
	}
}

func TestICMPEchoHandlerDropsOtherProbeFailures(t *testing.T) {
	writes := 0
	gvisor := &stackGVisor{
		echoProber: &failingEchoProber{err: errEchoOverloaded},
		rawICMPWriter: func(tcpip.NetworkProtocolNumber, []byte, tcpip.Address, tcpip.Address, uint8) error {
			writes++
			return nil
		},
	}
	packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(testICMPv4EchoRequest(t))})
	defer packet.DecRef()
	gvisor.handleICMPv4Packet(stack.TransportEndpointID{RemoteAddress: tcpip.AddrFrom4([4]byte{10, 0, 0, 2}), LocalAddress: tcpip.AddrFrom4([4]byte{198, 51, 100, 1})}, packet)
	if writes != 0 {
		t.Fatalf("writes = %d, want 0", writes)
	}
}

type failingEchoProber struct {
	err error
}

func (p *failingEchoProber) Probe(echoRequest, func(echoResult)) error { return p.err }

func (p *failingEchoProber) Close() error { return nil }

func testICMPv4EchoRequest(t *testing.T) []byte {
	t.Helper()
	request := []byte{byte(header.ICMPv4Echo), 0, 0, 0, 0x12, 0x34, 0x56, 0x78}
	request = append(request, []byte("payload")...)
	if err := tunicmp.RewriteChecksum(header.IPv4ProtocolNumber, request, tcpip.Address{}, tcpip.Address{}); err != nil {
		t.Fatal(err)
	}
	return request
}

func testICMPv6EchoRequest(t *testing.T) []byte {
	t.Helper()
	source := tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 2})
	destination := tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 1})
	request := []byte{byte(header.ICMPv6EchoRequest), 0, 0, 0, 0x12, 0x34, 0x56, 0x78}
	request = append(request, []byte("payload")...)
	if err := tunicmp.RewriteChecksum(header.IPv6ProtocolNumber, request, source, destination); err != nil {
		t.Fatal(err)
	}
	return request
}

func testIPv6EchoPacket(t *testing.T) []byte {
	t.Helper()
	request := testICMPv6EchoRequest(t)
	packet := make([]byte, header.IPv6MinimumSize+len(request))
	header.IPv6(packet).Encode(&header.IPv6Fields{
		PayloadLength:     uint16(len(request)),
		TransportProtocol: header.ICMPv6ProtocolNumber,
		HopLimit:          64,
		SrcAddr:           tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 2}),
		DstAddr:           tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 1}),
	})
	copy(packet[header.IPv6MinimumSize:], request)
	return packet
}

func parseEchoReply(message []byte) (uint16, uint16, bool) {
	if len(message) < header.ICMPv4MinimumSize || header.ICMPv4(message).Type() != header.ICMPv4EchoReply {
		return 0, 0, false
	}
	header := header.ICMPv4(message)
	return header.Ident(), header.Sequence(), true
}

type writtenEchoPacket struct {
	protocol    tcpip.NetworkProtocolNumber
	message     []byte
	source      tcpip.Address
	destination tcpip.Address
	ttl         uint8
}

type delayedEchoProber struct {
	mu           sync.Mutex
	completeFunc func(echoResult)
	request      echoRequest
	probes       int
	closed       bool
}

func (p *delayedEchoProber) Probe(request echoRequest, complete func(echoResult)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errEchoProberClosed
	}
	p.request = request
	p.probes++
	p.completeFunc = complete
	return nil
}

func (p *delayedEchoProber) snapshot() (int, echoRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probes, p.request
}

func (p *delayedEchoProber) complete(result echoResult) {
	p.mu.Lock()
	complete := p.completeFunc
	p.mu.Unlock()
	if complete != nil {
		complete(result)
	}
}

func (p *delayedEchoProber) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

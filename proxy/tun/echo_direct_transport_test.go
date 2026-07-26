package tun

import (
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	tunicmp "github.com/xtls/xray-core/proxy/tun/icmp"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func TestParseEchoWireReply(t *testing.T) {
	for _, test := range []struct {
		name        string
		family      echoFamily
		message     icmp.Message
		source      string
		destination string
	}{
		{name: "IPv4", family: echoIPv4, message: icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: 12, Seq: 34, Data: []byte("v4")}}, source: "198.51.100.1", destination: "192.0.2.1"},
		{name: "IPv6", family: echoIPv6, message: icmp.Message{Type: ipv6.ICMPTypeEchoReply, Body: &icmp.Echo{ID: 56, Seq: 78, Data: []byte("v6")}}, source: "2001:db8::1", destination: "2001:db8::2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			packet, err := test.message.Marshal(nil)
			if err != nil {
				t.Fatal(err)
			}
			source := netip.MustParseAddr(test.source)
			destination := netip.MustParseAddr(test.destination)
			protocol := header.IPv6ProtocolNumber
			if test.family == echoIPv4 {
				protocol = header.IPv4ProtocolNumber
			}
			if err := tunicmp.RewriteChecksum(protocol, packet, tcpip.AddrFromSlice(source.AsSlice()), tcpip.AddrFromSlice(destination.AsSlice())); err != nil {
				t.Fatal(err)
			}
			reply, ok := parseEchoWireReply(test.family, packet, source, destination, 41)
			if !ok || reply.Identifier != uint16(test.message.Body.(*icmp.Echo).ID) || reply.Sequence != uint16(test.message.Body.(*icmp.Echo).Seq) || reply.TTL != 41 {
				t.Fatalf("reply = %+v, ok=%v", reply, ok)
			}
		})
	}
}

func TestRawEchoTransportEncodesRequestThroughPacketIO(t *testing.T) {
	packet := &fakeEchoPacketIO{}
	transport := &rawEchoTransport{family: echoIPv4, packet: packet, closed: make(chan struct{})}
	request := echoWireRequest{Destination: netip.MustParseAddr("198.51.100.1"), Identifier: 12, Sequence: 34, Payload: []byte("payload")}
	if err := transport.Send(request); err != nil {
		t.Fatal(err)
	}
	message, err := icmp.ParseMessage(1, packet.written)
	if err != nil {
		t.Fatal(err)
	}
	echo := message.Body.(*icmp.Echo)
	if message.Type != ipv4.ICMPTypeEcho || echo.ID != 12 || echo.Seq != 34 || string(echo.Data) != "payload" || packet.destination != request.Destination {
		t.Fatalf("wire request = type %v body %+v destination %s", message.Type, echo, packet.destination)
	}
}

func TestRawEchoTransportStopsAfterPersistentReadError(t *testing.T) {
	packet := &fakeEchoPacketIO{readErr: errors.New("persistent read failure")}
	transport := &rawEchoTransport{family: echoIPv4, packet: packet, closed: make(chan struct{})}
	transport.wait.Add(1)
	go transport.receive()
	done := make(chan struct{})
	go func() {
		transport.wait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("receive loop spun after persistent read error")
	}
}

func TestRawEchoTransportIgnoresInvalidPacketMetadata(t *testing.T) {
	packet := &fakeEchoPacketIO{readErrors: []error{errInvalidEchoReplyMetadata, errors.New("socket failed")}}
	transport := &rawEchoTransport{family: echoIPv4, packet: packet, closed: make(chan struct{})}
	transport.wait.Add(1)
	go transport.receive()
	transport.wait.Wait()
	packet.mu.Lock()
	readCalls := packet.readCalls
	packet.mu.Unlock()
	if readCalls != 2 {
		t.Fatalf("Read() calls = %d, want invalid packet ignored before socket failure", readCalls)
	}
}

func TestParseEchoWireReplyRejectsUnrelatedAndMissingMetadata(t *testing.T) {
	request, err := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: 1}}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parseEchoWireReply(echoIPv4, request, netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("192.0.2.1"), 64); ok {
		t.Fatal("accepted Echo request as reply")
	}
	reply, err := (&icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: 1}}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parseEchoWireReply(echoIPv4, reply, netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("192.0.2.1"), 0); ok {
		t.Fatal("accepted reply without TTL")
	}
	reply[len(reply)-1] ^= 0xff
	if _, ok := parseEchoWireReply(echoIPv4, reply, netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("192.0.2.1"), 64); ok {
		t.Fatal("accepted reply with invalid checksum")
	}
}

type fakeEchoPacketIO struct {
	mu          sync.Mutex
	written     []byte
	destination netip.Addr
	readErr     error
	readErrors  []error
	readCalls   int
}

func (p *fakeEchoPacketIO) Write(packet []byte, destination netip.Addr) error {
	p.mu.Lock()
	p.written = append([]byte(nil), packet...)
	p.destination = destination
	p.mu.Unlock()
	return nil
}

func (p *fakeEchoPacketIO) Read([]byte) (int, netip.Addr, netip.Addr, uint8, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readCalls++
	if len(p.readErrors) > 0 {
		err := p.readErrors[0]
		p.readErrors = p.readErrors[1:]
		return 0, netip.Addr{}, netip.Addr{}, 0, err
	}
	return 0, netip.Addr{}, netip.Addr{}, 0, p.readErr
}

func (*fakeEchoPacketIO) Close() error { return nil }

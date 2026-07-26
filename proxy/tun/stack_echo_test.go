package tun

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
)

var (
	echoClient4 = netip.MustParseAddr("10.0.0.2")
	echoTarget4 = netip.MustParseAddr("198.51.100.1")
	echoClient6 = netip.MustParseAddr("2001:db8::2")
	echoTarget6 = netip.MustParseAddr("2001:db8::1")
)

// newEchoTestStack is the stack of a TUN handler whose echo requests go to prober. What it writes goes to the
// returned channel
func newEchoTestStack(t *testing.T, prober echoProber, gateway4, gateway6 netip.Addr) (*stack, *link, chan []byte) {
	l := newLink()
	written := make(chan []byte, 4)
	l.out = func(packet []byte) { written <- packet }
	s := &stack{
		ctx: context.Background(), tun: l, mtu: testMTU, window: tcpWindow,
		handle: func(conn net.Conn, _ net.Destination) { conn.Close() },
		conns:  make(map[flow]*tcpConn), sockets: make(map[netip.AddrPort]*udpSocket),
		echo: prober, gateway4: gateway4, gateway6: gateway6,
	}
	s.Start()
	t.Cleanup(func() {
		s.Close()
		l.Close()
	})
	return s, l, written
}

// echoRequestPacket is an echo request from src to dst with the identifier 0x1234 and the sequence number 0x5678,
// without a checksum
func echoRequestPacket(src, dst netip.Addr, payload []byte) []byte {
	proto, request := byte(protoICMPv4), byte(8)
	if src.Is6() {
		proto, request = protoICMPv6, 128
	}
	var ip [ipv6HeaderSize]byte
	n := putIPHeader(ip[:], proto, src, dst)
	setIPLength(ip[:n], n+8+len(payload))
	return append(append(ip[:n:n], request, 0, 0, 0, 0x12, 0x34, 0x56, 0x78), payload...)
}

func hopLimit(raw []byte) uint8 {
	if raw[0]>>4 == 4 {
		return raw[8]
	}
	return raw[7]
}

func nothingWritten(t *testing.T, written chan []byte) {
	t.Helper()
	select {
	case raw := <-written:
		t.Fatalf("unexpected packet written: %x", raw)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEchoProbeAnswersWithProbedReply(t *testing.T) {
	for _, test := range []struct {
		name           string
		client, target netip.Addr
		reply          byte
	}{
		{name: "IPv4", client: echoClient4, target: echoTarget4, reply: 0},
		{name: "IPv6", client: echoClient6, target: echoTarget6, reply: 129},
	} {
		t.Run(test.name, func(t *testing.T) {
			prober := &delayedEchoProber{}
			_, l, written := newEchoTestStack(t, prober, netip.Addr{}, netip.Addr{})
			l.in <- echoRequestPacket(test.client, test.target, []byte("request"))

			request := prober.waitRequest(t)
			if request.Source != test.client || request.Destination != test.target || request.Identifier != 0x1234 || request.Sequence != 0x5678 || string(request.Payload) != "request" {
				t.Fatalf("probe request = %+v", request)
			}
			nothingWritten(t, written)

			prober.complete(echoResult{Reply: &echoReply{Identifier: 0x1234, Sequence: 0x5678, Payload: []byte("payload"), TTL: 39}})
			raw := <-written
			p, ok := parsePacket(raw, 0)
			if !ok || p.src != test.target || p.dst != test.client || hopLimit(raw) != 39 || !p.verified() {
				t.Fatalf("Echo reply packet = %x", raw)
			}
			m := p.payload
			if m[0] != test.reply || m[1] != 0 || binary.BigEndian.Uint16(m[4:]) != 0x1234 || binary.BigEndian.Uint16(m[6:]) != 0x5678 || string(m[8:]) != "payload" {
				t.Fatalf("Echo reply message = %x", m)
			}
		})
	}
}

func TestEchoProbeDoesNotFallBackAfterFailureOrShutdown(t *testing.T) {
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
			s, l, written := newEchoTestStack(t, prober, netip.Addr{}, netip.Addr{})
			l.in <- echoRequestPacket(echoClient4, echoTarget4, nil)
			prober.waitRequest(t)
			if test.close {
				_ = s.Close()
			}
			prober.complete(test.result)
			nothingWritten(t, written)
		})
	}
}

func TestEchoProbeDropsOtherSubmitFailures(t *testing.T) {
	_, l, written := newEchoTestStack(t, &failingEchoProber{err: errEchoOverloaded}, netip.Addr{}, netip.Addr{})
	l.in <- echoRequestPacket(echoClient4, echoTarget4, nil)
	nothingWritten(t, written)
}

func TestEchoProbeReportsUnreachableWithoutCarrier(t *testing.T) {
	for _, test := range []struct {
		name           string
		client, target netip.Addr
		gateway        netip.Addr
		payload        int
		// kind is the type of the error message, request the one of the quoted request
		kind, request byte
		// size is the size of the error message when the quote is cut
		size int
	}{
		{name: "ipv4 with gateway", client: echoClient4, target: echoTarget4, gateway: netip.MustParseAddr("172.18.0.1"), kind: 3, request: 8},
		{name: "ipv4 without gateway", client: echoClient4, target: echoTarget4, kind: 3, request: 8},
		{name: "ipv4 quote is bounded", client: echoClient4, target: echoTarget4, payload: 1000, kind: 3, request: 8, size: maxIPv4UnreachableSize},
		{name: "ipv6 with gateway", client: echoClient6, target: echoTarget6, gateway: netip.MustParseAddr("fdfe:dcba:8964::1"), kind: 1, request: 128},
		{name: "ipv6 quote is bounded", client: echoClient6, target: echoTarget6, payload: 1400, kind: 1, request: 128, size: maxIPv6UnreachableSize},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gateway4, gateway6 netip.Addr
			if test.client.Is4() {
				gateway4 = test.gateway
			} else {
				gateway6 = test.gateway
			}
			_, l, written := newEchoTestStack(t, &failingEchoProber{err: errEchoCarrierUnknown}, gateway4, gateway6)
			l.in <- echoRequestPacket(test.client, test.target, bytes.Repeat([]byte{'x'}, test.payload))

			raw := <-written
			p, ok := parsePacket(raw, 0)
			reporter := test.target
			if test.gateway.IsValid() {
				reporter = test.gateway
			}
			if !ok || p.src != reporter || p.dst != test.client || !p.verified() {
				t.Fatalf("unreachable packet = %x, want from %v to %v", raw, reporter, test.client)
			}
			m := p.payload
			if m[0] != test.kind || m[1] != 0 {
				t.Fatalf("unreachable type/code = %d/%d", m[0], m[1])
			}
			if test.size != 0 && len(m) != test.size {
				t.Fatalf("unreachable message size = %d, want %d", len(m), test.size)
			}
			quoted := m[8:]
			size := ipv6HeaderSize
			if test.client.Is4() {
				size = ipv4HeaderSize
				if checksum(sum(quoted[:size], 0)) != 0 {
					t.Fatal("quoted IPv4 header checksum is wrong")
				}
			}
			// a cut quote is shorter than its header says, which parsePacket does not take
			q, ok := parsePacket(append(bytes.Clone(quoted), make([]byte, test.payload)...), 0)
			inner := quoted[size:]
			if !ok || q.src != test.client || q.dst != test.target || inner[0] != test.request ||
				binary.BigEndian.Uint16(inner[4:]) != 0x1234 || binary.BigEndian.Uint16(inner[6:]) != 0x5678 {
				t.Fatalf("quoted request = %x", quoted)
			}
		})
	}
}

type failingEchoProber struct {
	err error
}

func (p *failingEchoProber) Probe(echoRequest, func(echoResult)) error { return p.err }

func (p *failingEchoProber) Close() error { return nil }

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

// waitRequest waits until the stack has submitted a probe and returns its request
func (p *delayedEchoProber) waitRequest(t *testing.T) echoRequest {
	t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		p.mu.Lock()
		probes, request := p.probes, p.request
		p.mu.Unlock()
		if probes > 0 {
			return request
		}
	}
	t.Fatal("no Echo probe was submitted")
	return echoRequest{}
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

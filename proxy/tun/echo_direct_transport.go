package tun

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"

	xerrors "github.com/xtls/xray-core/common/errors"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type echoPacketIO interface {
	Write([]byte, netip.Addr) error
	Read([]byte) (int, netip.Addr, netip.Addr, uint8, error)
	Close() error
}

type rawEchoTransportFactory struct{}

func newPlatformEchoTransportFactory() echoTransportFactory {
	return rawEchoTransportFactory{}
}

func (rawEchoTransportFactory) Open(family echoFamily, carrier carrierInterface, deliver func(echoWireReply)) (echoTransport, error) {
	packetIO, err := openEchoPacketIO(family, carrier)
	if err != nil {
		return nil, err
	}
	transport := &rawEchoTransport{
		family:  family,
		packet:  packetIO,
		deliver: deliver,
		closed:  make(chan struct{}),
	}
	transport.wait.Add(1)
	go transport.receive()
	return transport, nil
}

type rawEchoTransport struct {
	family  echoFamily
	packet  echoPacketIO
	deliver func(echoWireReply)
	closed  chan struct{}
	close   sync.Once
	wait    sync.WaitGroup
}

func (t *rawEchoTransport) Send(request echoWireRequest) error {
	message := icmp.Message{
		Code: 0,
		Body: &icmp.Echo{ID: int(request.Identifier), Seq: int(request.Sequence), Data: append([]byte(nil), request.Payload...)},
	}
	if t.family == echoIPv4 {
		message.Type = ipv4.ICMPTypeEcho
	} else {
		message.Type = ipv6.ICMPTypeEchoRequest
	}
	packet, err := message.Marshal(nil)
	if err != nil {
		return err
	}
	return t.packet.Write(packet, request.Destination)
}

func (t *rawEchoTransport) receive() {
	defer t.wait.Done()
	buffer := make([]byte, 64*1024)
	for {
		n, source, destination, ttl, err := t.packet.Read(buffer)
		if err != nil {
			if errors.Is(err, errInvalidEchoReplyMetadata) {
				continue
			}
			select {
			case <-t.closed:
				return
			default:
				xerrors.LogInfoInner(context.Background(), err, "[tun] Direct Echo receive loop stopped")
				return
			}
		}
		reply, ok := parseEchoWireReply(t.family, buffer[:n], source, destination, ttl)
		if ok {
			t.deliver(reply)
		}
	}
}

func parseEchoWireReply(family echoFamily, packet []byte, source, destination netip.Addr, ttl uint8) (echoWireReply, bool) {
	proto := byte(protoICMPv6)
	if family == echoIPv4 {
		proto = protoICMPv4
	}
	message, err := icmp.ParseMessage(int(proto), packet)
	if err != nil || message.Code != 0 || ttl == 0 || len(packet) < 4 {
		return echoWireReply{}, false
	}
	if echoWireChecksum(proto, packet, source, destination) != binary.BigEndian.Uint16(packet[2:]) {
		return echoWireReply{}, false
	}
	if family == echoIPv4 && message.Type != ipv4.ICMPTypeEchoReply {
		return echoWireReply{}, false
	}
	if family == echoIPv6 && message.Type != ipv6.ICMPTypeEchoReply {
		return echoWireReply{}, false
	}
	echo, ok := message.Body.(*icmp.Echo)
	if !ok || echo.ID < 0 || echo.ID > 0xffff || echo.Seq < 0 || echo.Seq > 0xffff {
		return echoWireReply{}, false
	}
	return echoWireReply{
		Source:     source.Unmap(),
		Identifier: uint16(echo.ID),
		Sequence:   uint16(echo.Seq),
		Payload:    append([]byte(nil), echo.Data...),
		TTL:        ttl,
	}, true
}

// echoWireChecksum is the checksum that the ICMP message m from source to
// destination should carry
func echoWireChecksum(proto byte, m []byte, source, destination netip.Addr) uint16 {
	m = bytes.Clone(m)
	m[2], m[3] = 0, 0
	if proto == protoICMPv4 {
		return icmpChecksum(proto, nil, m)
	}
	var ip [ipv6HeaderSize]byte
	n := putIPHeader(ip[:], proto, source, destination)
	return icmpChecksum(proto, ip[:n], m)
}

func (t *rawEchoTransport) Close() error {
	var closeErr error
	t.close.Do(func() {
		close(t.closed)
		closeErr = t.packet.Close()
		t.wait.Wait()
	})
	return closeErr
}

var (
	errDirectEchoUnsupported    = errors.New("Direct Echo probes are not supported on this platform")
	errInvalidEchoReplyMetadata = errors.New("invalid Direct Echo reply metadata")
)

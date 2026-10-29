package freedom_test

import (
	"context"
	gonet "net"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/policy"
	. "github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/pipe"
)

type udpDialer struct {
	mu    sync.Mutex
	conns []gonet.PacketConn // returned first, in order, before fresh sockets
	dials []gonet.PacketConn
}

func (d *udpDialer) Dial(ctx context.Context, dest net.Destination) (stat.Connection, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var pc gonet.PacketConn
	if len(d.conns) > 0 {
		pc, d.conns = d.conns[0], d.conns[1:]
	} else {
		var err error
		if pc, err = gonet.ListenPacket("udp", "127.0.0.1:0"); err != nil {
			return nil, err
		}
	}
	d.dials = append(d.dials, pc)
	return &net.PacketConnWrapper{PacketConn: pc, Dest: dest.RawNetAddr()}, nil
}

func (d *udpDialer) DestIpAddress() net.IP { return nil }

func (d *udpDialer) SetOutboundGateway(ctx context.Context, ob *session.Outbound) {}

func (d *udpDialer) dialed() []gonet.PacketConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]gonet.PacketConn(nil), d.dials...)
}

// A UDP socket that took the port of its TUN source would receive its own
// replies from the TUN and forward them forever, so freedom has to redial.
func TestUDPRedialsWhenSocketTakesTunSourcePort(t *testing.T) {
	taken, err := gonet.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	sourcePort := net.Port(taken.LocalAddr().(*gonet.UDPAddr).Port)

	h := new(Handler)
	if err := h.Init(&Config{}, policy.DefaultManager{}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = session.ContextWithInbound(ctx, &session.Inbound{
		Name:   "tun",
		Source: net.UDPDestination(net.LocalHostIP, sourcePort),
	})
	ctx = session.ContextWithOutbounds(ctx, []*session.Outbound{{
		Target: net.UDPDestination(net.LocalHostIP, 9),
	}})

	inputReader, inputWriter := pipe.New()
	outputReader, outputWriter := pipe.New()
	defer outputReader.Interrupt()
	defer inputWriter.Close()

	dialer := &udpDialer{conns: []gonet.PacketConn{taken}}
	done := make(chan error, 1)
	go func() {
		done <- h.Process(ctx, &transport.Link{Reader: inputReader, Writer: outputWriter}, dialer)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for len(dialer.dialed()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("freedom kept the socket on the port of its TUN source")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := taken.WriteTo([]byte{0}, taken.LocalAddr()); err == nil {
		t.Error("the socket on the port of the TUN source was not closed")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Process did not return after cancellation")
	}
	for _, pc := range dialer.dialed()[1:] {
		pc.Close()
	}
}

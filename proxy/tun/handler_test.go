package tun

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type testCounter struct {
	value int64
}

func (c *testCounter) Value() int64 {
	return atomic.LoadInt64(&c.value)
}

func (c *testCounter) Set(value int64) int64 {
	return atomic.SwapInt64(&c.value, value)
}

func (c *testCounter) Add(value int64) int64 {
	return atomic.AddInt64(&c.value, value) - value
}

type testConn struct {
	reader *bytes.Reader
	writer bytes.Buffer
}

func newTestConn(input []byte) *testConn {
	return &testConn{reader: bytes.NewReader(input)}
}

func (c *testConn) Read(payload []byte) (int, error) {
	return c.reader.Read(payload)
}

func (c *testConn) Write(payload []byte) (int, error) {
	return c.writer.Write(payload)
}

func (c *testConn) Close() error {
	return nil
}

func (c *testConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1080}
}

func (c *testConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 12345}
}

func (c *testConn) SetDeadline(time.Time) error {
	return nil
}

func (c *testConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *testConn) SetWriteDeadline(time.Time) error {
	return nil
}

type testDispatcher struct {
	writePayload []byte
	readBytes    int32
}

type closingStack struct {
	events *[]string
}

func (*closingStack) Start() error { return nil }

func (s *closingStack) Close() error {
	*s.events = append(*s.events, "stack")
	return nil
}

type closingProber struct {
	events *[]string
}

func (*closingProber) Probe(echoRequest, func(echoResult)) error { return nil }

func (p *closingProber) Close() error {
	*p.events = append(*p.events, "prober")
	return nil
}

type ownershipCheckingTun struct {
	events              *[]string
	acquiredDuringClose bool
}

func (*ownershipCheckingTun) Start() error { return nil }

func (t *ownershipCheckingTun) Close() error {
	*t.events = append(*t.events, "tun")
	policy, err := acquireOutboundCarrierPolicyWithBinder(nil, func(string, string, uintptr, *net.Interface) error { return nil })
	if err == nil {
		t.acquiredDuringClose = true
		_ = policy.Close()
	}
	return nil
}

func (*ownershipCheckingTun) Name() (string, error)                    { return "tun", nil }
func (*ownershipCheckingTun) Index() (int, error)                      { return 99, nil }
func (*ownershipCheckingTun) newEndpoint() (stack.LinkEndpoint, error) { return nil, nil }
func (*ownershipCheckingTun) StartOutboundCarrierTracking(*outboundCarrierPolicy, string) error {
	return nil
}

func (t *ownershipCheckingTun) StopOutboundCarrierTracking() error {
	*t.events = append(*t.events, "tracker")
	return nil
}

func TestHandlerCloseKeepsCarrierPolicyUntilTunRoutesAreRemoved(t *testing.T) {
	events := make([]string, 0, 3)
	policy, err := acquireOutboundCarrierPolicyWithBinder(nil, func(string, string, uintptr, *net.Interface) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = policy.Close() })
	tunInterface := &ownershipCheckingTun{events: &events}
	handler := &Handler{
		stack:          &closingStack{events: &events},
		tun:            tunInterface,
		carrierPolicy:  policy,
		carrierTracker: tunInterface,
	}

	if err := handler.Close(); err != nil {
		t.Fatal(err)
	}
	if tunInterface.acquiredDuringClose {
		t.Fatal("Outbound carrier policy ownership was released before TUN routes were removed")
	}
	if got := strings.Join(events, ","); got != "stack,tracker,tun" {
		t.Fatalf("close order = %s, want stack,tracker,tun", got)
	}

	restarted, err := acquireOutboundCarrierPolicyWithBinder(nil, func(string, string, uintptr, *net.Interface) error { return nil })
	if err != nil {
		t.Fatalf("Outbound carrier policy ownership remained after Handler.Close(): %v", err)
	}
	_ = restarted.Close()
}

func TestStartupCleanupKeepsCarrierPolicyUntilTunRoutesAreRemoved(t *testing.T) {
	events := make([]string, 0, 3)
	policy, err := acquireOutboundCarrierPolicyWithBinder(nil, func(string, string, uintptr, *net.Interface) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = policy.Close() })
	tunInterface := &ownershipCheckingTun{events: &events}

	if err := closeTunResources(nil, &closingProber{events: &events}, tunInterface, tunInterface, policy); err != nil {
		t.Fatal(err)
	}
	if tunInterface.acquiredDuringClose {
		t.Fatal("Outbound carrier policy ownership was released before startup cleanup removed TUN routes")
	}
	if got := strings.Join(events, ","); got != "prober,tracker,tun" {
		t.Fatalf("startup cleanup order = %s, want prober,tracker,tun", got)
	}

	restarted, err := acquireOutboundCarrierPolicyWithBinder(nil, func(string, string, uintptr, *net.Interface) error { return nil })
	if err != nil {
		t.Fatalf("Outbound carrier policy ownership remained after startup cleanup: %v", err)
	}
	_ = restarted.Close()
}

func (d *testDispatcher) Type() interface{} {
	return routing.DispatcherType()
}

func (d *testDispatcher) Start() error {
	return nil
}

func (d *testDispatcher) Close() error {
	return nil
}

func (d *testDispatcher) Dispatch(context.Context, xnet.Destination) (*transport.Link, error) {
	return nil, nil
}

func (d *testDispatcher) DispatchLink(ctx context.Context, dest xnet.Destination, link *transport.Link) error {
	mb, err := link.Reader.ReadMultiBuffer()
	if err != nil {
		return err
	}
	atomic.StoreInt32(&d.readBytes, mb.Len())
	buf.ReleaseMulti(mb)

	return link.Writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes(d.writePayload)})
}

func TestHandlerCountsTunConnectionTraffic(t *testing.T) {
	uplinkCounter := new(testCounter)
	downlinkCounter := new(testCounter)
	dispatcher := &testDispatcher{writePayload: []byte("downlink")}
	conn := newTestConn([]byte("uplink"))

	handler := &Handler{
		ctx:             context.Background(),
		config:          &Config{},
		dispatcher:      dispatcher,
		uplinkCounter:   uplinkCounter,
		downlinkCounter: downlinkCounter,
	}
	handler.HandleConnection(conn, xnet.TCPDestination(xnet.LocalHostIP, 443))

	if got := uplinkCounter.Value(); got != int64(len("uplink")) {
		t.Fatalf("unexpected uplink counter: got %d, want %d", got, len("uplink"))
	}
	if got := downlinkCounter.Value(); got != int64(len("downlink")) {
		t.Fatalf("unexpected downlink counter: got %d, want %d", got, len("downlink"))
	}
	if got := int(atomic.LoadInt32(&dispatcher.readBytes)); got != len("uplink") {
		t.Fatalf("dispatcher read unexpected bytes: got %d, want %d", got, len("uplink"))
	}
	if got := conn.writer.String(); got != "downlink" {
		t.Fatalf("connection write mismatch: got %q, want %q", got, "downlink")
	}
}

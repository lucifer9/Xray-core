package internet

import (
	"context"
	go_errors "errors"
	"io"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
)

var (
	Controllers           []func(network, address string, c syscall.RawConn) error
	ControllersLock       sync.Mutex
	scopedControllers     []scopedDialerController
	nextControllerID      uint64
	effectiveSystemDialer SystemDialer = &DefaultSystemDialer{}
)

type scopedDialerController struct {
	id         uint64
	controller func(network, address string, c syscall.RawConn) error
}

type dialerControllers []func(network, address string, c syscall.RawConn) error

func snapshotDialerControllers() dialerControllers {
	ControllersLock.Lock()
	defer ControllersLock.Unlock()

	controllers := make(dialerControllers, 0, len(Controllers)+len(scopedControllers))
	controllers = append(controllers, Controllers...)
	for _, scoped := range scopedControllers {
		controllers = append(controllers, scoped.controller)
	}
	return controllers
}

// HasDialerControllers reports whether the default system dialer has any
// permanent or scoped controllers registered.
func HasDialerControllers() bool {
	ControllersLock.Lock()
	defer ControllersLock.Unlock()
	return len(Controllers) > 0 || len(scopedControllers) > 0
}

func (s dialerControllers) control(network, address string, c syscall.RawConn) error {
	for _, ctl := range s {
		if err := ctl(network, address, c); err != nil {
			return errors.New("failed to apply external controller").Base(err)
		}
	}
	return nil
}

func (s dialerControllers) controlOutboundSocket(ctx context.Context, network, address string, c syscall.RawConn, sockopt *SocketConfig) error {
	if err := s.control(network, address, c); err != nil {
		return err
	}
	if sockopt == nil {
		return nil
	}
	var sockErr, controlErr error
	controlErr = c.Control(func(fd uintptr) {
		if sockErr = applyOutboundSocketOptions(network, address, fd, sockopt); sockErr != nil {
			errors.LogInfoInner(ctx, sockErr, "failed to apply socket options")
		}
	})
	if _, ok := sockErr.(*BindError); ok {
		return sockErr
	}
	return controlErr
}

// DialerControllerControl returns a control function backed by a stable snapshot
// of the currently registered dialer controllers.
func DialerControllerControl() func(network, address string, c syscall.RawConn) error {
	return snapshotDialerControllers().control
}

type SystemDialer interface {
	Dial(ctx context.Context, source net.Address, destination net.Destination, sockopt *SocketConfig) (net.Conn, error)
	DestIpAddress() net.IP
}

type DefaultSystemDialer struct {
	dns dns.Client
	obm outbound.Manager
}

func resolveSrcAddr(network net.Network, src net.Address) net.Addr {
	if src == nil || src == net.AnyIP {
		return nil
	}

	if network == net.Network_TCP {
		return &net.TCPAddr{
			IP:   src.IP(),
			Port: 0,
		}
	}

	return &net.UDPAddr{
		IP:   src.IP(),
		Port: 0,
	}
}

func (d *DefaultSystemDialer) Dial(ctx context.Context, src net.Address, dest net.Destination, sockopt *SocketConfig) (net.Conn, error) {
	errors.LogDebug(ctx, "dialing to "+dest.String())
	controllers := snapshotDialerControllers()

	if dest.Network == net.Network_UDP {
		destAddr, err := net.ResolveUDPAddr("udp", dest.NetAddr())
		if err != nil {
			return nil, err
		}
		srcAddr := resolveSrcAddr(net.Network_UDP, src)
		if srcAddr == nil {
			// some OS don't support mapped IPv4 dual stack
			// and need to select 0.0.0.0 or [::] manually based on the destination
			wildcard := net.AnyIP.IP()
			if destAddr.IP.To4() == nil {
				wildcard = net.AnyIPv6.IP()
			}
			srcAddr = &net.UDPAddr{
				IP:   wildcard,
				Port: 0,
			}
		}
		var lc net.ListenConfig
		if sockopt != nil || len(controllers) > 0 {
			lc.Control = func(network, _ string, c syscall.RawConn) error {
				// ListenPacket supplies a local address; dialer controllers need the destination.
				return controllers.controlOutboundSocket(ctx, network, destAddr.String(), c, sockopt)
			}
		}
		packetConn, err := lc.ListenPacket(ctx, srcAddr.Network(), srcAddr.String())
		var bindErr *BindError
		if err != nil && runtime.GOOS == "windows" && go_errors.As(err, &bindErr) {
			// Keep dual-stack UDP when possible. If the interface cannot bind both
			// families, retry a socket that cannot send on the unbound family.
			network := "udp6"
			if destAddr.IP.To4() != nil {
				network = "udp4"
			}
			packetConn, err = lc.ListenPacket(ctx, network, srcAddr.String())
		}
		if err != nil {
			return nil, err
		}
		return &net.PacketConnWrapper{
			PacketConn: packetConn,
			Dest:       destAddr,
		}, nil
	}
	// Chrome defaults
	keepAliveConfig := net.KeepAliveConfig{
		Enable:   true,
		Idle:     45 * time.Second,
		Interval: 45 * time.Second,
		Count:    -1,
	}
	keepAlive := time.Duration(0)
	if sockopt != nil {
		if sockopt.TcpKeepAliveIdle*sockopt.TcpKeepAliveInterval < 0 {
			return nil, errors.New("invalid TcpKeepAliveIdle or TcpKeepAliveInterval value: ", sockopt.TcpKeepAliveIdle, " ", sockopt.TcpKeepAliveInterval)
		}
		if sockopt.TcpKeepAliveIdle < 0 || sockopt.TcpKeepAliveInterval < 0 {
			keepAlive = -1
			keepAliveConfig.Enable = false
		}
		if sockopt.TcpKeepAliveIdle > 0 {
			keepAliveConfig.Idle = time.Duration(sockopt.TcpKeepAliveIdle) * time.Second
		}
		if sockopt.TcpKeepAliveInterval > 0 {
			keepAliveConfig.Interval = time.Duration(sockopt.TcpKeepAliveInterval) * time.Second
		}
	}
	dialer := &net.Dialer{
		Timeout:         time.Second * 16,
		LocalAddr:       resolveSrcAddr(dest.Network, src),
		KeepAlive:       keepAlive,
		KeepAliveConfig: keepAliveConfig,
	}

	if sockopt != nil || len(controllers) > 0 {
		if sockopt != nil && sockopt.TcpMptcp {
			dialer.SetMultipathTCP(true)
		}
		dialer.Control = func(network, address string, c syscall.RawConn) error {
			return controllers.controlOutboundSocket(ctx, network, address, c, sockopt)
		}
	}

	return dialer.DialContext(ctx, dest.Network.SystemString(), dest.NetAddr())
}

func (d *DefaultSystemDialer) DestIpAddress() net.IP {
	return nil
}

type SystemDialerAdapter interface {
	Dial(network string, address string) (net.Conn, error)
}

type SimpleSystemDialer struct {
	adapter SystemDialerAdapter
}

func WithAdapter(dialer SystemDialerAdapter) SystemDialer {
	return &SimpleSystemDialer{
		adapter: dialer,
	}
}

func (v *SimpleSystemDialer) Dial(ctx context.Context, src net.Address, dest net.Destination, sockopt *SocketConfig) (net.Conn, error) {
	return v.adapter.Dial(dest.Network.SystemString(), dest.NetAddr())
}

func (d *SimpleSystemDialer) DestIpAddress() net.IP {
	return nil
}

// UseAlternativeSystemDialer replaces the current system dialer with a given one.
// Caller must ensure there is no race condition.
//
// xray:api:stable
func UseAlternativeSystemDialer(dialer SystemDialer) {
	if dialer == nil {
		dialer = &DefaultSystemDialer{}
	}
	effectiveSystemDialer = dialer
}

// RegisterDialerController adds a controller to the effective system dialer.
// The controller can be used to operate on file descriptors before they are put into use.
// It receives the network and resolved remote address, as in net.Dialer.Control.
// It only works when effective dialer is the default dialer.
//
// xray:api:beta
func RegisterDialerController(ctl func(network, address string, c syscall.RawConn) error) error {
	if ctl == nil {
		return errors.New("nil listener controller")
	}

	ControllersLock.Lock()
	Controllers = append(Controllers, ctl)
	ControllersLock.Unlock()

	_, ok := effectiveSystemDialer.(*DefaultSystemDialer)
	if !ok {
		return errors.New("RegisterListenerController not supported in custom dialer")
	}

	return nil
}

type scopedDialerControllerRegistration struct {
	id   uint64
	once sync.Once
}

func (r *scopedDialerControllerRegistration) Close() error {
	r.once.Do(func() {
		ControllersLock.Lock()
		defer ControllersLock.Unlock()
		for i, scoped := range scopedControllers {
			if scoped.id == r.id {
				scopedControllers = append(scopedControllers[:i], scopedControllers[i+1:]...)
				return
			}
		}
	})
	return nil
}

// RegisterScopedDialerController registers a controller that stays active
// until the returned registration is closed. Like RegisterDialerController, an
// error returned by the controller aborts connection establishment. Closing the
// returned registration is idempotent.
func RegisterScopedDialerController(controller func(network, address string, c syscall.RawConn) error) (io.Closer, error) {
	if controller == nil {
		return nil, errors.New("nil dialer controller")
	}
	if _, ok := effectiveSystemDialer.(*DefaultSystemDialer); !ok {
		return nil, errors.New("RegisterScopedDialerController not supported in custom dialer")
	}

	ControllersLock.Lock()
	nextControllerID++
	registration := &scopedDialerControllerRegistration{id: nextControllerID}
	scopedControllers = append(scopedControllers, scopedDialerController{
		id:         registration.id,
		controller: controller,
	})
	ControllersLock.Unlock()

	return registration, nil
}

type FakePacketConn struct {
	net.Conn
}

func (c *FakePacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, err = c.Read(p)
	return n, &net.UDPAddr{IP: c.Conn.RemoteAddr().(*net.TCPAddr).IP, Port: c.Conn.RemoteAddr().(*net.TCPAddr).Port}, err
}

func (c *FakePacketConn) WriteTo(p []byte, _ net.Addr) (n int, err error) {
	return c.Write(p)
}

func (c *FakePacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: []byte{0, 0, 0, 0}}
}

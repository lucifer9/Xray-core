package internet

import (
	"context"
	"io"
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
	requiredControllers   []requiredDialerController
	nextControllerID      uint64
	effectiveSystemDialer SystemDialer = &DefaultSystemDialer{}
)

type requiredDialerController struct {
	id         uint64
	controller func(network, address string, c syscall.RawConn) error
}

type dialerControllerSnapshot struct {
	legacy   []func(network, address string, c syscall.RawConn) error
	required []requiredDialerController
}

func snapshotDialerControllers() dialerControllerSnapshot {
	ControllersLock.Lock()
	defer ControllersLock.Unlock()

	return dialerControllerSnapshot{
		legacy:   append([]func(network, address string, c syscall.RawConn) error(nil), Controllers...),
		required: append([]requiredDialerController(nil), requiredControllers...),
	}
}

// HasDialerControllers reports whether the default system dialer has any
// legacy or required controllers registered.
func HasDialerControllers() bool {
	ControllersLock.Lock()
	defer ControllersLock.Unlock()
	return len(Controllers) > 0 || len(requiredControllers) > 0
}

func (s dialerControllerSnapshot) control(ctx context.Context, network, address string, c syscall.RawConn) error {
	for _, controller := range s.legacy {
		if err := controller(network, address, c); err != nil {
			errors.LogInfoInner(ctx, err, "failed to apply external controller")
		}
	}
	for _, controller := range s.required {
		if err := controller.controller(network, address, c); err != nil {
			return errors.New("failed to apply required dialer controller").Base(err)
		}
	}
	return nil
}

func (s dialerControllerSnapshot) controlOutboundSocket(ctx context.Context, network, address string, c syscall.RawConn, sockopt *SocketConfig) error {
	if sockopt != nil {
		if err := c.Control(func(fd uintptr) {
			if err := applyOutboundSocketOptions(network, address, fd, sockopt); err != nil {
				errors.LogInfoInner(ctx, err, "failed to apply socket options")
			}
		}); err != nil {
			return err
		}
	}
	return s.control(ctx, network, address, c)
}

// DialerControllerControl returns a control function backed by a stable snapshot
// of the currently registered dialer controllers.
func DialerControllerControl(ctx context.Context) func(network, address string, c syscall.RawConn) error {
	controllers := snapshotDialerControllers()
	return func(network, address string, c syscall.RawConn) error {
		return controllers.control(ctx, network, address, c)
	}
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
		srcAddr := resolveSrcAddr(net.Network_UDP, src)
		if srcAddr == nil {
			srcAddr = &net.UDPAddr{
				IP:   []byte{0, 0, 0, 0},
				Port: 0,
			}
		}
		var lc net.ListenConfig
		destAddr, err := net.ResolveUDPAddr("udp", dest.NetAddr())
		if err != nil {
			return nil, err
		}
		lc.Control = func(network, address string, c syscall.RawConn) error {
			return controllers.controlOutboundSocket(ctx, network, destAddr.String(), c, sockopt)
		}
		packetConn, err := lc.ListenPacket(ctx, srcAddr.Network(), srcAddr.String())
		if err != nil {
			return nil, err
		}
		return &PacketConnWrapper{
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

	if sockopt != nil || len(controllers.legacy) > 0 || len(controllers.required) > 0 {
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

type PacketConnWrapper struct {
	net.PacketConn
	Dest net.Addr
}

func (c *PacketConnWrapper) Read(p []byte) (int, error) {
	n, _, err := c.PacketConn.ReadFrom(p)
	return n, err
}

func (c *PacketConnWrapper) Write(p []byte) (int, error) {
	return c.PacketConn.WriteTo(p, c.Dest)
}

func (c *PacketConnWrapper) RemoteAddr() net.Addr {
	return c.Dest
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

type requiredDialerControllerRegistration struct {
	id   uint64
	once sync.Once
}

func (r *requiredDialerControllerRegistration) Close() error {
	r.once.Do(func() {
		ControllersLock.Lock()
		defer ControllersLock.Unlock()
		for i, controller := range requiredControllers {
			if controller.id == r.id {
				requiredControllers = append(requiredControllers[:i], requiredControllers[i+1:]...)
				return
			}
		}
	})
	return nil
}

// RegisterRequiredDialerController registers a safety-critical controller.
// Unlike legacy controllers, an error returned by a required controller aborts
// connection establishment. Closing the returned registration is idempotent.
func RegisterRequiredDialerController(controller func(network, address string, c syscall.RawConn) error) (io.Closer, error) {
	if controller == nil {
		return nil, errors.New("nil dialer controller")
	}
	if _, ok := effectiveSystemDialer.(*DefaultSystemDialer); !ok {
		return nil, errors.New("RegisterRequiredDialerController not supported in custom dialer")
	}

	ControllersLock.Lock()
	nextControllerID++
	registration := &requiredDialerControllerRegistration{id: nextControllerID}
	requiredControllers = append(requiredControllers, requiredDialerController{
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
	return &net.UDPAddr{IP: c.Conn.LocalAddr().(*net.TCPAddr).IP, Port: c.Conn.LocalAddr().(*net.TCPAddr).Port}
}

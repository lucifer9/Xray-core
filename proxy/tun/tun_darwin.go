//go:build darwin

package tun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"unsafe"

	"github.com/xtls/xray-core/common/buf"
	xerrors "github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/platform"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const (
	utunControlName    = "com.apple.net.utun_control"
	sysprotoControl    = 2
	utunHeaderSize     = 4
	UTUN_OPT_IFNAME    = 2
	fallbackIPv4Prefix = "169.254.10.1/30"
)

const (
	SIOCAIFADDR6          = 2155899162 // netinet6/in6_var.h
	IN6_IFF_NODAD         = 0x0020     // netinet6/in6_var.h
	IN6_IFF_SECURED       = 0x0400     // netinet6/in6_var.h
	ND6_INFINITE_LIFETIME = 0xFFFFFFFF // netinet6/nd6.h
)

//go:linkname procyield runtime.procyield
func procyield(cycles uint32)

type DarwinTun struct {
	tunFile *os.File
	options *Config
	tunFd   int
	ownsFd  bool // true for macOS (we created the fd), false for iOS (fd from system)

	routeMonitor     *os.File
	routeMonitorOnce sync.Once
	systemRoutes     []netip.Prefix
}

var (
	_ Tun          = (*DarwinTun)(nil)
	_ GVisorDevice = (*DarwinTun)(nil)
)

func NewTun(options *Config) (Tun, error) {
	// Check if fd is provided via environment (iOS mode)
	fdStr := platform.NewEnvFlag(platform.TunFdKey).GetValue(func() string { return "" })
	if fdStr != "" {
		// iOS: use provided fd from NetworkExtension
		fd, err := strconv.Atoi(fdStr)
		if err != nil {
			return nil, err
		}

		if err = unix.SetNonblock(fd, true); err != nil {
			return nil, err
		}

		return &DarwinTun{
			tunFile: os.NewFile(uintptr(fd), "utun"),
			options: options,
			tunFd:   fd,
			ownsFd:  false,
		}, nil
	}

	// macOS: create our own utun interface
	var tunFile *os.File
	var name string
	var err error
	if options.Name == "" {
		tunFile, name, err = openAuto()
	} else {
		name = options.Name
		tunFile, err = open(name)
	}
	if err != nil {
		return nil, err
	}

	err = setup(name, options.MTU, options.Gateway)
	if err != nil {
		_ = tunFile.Close()
		return nil, err
	}

	return &DarwinTun{
		tunFile: tunFile,
		options: options,
		tunFd:   int(tunFile.Fd()),
		ownsFd:  true,
	}, nil
}

func (t *DarwinTun) Start() error {
	if !t.ownsFd {
		return nil
	}

	if err := t.setSystemRoutes(); err != nil {
		return err
	}

	if updater != nil {
		fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, 0)
		if err != nil {
			_ = t.unsetSystemRoutes()
			return err
		}
		t.routeMonitor = os.NewFile(uintptr(fd), "xray-route-monitor")
		go t.monitorRouteChanges()
	}
	return nil
}

func (t *DarwinTun) Close() error {
	t.routeMonitorOnce.Do(func() {
		if t.routeMonitor != nil {
			_ = t.routeMonitor.Close()
		}
	})
	routeErr := t.unsetSystemRoutes()
	if t.ownsFd {
		return xerrors.Combine(routeErr, t.tunFile.Close())
	}
	// iOS: don't close the fd, it's owned by NetworkExtension
	return routeErr
}

func (t *DarwinTun) monitorRouteChanges() {
	buffer := make([]byte, 64*1024)
	for {
		if _, err := t.routeMonitor.Read(buffer); err != nil {
			if !errors.Is(err, os.ErrClosed) {
				xerrors.LogInfoInner(context.Background(), err, "[tun] failed to monitor route changes")
			}
			return
		}
		if updater != nil {
			updater.Update()
		}
	}
}

func (t *DarwinTun) Name() (string, error) {
	return unix.GetsockoptString(t.tunFd, sysprotoControl, UTUN_OPT_IFNAME)
}

func (t *DarwinTun) Index() (int, error) {
	name, err := t.Name()
	if err != nil {
		return 0, err
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return iface.Index, nil
}

// WritePacket implements GVisorDevice method to write one packet to the tun device
func (t *DarwinTun) WritePacket(packet *stack.PacketBuffer) tcpip.Error {
	// request memory to write from reusable buffer pool
	b := buf.NewWithSize(int32(t.options.MTU) + utunHeaderSize)
	defer b.Release()

	// prepare Darwin specific packet header
	_, _ = b.Write([]byte{0x0, 0x0, 0x0, 0x0})
	// copy the bytes of slices that compose the packet into the allocated buffer
	for _, packetElement := range packet.AsSlices() {
		_, _ = b.Write(packetElement)
	}
	// fill Darwin specific header from the first raw packet byte, that we can access now
	var family byte
	switch b.Byte(4) >> 4 {
	case 4:
		family = unix.AF_INET
	case 6:
		family = unix.AF_INET6
	default:
		return &tcpip.ErrAborted{}
	}
	b.SetByte(3, family)

	if _, err := t.tunFile.Write(b.Bytes()); err != nil {
		if errors.Is(err, unix.EAGAIN) {
			return &tcpip.ErrWouldBlock{}
		}
		return &tcpip.ErrAborted{}
	}
	return nil
}

// ReadPacket implements GVisorDevice method to read one packet from the tun device
// It is expected that the method will not block, rather return ErrQueueEmpty when there is nothing on the line,
// which will make the stack call Wait which should implement desired push-back
func (t *DarwinTun) ReadPacket() (byte, *stack.PacketBuffer, error) {
	// request memory to write from reusable buffer pool
	b := buf.NewWithSize(int32(t.options.MTU) + utunHeaderSize)

	// read the bytes to the interface file
	n, err := b.ReadFrom(t.tunFile)
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		b.Release()
		return 0, nil, ErrQueueEmpty
	}
	if err != nil {
		b.Release()
		return 0, nil, err
	}

	// discard empty or sub-empty packets
	if n <= utunHeaderSize {
		b.Release()
		return 0, nil, ErrQueueEmpty
	}

	// network protocol version from first byte of the raw packet, the one that follows Darwin specific header
	version := b.Byte(utunHeaderSize) >> 4
	packetBuffer := buffer.MakeWithData(b.BytesFrom(utunHeaderSize))
	return version, stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload:           packetBuffer,
		IsForwardedPacket: true,
		OnRelease: func() {
			b.Release()
		},
	}), nil
}

// Wait some cpu cycles
func (t *DarwinTun) Wait() {
	procyield(1)
}

func (t *DarwinTun) newEndpoint() (stack.LinkEndpoint, error) {
	return &LinkEndpoint{deviceMTU: t.options.MTU, device: t}, nil
}

// openAuto creates a utun interface with kernel-assigned unit number.
// Passing Unit=0 to connect(2) tells the kernel to allocate the next available utun.
func openAuto() (*os.File, string, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, sysprotoControl)
	if err != nil {
		return nil, "", err
	}

	ctlInfo := &unix.CtlInfo{}
	copy(ctlInfo.Name[:], utunControlName)
	if err := unix.IoctlCtlInfo(fd, ctlInfo); err != nil {
		_ = unix.Close(fd)
		return nil, "", err
	}

	sockaddr := &unix.SockaddrCtl{
		ID:   ctlInfo.Id,
		Unit: 0, // let the kernel pick the next available utun
	}
	if err := unix.Connect(fd, sockaddr); err != nil {
		_ = unix.Close(fd)
		return nil, "", err
	}

	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, "", err
	}

	name, err := unix.GetsockoptString(fd, sysprotoControl, UTUN_OPT_IFNAME)
	if err != nil {
		_ = unix.Close(fd)
		return nil, "", err
	}

	return os.NewFile(uintptr(fd), name), name, nil
}

// open the interface, by creating new utunN if in the system and returning its file descriptor
func open(name string) (*os.File, error) {
	ifIndex := -1
	_, err := fmt.Sscanf(name, "utun%d", &ifIndex)
	if err != nil || ifIndex < 0 {
		return nil, errors.New("interface name must be utunN, where N is a number, e.g. utun9, utun11 and so on")
	}

	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, sysprotoControl)
	if err != nil {
		return nil, err
	}

	ctlInfo := &unix.CtlInfo{}
	copy(ctlInfo.Name[:], utunControlName)
	if err := unix.IoctlCtlInfo(fd, ctlInfo); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	sockaddr := &unix.SockaddrCtl{
		ID:   ctlInfo.Id,
		Unit: uint32(ifIndex) + 1,
	}
	if err := unix.Connect(fd, sockaddr); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	return os.NewFile(uintptr(fd), name), nil
}

// setup configures the tun interface with MTU and IP addresses from gateway config.
// Addresses are in CIDR format (e.g. "172.18.0.1/30", "fd00::1/126").
// On macOS, the tun interface is point-to-point: local and remote addresses
// are set to the gateway address itself (Addr == Dstaddr)
func setup(name string, MTU uint32, gateways []string) error {
	if err := setMTU(name, MTU); err != nil {
		return err
	}

	var prefix4, prefix6 netip.Prefix
	for _, gw := range gateways {
		prefix, err := netip.ParsePrefix(gw)
		if err != nil {
			continue
		}
		if prefix.Addr().Is4() && !prefix4.IsValid() {
			prefix4 = prefix
		} else if prefix.Addr().Is6() && !prefix6.IsValid() {
			prefix6 = prefix
		}
	}

	// Fallback: no gateway configured, use link-local address
	if !prefix4.IsValid() && !prefix6.IsValid() {
		prefix4, _ = netip.ParsePrefix(fallbackIPv4Prefix)
	}

	if prefix4.IsValid() {
		if err := setIPv4Address(name, prefix4); err != nil {
			return err
		}
	}
	if prefix6.IsValid() {
		if err := setIPv6Address(name, prefix6); err != nil {
			return err
		}
	}

	return nil
}

// setMTU sets MTU on the interface by given name
func setMTU(name string, mtu uint32) error {
	socket, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(socket)

	ifr := unix.IfreqMTU{MTU: int32(mtu)}
	copy(ifr.Name[:], name)
	return unix.IoctlSetIfreqMTU(socket, &ifr)
}

type ifAliasReq4 struct {
	Name    [unix.IFNAMSIZ]byte
	Addr    unix.RawSockaddrInet4
	Dstaddr unix.RawSockaddrInet4
	Mask    unix.RawSockaddrInet4
}

type ifAliasReq6 struct {
	Name     [unix.IFNAMSIZ]byte
	Addr     unix.RawSockaddrInet6
	Dstaddr  unix.RawSockaddrInet6
	Mask     unix.RawSockaddrInet6
	Flags    uint32
	Lifetime addrLifetime6
}

type addrLifetime6 struct {
	Expire    float64
	Preferred float64
	Vltime    uint32
	Pltime    uint32
}

// setIPv4Address assigns an IPv4 address to the tun interface.
// On macOS tun is point-to-point: Addr and Dstaddr are both set to the
// gateway address, following sing-tun convention.
func setIPv4Address(name string, prefix netip.Prefix) error {
	socket, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(socket)

	addr := prefix.Addr().As4()
	mask := netip.MustParseAddr(net.IP(net.CIDRMask(prefix.Bits(), 32)).String()).As4()

	ifReq := ifAliasReq4{
		Addr: unix.RawSockaddrInet4{
			Len:    unix.SizeofSockaddrInet4,
			Family: unix.AF_INET,
			Addr:   addr,
		},
		Dstaddr: unix.RawSockaddrInet4{
			Len:    unix.SizeofSockaddrInet4,
			Family: unix.AF_INET,
			Addr:   addr,
		},
		Mask: unix.RawSockaddrInet4{
			Len:    unix.SizeofSockaddrInet4,
			Family: unix.AF_INET,
			Addr:   mask,
		},
	}
	copy(ifReq.Name[:], name)
	if err = ioctlPtr(socket, unix.SIOCAIFADDR, unsafe.Pointer(&ifReq)); err != nil {
		return os.NewSyscallError("SIOCAIFADDR", err)
	}
	return nil
}

// setIPv6Address assigns an IPv6 address to the tun interface.
// For /128 prefixes, Dstaddr is set to Addr.Next() for point-to-point peer.
func setIPv6Address(name string, prefix netip.Prefix) error {
	socket, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(socket)

	addr := prefix.Addr().As16()
	mask := netip.MustParseAddr(net.IP(net.CIDRMask(prefix.Bits(), 128)).String()).As16()

	ifReq6 := ifAliasReq6{
		Addr: unix.RawSockaddrInet6{
			Len:    unix.SizeofSockaddrInet6,
			Family: unix.AF_INET6,
			Addr:   addr,
		},
		Mask: unix.RawSockaddrInet6{
			Len:    unix.SizeofSockaddrInet6,
			Family: unix.AF_INET6,
			Addr:   mask,
		},
		Flags: IN6_IFF_NODAD | IN6_IFF_SECURED,
		Lifetime: addrLifetime6{
			Vltime: ND6_INFINITE_LIFETIME,
			Pltime: ND6_INFINITE_LIFETIME,
		},
	}
	if prefix.Bits() == 128 {
		ifReq6.Dstaddr = unix.RawSockaddrInet6{
			Len:    unix.SizeofSockaddrInet6,
			Family: unix.AF_INET6,
			Addr:   prefix.Addr().Next().As16(),
		}
	}
	copy(ifReq6.Name[:], name)
	if err = ioctlPtr(socket, SIOCAIFADDR6, unsafe.Pointer(&ifReq6)); err != nil {
		return os.NewSyscallError("SIOCAIFADDR6", err)
	}
	return nil
}

func ioctlPtr(fd int, req uint, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func setinterface(network, address string, fd uintptr, iface *net.Interface) error {
	switch network {
	case "tcp6", "udp6", "ip6":
		return unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, iface.Index)
	case "tcp4", "udp4", "ip4":
		return unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, iface.Index)
	default:
		panic(network + " " + address)
	}
}

func findOutboundInterface(tunIndex int, fixedName string) (*net.Interface, error) {
	if fixedName != "" {
		iface, err := net.InterfaceByName(fixedName)
		if err != nil {
			return nil, err
		}
		if iface.Index == tunIndex {
			return nil, errors.New("outbound interface cannot be the TUN interface")
		}
		return iface, nil
	}

	rib, err := route.FetchRIB(unix.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, err
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil, err
	}

	var ipv6Index int
	for _, message := range messages {
		routeMessage, ok := message.(*route.RouteMessage)
		if !ok || routeMessage.Index == tunIndex {
			continue
		}
		if routeMessage.Flags&unix.RTF_UP == 0 || routeMessage.Flags&unix.RTF_GATEWAY == 0 {
			continue
		}

		family, ok := defaultRouteFamily(routeMessage)
		if !ok {
			continue
		}
		if family == unix.AF_INET {
			return usableDarwinInterface(routeMessage.Index)
		}
		if family == unix.AF_INET6 && ipv6Index == 0 {
			ipv6Index = routeMessage.Index
		}
	}

	if ipv6Index != 0 {
		return usableDarwinInterface(ipv6Index)
	}
	return nil, errors.New("default route not found")
}

func defaultRouteFamily(message *route.RouteMessage) (int, bool) {
	if len(message.Addrs) <= unix.RTAX_NETMASK {
		return 0, false
	}

	switch destination := message.Addrs[unix.RTAX_DST].(type) {
	case *route.Inet4Addr:
		mask, ok := message.Addrs[unix.RTAX_NETMASK].(*route.Inet4Addr)
		if !ok || destination.IP != netip.IPv4Unspecified().As4() {
			return 0, false
		}
		ones, bits := net.IPMask(mask.IP[:]).Size()
		return unix.AF_INET, ones == 0 && bits == 32
	case *route.Inet6Addr:
		mask, ok := message.Addrs[unix.RTAX_NETMASK].(*route.Inet6Addr)
		if !ok || destination.IP != netip.IPv6Unspecified().As16() {
			return 0, false
		}
		ones, bits := net.IPMask(mask.IP[:]).Size()
		return unix.AF_INET6, ones == 0 && bits == 128
	default:
		return 0, false
	}
}

func usableDarwinInterface(index int) (*net.Interface, error) {
	iface, err := net.InterfaceByIndex(index)
	if err != nil {
		return nil, err
	}
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return nil, errors.New("default route interface is not usable")
	}
	return iface, nil
}

func (t *DarwinTun) setSystemRoutes() error {
	routes, err := buildDarwinSystemRoutes(t.options.AutoSystemRoutingTable)
	if err != nil {
		return err
	}
	if len(routes) == 0 {
		return nil
	}

	tunIndex, err := t.Index()
	if err != nil {
		return err
	}
	for _, destination := range routes {
		if err := execDarwinRoute(unix.RTM_ADD, tunIndex, destination); err != nil {
			_ = t.unsetSystemRoutes()
			return xerrors.New("failed to add system route ", destination).Base(err)
		}
		t.systemRoutes = append(t.systemRoutes, destination)
	}
	return nil
}

func (t *DarwinTun) unsetSystemRoutes() error {
	var errs []error
	tunIndex, indexErr := t.Index()
	if indexErr != nil && len(t.systemRoutes) > 0 {
		errs = append(errs, indexErr)
	}
	for i := len(t.systemRoutes) - 1; i >= 0; i-- {
		destination := t.systemRoutes[i]
		if err := execDarwinRoute(unix.RTM_DELETE, tunIndex, destination); err != nil && !errors.Is(err, unix.ESRCH) {
			errs = append(errs, xerrors.New("failed to delete system route ", destination).Base(err))
		}
	}
	t.systemRoutes = nil
	return xerrors.Combine(errs...)
}

func buildDarwinSystemRoutes(configured []string) ([]netip.Prefix, error) {
	routes := make([]netip.Prefix, 0, len(configured))
	seen := make(map[netip.Prefix]struct{})

	appendRoute := func(prefix netip.Prefix) {
		prefix = prefix.Masked()
		if _, found := seen[prefix]; found {
			return
		}
		seen[prefix] = struct{}{}
		routes = append(routes, prefix)
	}

	for _, value := range configured {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, xerrors.New("invalid system route ", value).Base(err)
		}
		prefix = prefix.Masked()
		if prefix.Bits() == 0 {
			for _, protected := range darwinProtectedDefaultRoutes(prefix.Addr().Is4()) {
				appendRoute(protected)
			}
			continue
		}
		appendRoute(prefix)
	}

	return routes, nil
}

func darwinProtectedDefaultRoutes(ipv4 bool) []netip.Prefix {
	routes := make([]netip.Prefix, 0, 8)
	for i := 0; i < 8; i++ {
		if ipv4 {
			var address [4]byte
			address[0] = 1 << i
			routes = append(routes, netip.PrefixFrom(netip.AddrFrom4(address), 8-i))
		} else {
			var address [16]byte
			address[0] = 1 << i
			routes = append(routes, netip.PrefixFrom(netip.AddrFrom16(address), 8-i))
		}
	}
	return routes
}

func execDarwinRoute(messageType int, interfaceIndex int, destination netip.Prefix) error {
	message := route.RouteMessage{
		Type:    messageType,
		Version: unix.RTM_VERSION,
		Flags:   unix.RTF_STATIC | unix.RTF_GATEWAY,
		Seq:     1,
	}
	if messageType == unix.RTM_ADD {
		message.Flags |= unix.RTF_UP
	}

	if destination.Addr().Is4() {
		gatewayPrefix := netip.MustParsePrefix(gateway)
		message.Addrs = []route.Addr{
			unix.RTAX_DST:     &route.Inet4Addr{IP: destination.Addr().As4()},
			unix.RTAX_NETMASK: &route.Inet4Addr{IP: prefixMask4(destination.Bits())},
			unix.RTAX_GATEWAY: &route.Inet4Addr{IP: gatewayPrefix.Addr().As4()},
		}
	} else {
		message.Flags &^= unix.RTF_GATEWAY
		message.Index = interfaceIndex
		message.Addrs = []route.Addr{
			unix.RTAX_DST:     &route.Inet6Addr{IP: destination.Addr().As16()},
			unix.RTAX_NETMASK: &route.Inet6Addr{IP: prefixMask6(destination.Bits())},
			unix.RTAX_GATEWAY: &route.LinkAddr{Index: interfaceIndex},
		}
	}

	request, err := message.Marshal()
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	_, err = unix.Write(fd, request)
	return err
}

func prefixMask4(bits int) [4]byte {
	var mask [4]byte
	copy(mask[:], net.CIDRMask(bits, 32))
	return mask
}

func prefixMask6(bits int) [16]byte {
	var mask [16]byte
	copy(mask[:], net.CIDRMask(bits, 128))
	return mask
}

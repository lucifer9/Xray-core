//go:build darwin

package tun

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"unsafe"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/platform"
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
}

var _ Tun = (*DarwinTun)(nil)
var _ GVisorDevice = (*DarwinTun)(nil)

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
	return nil
}

func (t *DarwinTun) Close() error {
	if t.ownsFd {
		return t.tunFile.Close()
	}
	// iOS: don't close the fd, it's owned by NetworkExtension
	return nil
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
// are set to the gateway address itself (Addr == Dstaddr), matching sing-tun convention.
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
	var err1, err2 error

	switch network {
	case "tcp6", "udp6", "ip6":
		err1 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, iface.Index)
		fallthrough
	case "tcp4", "udp4", "ip4":
		err2 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, iface.Index)
	default:
		panic(network + " " + address)
	}

	return errors.Join(err1, err2)
}

package tun

import (
	"context"
	stderrors "errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/transport/internet"
)

var errNoOutboundCarrier = stderrors.New("no usable Outbound carrier interface found")

type carrierFamily uint8

const (
	carrierIPv4 carrierFamily = 4
	carrierIPv6 carrierFamily = 6
)

type carrierInterface struct {
	Name  string
	Index int
}

type outboundCarrierPolicy struct {
	mu             sync.RWMutex
	ipv4           *carrierInterface
	ipv6           *carrierInterface
	binder         func(network, address string, fd uintptr, iface *net.Interface) error
	close          sync.Once
	control        interface{ Close() error }
	subscribers    map[uint64]func(carrierFamily, *carrierInterface)
	nextSubscriber uint64
	refreshFailure map[carrierFamily]string
}

var outboundCarrierOwnership struct {
	sync.Mutex
	owner *outboundCarrierPolicy
}

func acquireOutboundCarrierPolicy() (*outboundCarrierPolicy, error) {
	return acquireOutboundCarrierPolicyWithBinder(setinterface)
}

func acquireOutboundCarrierPolicyWithBinder(binder func(string, string, uintptr, *net.Interface) error) (*outboundCarrierPolicy, error) {
	policy := &outboundCarrierPolicy{
		binder:      binder,
		subscribers: make(map[uint64]func(carrierFamily, *carrierInterface)),
	}

	outboundCarrierOwnership.Lock()
	if outboundCarrierOwnership.owner != nil {
		outboundCarrierOwnership.Unlock()
		return nil, errors.New("Outbound carrier policy is already owned by another TUN inbound")
	}
	outboundCarrierOwnership.owner = policy
	outboundCarrierOwnership.Unlock()

	registration, err := internet.RegisterRequiredDialerController(policy.controlConnectionLeg)
	if err != nil {
		outboundCarrierOwnership.Lock()
		if outboundCarrierOwnership.owner == policy {
			outboundCarrierOwnership.owner = nil
		}
		outboundCarrierOwnership.Unlock()
		return nil, err
	}
	policy.control = registration
	return policy, nil
}

func (p *outboundCarrierPolicy) Close() error {
	p.close.Do(func() {
		if p.control != nil {
			_ = p.control.Close()
		}
		outboundCarrierOwnership.Lock()
		if outboundCarrierOwnership.owner == p {
			outboundCarrierOwnership.owner = nil
		}
		outboundCarrierOwnership.Unlock()
	})
	return nil
}

func (p *outboundCarrierPolicy) update(family carrierFamily, iface *net.Interface) {
	p.mu.Lock()
	var carrier *carrierInterface
	if iface != nil {
		carrier = &carrierInterface{Name: iface.Name, Index: iface.Index}
	}
	var previous *carrierInterface
	if family == carrierIPv4 {
		previous = p.ipv4
		if sameCarrierInterface(previous, carrier) {
			p.mu.Unlock()
			return
		}
		p.ipv4 = carrier
	} else {
		previous = p.ipv6
		if sameCarrierInterface(previous, carrier) {
			p.mu.Unlock()
			return
		}
		p.ipv6 = carrier
	}
	subscribers := make([]func(carrierFamily, *carrierInterface), 0, len(p.subscribers))
	for _, subscriber := range p.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	p.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber(family, carrier)
	}
}

func sameCarrierInterface(left, right *carrierInterface) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Name == right.Name && left.Index == right.Index
}

func (p *outboundCarrierPolicy) usesSystemPath(destination netip.Addr) bool {
	return destination.Unmap().IsLoopback()
}

func (p *outboundCarrierPolicy) subscribeWithSnapshot(subscriber func(carrierFamily, *carrierInterface)) (func(), map[carrierFamily]*carrierInterface) {
	p.mu.Lock()
	p.nextSubscriber++
	id := p.nextSubscriber
	p.subscribers[id] = subscriber
	snapshot := map[carrierFamily]*carrierInterface{
		carrierIPv4: cloneCarrierInterface(p.ipv4),
		carrierIPv6: cloneCarrierInterface(p.ipv6),
	}
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			delete(p.subscribers, id)
			p.mu.Unlock()
		})
	}, snapshot
}

func cloneCarrierInterface(carrier *carrierInterface) *carrierInterface {
	if carrier == nil {
		return nil
	}
	copy := *carrier
	return &copy
}

func (p *outboundCarrierPolicy) carrier(family carrierFamily) *carrierInterface {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var carrier *carrierInterface
	if family == carrierIPv4 {
		carrier = p.ipv4
	} else {
		carrier = p.ipv6
	}
	if carrier == nil {
		return nil
	}
	copy := *carrier
	return &copy
}

func (p *outboundCarrierPolicy) controlConnectionLeg(network, address string, raw syscall.RawConn) error {
	destination, exempt, err := p.governedDestination(address)
	if err != nil {
		return err
	}
	if exempt {
		return nil
	}
	family := carrierIPv6
	if destination.Is4() {
		family = carrierIPv4
	}
	carrier := p.carrier(family)
	if carrier == nil {
		return errors.New("Outbound carrier interface is unknown for ", destination)
	}
	iface := &net.Interface{Name: carrier.Name, Index: carrier.Index}
	if network == "tcp" || network == "udp" || network == "ip" {
		if destination.Is4() {
			network += "4"
		} else {
			network += "6"
		}
	}
	var bindErr error
	if err := raw.Control(func(fd uintptr) {
		bindErr = p.binder(network, address, fd, iface)
	}); err != nil {
		return err
	}
	return bindErr
}

func (p *outboundCarrierPolicy) governedDestination(address string) (netip.Addr, bool, error) {
	if strings.HasPrefix(strings.ToLower(address), "localhost:") {
		return netip.Addr{}, true, nil
	}
	addressPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return netip.Addr{}, false, errors.New("invalid connection-leg destination ", address).Base(err)
	}
	destination := addressPort.Addr().Unmap()
	if p.usesSystemPath(destination) {
		return destination, true, nil
	}
	return destination, false, nil
}

// noteRefreshFailure reports whether this failure should be logged: the
// first failure for a family, or a failure whose reason changed. Identical
// repeated failures (for example on every netlink route event) are suppressed.
func (p *outboundCarrierPolicy) noteRefreshFailure(family carrierFamily, err error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refreshFailure == nil {
		p.refreshFailure = make(map[carrierFamily]string)
	}
	message := err.Error()
	if previous, ok := p.refreshFailure[family]; ok && previous == message {
		return false
	}
	p.refreshFailure[family] = message
	return true
}

func (p *outboundCarrierPolicy) clearRefreshFailure(family carrierFamily) {
	p.mu.Lock()
	delete(p.refreshFailure, family)
	p.mu.Unlock()
}

func (p *outboundCarrierPolicy) refresh(tunIndex int, fixedName string) error {
	var refreshErrors []error
	for _, family := range []carrierFamily{carrierIPv4, carrierIPv6} {
		iface, err := findOutboundInterface(family, tunIndex, fixedName)
		if err != nil {
			if p.noteRefreshFailure(family, err) {
				errors.LogInfoInner(context.Background(), err, "[tun] failed to update ", familyLabel(family), " Outbound carrier interface")
			}
			p.update(family, nil)
			if fixedName != "" || !stderrors.Is(err, errNoOutboundCarrier) {
				refreshErrors = append(refreshErrors, errors.New("failed to select ", familyLabel(family), " Outbound carrier interface").Base(err))
			}
			continue
		}
		p.clearRefreshFailure(family)
		p.update(family, iface)
	}
	return errors.Combine(refreshErrors...)
}

func familyLabel(family carrierFamily) string {
	if family == carrierIPv4 {
		return "IPv4"
	}
	return "IPv6"
}

func validateFixedCarrierInterface(iface *net.Interface, tunIndex int) error {
	if iface.Index == tunIndex {
		return errors.New("Outbound carrier interface cannot be the TUN interface")
	}
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return errors.New("configured Outbound carrier interface is not usable")
	}
	return nil
}

type outboundCarrierTracker interface {
	StartOutboundCarrierTracking(*outboundCarrierPolicy, string) error
	StopOutboundCarrierTracking() error
}

func startOutboundCarrierPolicy(tunInterface Tun, config *Config) (*outboundCarrierPolicy, outboundCarrierTracker, error) {
	if config.AutoOutboundsInterface == "" {
		return nil, nil, nil
	}
	if err := validateOutboundCarrierBinding(); err != nil {
		return nil, nil, err
	}
	tunIndex, err := tunInterface.Index()
	if err != nil {
		return nil, nil, err
	}
	fixedName := config.AutoOutboundsInterface
	if fixedName == "auto" {
		fixedName = ""
	}
	policy, err := acquireOutboundCarrierPolicy()
	if err != nil {
		return nil, nil, err
	}
	if tracker, ok := tunInterface.(outboundCarrierTracker); ok {
		if err := tracker.StartOutboundCarrierTracking(policy, fixedName); err != nil {
			_ = policy.Close()
			return nil, nil, err
		}
		return policy, tracker, nil
	}
	if fixedName == "" {
		_ = policy.Close()
		return nil, nil, errors.New("automatic Outbound carrier interface tracking is not supported on this platform")
	}
	if err := policy.refresh(tunIndex, fixedName); err != nil {
		_ = policy.Close()
		return nil, nil, err
	}
	return policy, nil, nil
}

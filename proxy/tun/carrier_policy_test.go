package tun

import (
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestOutboundCarrierPolicyOwnershipLifecycle(t *testing.T) {
	first, err := acquireOutboundCarrierPolicyWithBinder(nil, func(string, string, uintptr, *net.Interface) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	if _, err := acquireOutboundCarrierPolicyWithBinder(nil, func(string, string, uintptr, *net.Interface) error { return nil }); err == nil {
		t.Fatal("second owner acquired Outbound carrier policy")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("idempotent Close() error = %v", err)
	}

	restarted, err := acquireOutboundCarrierPolicyWithBinder(nil, func(string, string, uintptr, *net.Interface) error { return nil })
	if err != nil {
		t.Fatalf("restart acquisition error = %v", err)
	}
	_ = restarted.Close()
}

func TestOutboundCarrierStartupOrdersObserverSnapshotBeforeRoutes(t *testing.T) {
	events := make([]string, 0, 3)
	tunInterface := &orderingTun{events: &events}
	policy, tracker, err := startOutboundCarrierPolicy(tunInterface, &Config{AutoOutboundsInterface: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tracker.StopOutboundCarrierTracking()
		_ = policy.Close()
	})
	if err := tunInterface.Start(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "observer,snapshot,routes" {
		t.Fatalf("startup order = %v", events)
	}
}

func TestOutboundCarrierStartupFailureReleasesOwnership(t *testing.T) {
	tunInterface := &orderingTun{events: &[]string{}, startErr: errors.New("observer failed")}
	if _, _, err := startOutboundCarrierPolicy(tunInterface, &Config{AutoOutboundsInterface: "auto"}); err == nil {
		t.Fatal("expected observer startup error")
	}
	policy, err := acquireOutboundCarrierPolicyWithBinder(nil, func(string, string, uintptr, *net.Interface) error { return nil })
	if err != nil {
		t.Fatalf("ownership remained after failed startup: %v", err)
	}
	_ = policy.Close()
}

func TestOutboundCarrierInitialSnapshotFailureIsReturned(t *testing.T) {
	policy := &outboundCarrierPolicy{subscribers: make(map[uint64]func(carrierFamily, *carrierInterface))}
	if err := policy.refresh(-1, "xray-interface-that-does-not-exist"); err == nil {
		t.Fatal("initial snapshot failure was ignored")
	}
}

type orderingTun struct {
	events   *[]string
	startErr error
}

func (t *orderingTun) StartOutboundCarrierTracking(policy *outboundCarrierPolicy, _ string) error {
	*t.events = append(*t.events, "observer")
	if t.startErr != nil {
		return t.startErr
	}
	policy.update(carrierIPv4, &net.Interface{Name: "carrier", Index: 1, Flags: net.FlagUp})
	*t.events = append(*t.events, "snapshot")
	return nil
}

func (*orderingTun) StopOutboundCarrierTracking() error       { return nil }
func (t *orderingTun) Start() error                           { *t.events = append(*t.events, "routes"); return nil }
func (*orderingTun) Close() error                             { return nil }
func (*orderingTun) Name() (string, error)                    { return "tun", nil }
func (*orderingTun) Index() (int, error)                      { return 99, nil }
func (*orderingTun) newEndpoint() (stack.LinkEndpoint, error) { return nil, nil }

func TestOutboundCarrierPolicyFamilyIsolationAndRecovery(t *testing.T) {
	var mu sync.Mutex
	var bound []string
	policy, err := acquireOutboundCarrierPolicyWithBinder(nil, func(network, _ string, _ uintptr, iface *net.Interface) error {
		mu.Lock()
		bound = append(bound, network+":"+iface.Name)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = policy.Close() })
	policy.update(carrierIPv4, &net.Interface{Name: "carrier4", Index: 4})

	raw := fakeRawConn{}
	if err := policy.controlConnectionLeg("tcp4", "198.51.100.1:443", raw); err != nil {
		t.Fatal(err)
	}
	if err := policy.controlConnectionLeg("tcp6", "[2001:db8::1]:443", raw); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown IPv6 carrier error = %v", err)
	}

	policy.update(carrierIPv6, &net.Interface{Name: "carrier6", Index: 6})
	if err := policy.controlConnectionLeg("tcp6", "[2001:db8::1]:443", raw); err != nil {
		t.Fatalf("IPv6 recovery error = %v", err)
	}
	policy.update(carrierIPv4, nil)
	if err := policy.controlConnectionLeg("tcp4", "198.51.100.1:443", raw); err == nil {
		t.Fatal("lost IPv4 carrier did not fail closed")
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(bound, ",") != "tcp4:carrier4,tcp6:carrier6" {
		t.Fatalf("bound connection legs = %v", bound)
	}
}

func TestOutboundCarrierPolicyExemptsLoopbackAndExcludedPrefixes(t *testing.T) {
	calls := 0
	policy, err := acquireOutboundCarrierPolicyWithBinder([]string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"}, func(string, string, uintptr, *net.Interface) error {
		calls++
		return errors.New("unexpected bind")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = policy.Close() })

	for _, address := range []string{"127.0.0.1:80", "localhost:53", "100.64.1.1:443", "[fd7a:115c:a1e0::1]:443"} {
		if err := policy.controlConnectionLeg("tcp4", address, fakeRawConn{}); err != nil {
			t.Fatalf("exempt destination %s error = %v", address, err)
		}
	}
	if calls != 0 {
		t.Fatalf("binder calls = %d, want 0", calls)
	}
	if err := policy.controlConnectionLeg("tcp4", "203.0.113.1:443", fakeRawConn{}); err == nil {
		t.Fatal("later external connection leg inherited exemption")
	}
}

type fakeRawConn struct{}

func (fakeRawConn) Control(f func(uintptr)) error  { f(42); return nil }
func (fakeRawConn) Read(func(uintptr) bool) error  { return nil }
func (fakeRawConn) Write(func(uintptr) bool) error { return nil }

var _ syscall.RawConn = fakeRawConn{}

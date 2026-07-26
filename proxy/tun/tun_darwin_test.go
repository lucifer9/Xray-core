//go:build darwin

package tun

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

func TestSelectDarwinGatewayDefault(t *testing.T) {
	gateway, err := selectDarwinGateway(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := gateway.String(); got != defaultDarwinGateway {
		t.Fatalf("unexpected default gateway: got %s, want %s", got, defaultDarwinGateway)
	}
}

func TestApplyDarwinSystemRoutesRollsBackInstalledRoutesInReverse(t *testing.T) {
	routes := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
	var deleted []netip.Prefix
	addCount := 0
	execute := func(messageType, _ int, destination, _ netip.Prefix) error {
		if messageType == unix.RTM_ADD {
			addCount++
			if addCount == 3 {
				return errors.New("install failed")
			}
		} else {
			deleted = append(deleted, destination)
		}
		return nil
	}
	installed, err := applyDarwinSystemRoutes(42, routes, netip.MustParsePrefix(defaultDarwinGateway), execute)
	if err == nil || len(installed) != 0 {
		t.Fatalf("installed=%v error=%v", installed, err)
	}
	if len(deleted) != 2 || deleted[0] != routes[1] || deleted[1] != routes[0] {
		t.Fatalf("rollback order = %v", deleted)
	}
}

// newTestSocketpair returns a connected AF_UNIX/SOCK_DGRAM pair -- a real
// fd DarwinTun.Wait's kqueue can register EVFILT_READ against, without
// needing an actual utun interface (which requires root/network
// entitlements this test environment doesn't have). Datagram sockets
// (unlike pipes) support both "write makes readable" and "close makes
// readable" the same way a tun fd's read-readiness behaves.
func newTestSocketpair(t *testing.T) (a, b int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})
	return fds[0], fds[1]
}

// Reviewer feedback, XTLS/Xray-core#6580: "blocking with no data" case --
// wait() must not return before the timeout when nothing is written.
func TestWaitKqueueBlocksWithNoData(t *testing.T) {
	a, _ := newTestSocketpair(t)
	kq := newWaitKqueue(a)
	if kq == nil {
		t.Fatal("newWaitKqueue returned nil")
	}
	defer kq.close()

	start := time.Now()
	ok := kq.wait(150 * time.Millisecond)
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("wait() returned false on a healthy kqueue with a plain timeout")
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("wait() returned after only %v, expected it to block close to the 150ms timeout", elapsed)
	}
}

// Reviewer feedback: "wake up with a readable fd" case.
func TestWaitKqueueWakesOnReadable(t *testing.T) {
	a, b := newTestSocketpair(t)
	kq := newWaitKqueue(a)
	if kq == nil {
		t.Fatal("newWaitKqueue returned nil")
	}
	defer kq.close()

	done := make(chan bool, 1)
	go func() {
		done <- kq.wait(5 * time.Second)
	}()

	time.Sleep(20 * time.Millisecond) // let wait() actually enter the syscall first
	if _, err := unix.Write(b, []byte{0x1}); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("wait() returned false after the fd became readable")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait() did not wake up within 2s of the fd becoming readable")
	}
}

// Reviewer feedback: "timeout" case, explicitly (distinct from the
// no-data test above, which also checks blocking duration -- this one
// only checks the return value).
func TestWaitKqueueTimesOut(t *testing.T) {
	a, _ := newTestSocketpair(t)
	kq := newWaitKqueue(a)
	if kq == nil {
		t.Fatal("newWaitKqueue returned nil")
	}
	defer kq.close()

	if !kq.wait(50 * time.Millisecond) {
		t.Fatal("wait() returned false on a plain timeout with no error condition")
	}
}

// Reviewer feedback: "Close() wakes a blocked wait" case, and the
// no-double-close/no-fd-reuse concern (P1) -- close() while wait() is
// parked in its syscall must not panic, must not leave wait() hung, and a
// second close() call (from a caller that, say, calls Close() twice on
// the same DarwinTun) must be safe.
func TestWaitKqueueCloseDuringWaitIsSafe(t *testing.T) {
	a, _ := newTestSocketpair(t)
	kq := newWaitKqueue(a)
	if kq == nil {
		t.Fatal("newWaitKqueue returned nil")
	}

	started := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		close(started)
		done <- kq.wait(5 * time.Second)
	}()

	<-started
	time.Sleep(20 * time.Millisecond) // let wait() actually enter the syscall first
	kq.close()
	kq.close() // double-close must be idempotent, not panic or double-free the fd

	select {
	case <-done:
		// Either true (the close-of-the-underlying-fd unblocked kevent, a
		// real kqueue behavior) or false (wait() observed the closed flag
		// first) is acceptable -- what matters is that it returned at all,
		// promptly, without hanging or crashing.
	case <-time.After(2 * time.Second):
		t.Fatal("wait() did not return within 2s of close() being called")
	}

	// A wait() call *after* close() must return false immediately (the
	// closed-flag fast path), not attempt a syscall against the
	// already-closed (and potentially since-reused, on a real system) fd
	// number.
	if kq.wait(time.Second) {
		t.Fatal("wait() returned true after close() -- should short-circuit via the closed flag")
	}
}

// Reviewer feedback: "multiple/concurrent close guard" case -- many
// goroutines calling close() concurrently must close the underlying fd
// exactly once.
func TestWaitKqueueConcurrentCloseIsSafe(t *testing.T) {
	a, _ := newTestSocketpair(t)
	kq := newWaitKqueue(a)
	if kq == nil {
		t.Fatal("newWaitKqueue returned nil")
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kq.close()
		}()
	}
	wg.Wait()

	if !kq.closed.Load() {
		t.Fatal("closed flag not set after concurrent close() calls")
	}
}

// Reviewer feedback: "kevent runtime failure without spinning" case (P2).
// Simulates a kqueue that has gone bad (closed out from under it, as if a
// concurrent/erroneous close happened) and confirms wait() reports it as
// unusable (false) rather than silently returning true forever, which is
// what DarwinTun.Wait relies on to permanently fall back to the sleep
// path instead of re-entering a failing syscall on every dispatchLoop
// iteration.
func TestWaitKqueueReportsPersistentFailure(t *testing.T) {
	a, _ := newTestSocketpair(t)
	kq := newWaitKqueue(a)
	if kq == nil {
		t.Fatal("newWaitKqueue returned nil")
	}
	// Close the underlying kqueue fd directly (bypassing kq.close(), which
	// would also set the closed flag) to simulate the fd going bad for a
	// reason other than this type's own close() -- e.g. some other code
	// path in the process closing it, or the kernel invalidating it.
	_ = unix.Close(kq.fd)

	for i := 0; i < 5; i++ {
		if kq.wait(50 * time.Millisecond) {
			t.Fatalf("wait() call %d returned true against a closed underlying fd -- should report failure, not spin", i)
		}
	}
}

func TestDarwinDirectEchoBindingUsesFamilySpecificOptions(t *testing.T) {
	for _, test := range []struct {
		family    echoFamily
		wantLevel int
		wantName  int
	}{
		{family: echoIPv4, wantLevel: unix.IPPROTO_IP, wantName: unix.IP_BOUND_IF},
		{family: echoIPv6, wantLevel: unix.IPPROTO_IPV6, wantName: unix.IPV6_BOUND_IF},
	} {
		var gotLevel, gotName, gotIndex int
		err := bindDarwinDirectEchoSocket(test.family, 42, carrierInterface{Name: "en0", Index: 7}, func(_ int, level, name, index int) error {
			gotLevel, gotName, gotIndex = level, name, index
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotLevel != test.wantLevel || gotName != test.wantName || gotIndex != 7 {
			t.Fatalf("family %d binding = level %d name %d index %d", test.family, gotLevel, gotName, gotIndex)
		}
	}
}

func TestSetInterfaceBindsIPv6SocketWithoutIPv4Option(t *testing.T) {
	iface, err := net.InterfaceByName("lo0")
	if err != nil {
		t.Fatal(err)
	}

	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	if err := setinterface("udp6", "[::]:0", uintptr(fd), iface); err != nil {
		t.Fatal(err)
	}
}

func TestBuildDarwinSystemRoutesAppliesExclusionsAfterProtectedDefaultExpansion(t *testing.T) {
	routes, err := buildDarwinSystemRoutes([]string{"0.0.0.0/0", "::/0"}, []string{"100.64.0.0/10", "2001:db8::/32"})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) == 0 {
		t.Fatal("protected default expansion returned no routes")
	}
	for _, route := range routes {
		if strings.HasPrefix(route.String(), "0.0.0.0/0") || strings.HasPrefix(route.String(), "::/0") {
			t.Fatalf("unprotected default route returned: %s", route)
		}
		for _, excluded := range []string{"100.64.0.0/10", "2001:db8::/32"} {
			prefix := mustPrefix(t, excluded)
			if prefixesOverlap(route, prefix) {
				t.Fatalf("route %s overlaps exclusion %s", route, prefix)
			}
		}
	}
}

func TestBuildDarwinSystemRoutesEmptyExclusionsPreserveNonDefaultRoutes(t *testing.T) {
	routes, err := buildDarwinSystemRoutes([]string{"10.0.0.1/8", "2001:db8::/32"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0].String() != "10.0.0.0/8" || routes[1].String() != "2001:db8::/32" {
		t.Fatalf("routes = %v", routes)
	}
}

func TestSelectDarwinDefaultRouteSkipsUnusableEarlierRoute(t *testing.T) {
	messages := []route.Message{
		darwinDefaultRouteMessage(carrierIPv4, 10),
		darwinDefaultRouteMessage(carrierIPv4, 20),
	}
	lookup := func(index int) (*net.Interface, error) {
		if index == 10 {
			return &net.Interface{Index: index, Name: "down0"}, nil
		}
		return &net.Interface{Index: index, Name: "en0", Flags: net.FlagUp}, nil
	}

	iface, err := selectDarwinDefaultRoute(messages, carrierIPv4, 99, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if iface.Index != 20 {
		t.Fatalf("selected interface = %+v, want index 20", iface)
	}
}

func darwinDefaultRouteMessage(family carrierFamily, index int) *route.RouteMessage {
	message := &route.RouteMessage{Index: index, Flags: unix.RTF_UP | unix.RTF_GATEWAY}
	if family == carrierIPv4 {
		message.Addrs = []route.Addr{
			unix.RTAX_DST:     &route.Inet4Addr{},
			unix.RTAX_NETMASK: &route.Inet4Addr{},
		}
	} else {
		message.Addrs = []route.Addr{
			unix.RTAX_DST:     &route.Inet6Addr{},
			unix.RTAX_NETMASK: &route.Inet6Addr{},
		}
	}
	return message
}

func mustPrefix(t *testing.T, value string) netip.Prefix {
	t.Helper()
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		t.Fatal(err)
	}
	return prefix.Masked()
}

func TestSelectDarwinGatewayConfiguredIPv4(t *testing.T) {
	gateway, err := selectDarwinGateway([]string{"198.18.0.1/15"})
	if err != nil {
		t.Fatal(err)
	}
	if got := gateway.String(); got != "198.18.0.1/15" {
		t.Fatalf("unexpected gateway: got %s", got)
	}
}

func TestSelectDarwinGatewaySkipsIPv6(t *testing.T) {
	gateway, err := selectDarwinGateway([]string{"fc00::1/64", "198.18.0.1/15"})
	if err != nil {
		t.Fatal(err)
	}
	if got := gateway.String(); got != "198.18.0.1/15" {
		t.Fatalf("unexpected gateway: got %s", got)
	}
}

func TestSelectDarwinGatewayRequiresIPv4(t *testing.T) {
	if _, err := selectDarwinGateway([]string{"fc00::1/64"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestSelectDarwinGatewayRequiresUsableLocalAddress(t *testing.T) {
	if _, err := selectDarwinGateway([]string{"198.18.0.1/32"}); err == nil {
		t.Fatal("expected error")
	}
}

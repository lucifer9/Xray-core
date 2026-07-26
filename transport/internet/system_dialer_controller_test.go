package internet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
)

func TestRequiredDialerControllerLifecycle(t *testing.T) {
	server := listenTCP(t)
	controlErr := errors.New("required control rejected connection")

	registration, err := RegisterRequiredDialerController(func(string, string, syscall.RawConn) error {
		return controlErr
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registration.Close() })

	if _, err := DialSystem(context.Background(), server.destination, nil); err == nil || !strings.Contains(err.Error(), controlErr.Error()) {
		t.Fatalf("DialSystem() error = %v, want required control error", err)
	}

	if err := registration.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registration.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want nil", err)
	}

	conn, err := DialSystem(context.Background(), server.destination, nil)
	if err != nil {
		t.Fatalf("DialSystem() after Close() error = %v", err)
	}
	_ = conn.Close()
}

func TestRequiredDialerControllerRejectsUDP(t *testing.T) {
	controlErr := errors.New("required control rejected packet connection")
	registration, err := RegisterRequiredDialerController(func(_ string, address string, _ syscall.RawConn) error {
		if address != "127.0.0.1:53" {
			return nil
		}
		return controlErr
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registration.Close() })

	_, err = DialSystem(context.Background(), net.UDPDestination(net.LocalHostIP, 53), nil)
	if err == nil || !strings.Contains(err.Error(), controlErr.Error()) {
		t.Fatalf("DialSystem() error = %v, want required control error", err)
	}
}

func TestLegacyDialerControllerRemainsBestEffort(t *testing.T) {
	resetLegacyDialerControllers(t)
	var calls atomic.Int32
	if err := RegisterDialerController(func(string, string, syscall.RawConn) error {
		calls.Add(1)
		return errors.New("legacy control failed")
	}); err != nil {
		t.Fatal(err)
	}

	server := listenTCP(t)
	conn, err := DialSystem(context.Background(), server.destination, nil)
	if err != nil {
		t.Fatalf("TCP DialSystem() error = %v, want success", err)
	}
	_ = conn.Close()

	packetConn, err := DialSystem(context.Background(), net.UDPDestination(net.LocalHostIP, 53), nil)
	if err != nil {
		t.Fatalf("UDP DialSystem() error = %v, want success", err)
	}
	_ = packetConn.Close()

	if calls.Load() < 2 {
		t.Fatalf("legacy controller calls = %d, want at least 2", calls.Load())
	}
}

func TestDialUsesStableControllerSnapshot(t *testing.T) {
	server := listenTCP(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	first, err := RegisterRequiredDialerController(func(string, string, syscall.RawConn) error {
		close(entered)
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	dialResult := make(chan error, 1)
	go func() {
		conn, err := DialSystem(context.Background(), server.destination, nil)
		if conn != nil {
			_ = conn.Close()
		}
		dialResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("controller was not called")
	}

	laterErr := errors.New("later required control")
	later, err := RegisterRequiredDialerController(func(string, string, syscall.RawConn) error {
		return laterErr
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = later.Close() })
	close(release)

	select {
	case err := <-dialResult:
		if err != nil {
			t.Fatalf("in-flight DialSystem() error = %v, want stable pre-registration snapshot", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight DialSystem() did not return")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := DialSystem(context.Background(), server.destination, nil); err == nil || !strings.Contains(err.Error(), laterErr.Error()) {
		t.Fatalf("later DialSystem() error = %v, want later controller error", err)
	}
}

func TestRequiredDialerControllerRejectsCustomDialerWithoutRegistration(t *testing.T) {
	UseAlternativeSystemDialer(WithAdapter(failingDialerAdapter{}))
	t.Cleanup(func() { UseAlternativeSystemDialer(nil) })

	var calls atomic.Int32
	registration, err := RegisterRequiredDialerController(func(string, string, syscall.RawConn) error {
		calls.Add(1)
		return nil
	})
	if err == nil {
		if registration != nil {
			_ = registration.Close()
		}
		t.Fatal("RegisterRequiredDialerController() error = nil, want custom dialer rejection")
	}

	UseAlternativeSystemDialer(nil)
	server := listenTCP(t)
	conn, err := DialSystem(context.Background(), server.destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if calls.Load() != 0 {
		t.Fatalf("rejected controller calls = %d, want 0", calls.Load())
	}
}

func TestDialerControllerConcurrentLifecycleAndDial(t *testing.T) {
	server := listenTCP(t)
	const iterations = 50
	var wait sync.WaitGroup
	errs := make(chan error, iterations*2)

	wait.Add(2)
	go func() {
		defer wait.Done()
		for range iterations {
			registration, err := RegisterRequiredDialerController(func(string, string, syscall.RawConn) error {
				return nil
			})
			if err != nil {
				errs <- err
				continue
			}
			if err := registration.Close(); err != nil {
				errs <- err
			}
		}
	}()
	go func() {
		defer wait.Done()
		for range iterations {
			conn, err := DialSystem(context.Background(), server.destination, nil)
			if err != nil {
				errs <- err
				continue
			}
			_ = conn.Close()
		}
	}()

	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

type failingDialerAdapter struct{}

func (failingDialerAdapter) Dial(string, string) (net.Conn, error) {
	return nil, errors.New("custom dialer called")
}

type tcpTestServer struct {
	destination net.Destination
}

func listenTCP(t *testing.T) tcpTestServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	return tcpTestServer{
		destination: net.TCPDestination(net.IPAddress(addr.IP), net.Port(addr.Port)),
	}
}

func resetLegacyDialerControllers(t *testing.T) {
	t.Helper()
	ControllersLock.Lock()
	previous := Controllers
	Controllers = nil
	ControllersLock.Unlock()
	t.Cleanup(func() {
		ControllersLock.Lock()
		Controllers = previous
		ControllersLock.Unlock()
	})
}

package localdns

import (
	"context"
	"errors"
	stdnet "net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/transport/internet"
)

func TestLookupIPReturnsRequiredDialerControllerError(t *testing.T) {
	client := newTestClient(t)
	controlErr := errors.New("required local DNS control failed")
	registration, err := internet.RegisterRequiredDialerController(func(string, string, syscall.RawConn) error {
		return controlErr
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registration.Close() })

	_, _, err = client.LookupIP("required-control.invalid", dns.IPOption{IPv4Enable: true, IPv6Enable: true})
	if err == nil || !strings.Contains(err.Error(), controlErr.Error()) {
		t.Fatalf("LookupIP() error = %v, want required control error", err)
	}
}

func TestLookupIPKeepsLegacyDialerControllerBestEffort(t *testing.T) {
	resetLegacyDialerControllers(t)
	client := newTestClient(t)
	var calls atomic.Int32
	if err := internet.RegisterDialerController(func(string, string, syscall.RawConn) error {
		calls.Add(1)
		return errors.New("legacy local DNS control failed")
	}); err != nil {
		t.Fatal(err)
	}

	ips, _, err := client.LookupIP("legacy-control.test", dns.IPOption{IPv4Enable: true})
	if err != nil {
		t.Fatalf("LookupIP() error = %v, want legacy control failure ignored", err)
	}
	if len(ips) != 1 || ips[0].String() != "127.0.0.42" {
		t.Fatalf("LookupIP() = %v, want [127.0.0.42]", ips)
	}
	if calls.Load() == 0 {
		t.Fatal("legacy dialer controller was not called")
	}
}

func resetLegacyDialerControllers(t *testing.T) {
	t.Helper()
	internet.ControllersLock.Lock()
	previous := internet.Controllers
	internet.Controllers = nil
	internet.ControllersLock.Unlock()
	t.Cleanup(func() {
		internet.ControllersLock.Lock()
		internet.Controllers = previous
		internet.ControllersLock.Unlock()
	})
}

func TestLookupIPDuringRequiredControllerLifecycle(t *testing.T) {
	client := newTestClient(t)
	baseline, err := internet.RegisterRequiredDialerController(func(string, string, syscall.RawConn) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = baseline.Close() })

	const iterations = 25
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		for range iterations {
			registration, err := internet.RegisterRequiredDialerController(func(string, string, syscall.RawConn) error {
				return nil
			})
			if err != nil {
				t.Error(err)
				return
			}
			if err := registration.Close(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wait.Done()
		for range iterations {
			_, _, _ = client.LookupIP("localhost", dns.IPOption{IPv4Enable: true, IPv6Enable: true})
		}
	}()
	wait.Wait()
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	server, err := stdnet.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	go serveDNS(t, server)

	client := New()
	dial := client.r.Dial
	client.r.Dial = func(ctx context.Context, network, _ string) (stdnet.Conn, error) {
		return dial(ctx, network, server.LocalAddr().String())
	}
	return client
}

func serveDNS(t *testing.T, server stdnet.PacketConn) {
	t.Helper()
	buffer := make([]byte, 1500)
	for {
		n, address, err := server.ReadFrom(buffer)
		if err != nil {
			return
		}
		response, err := dnsAResponse(buffer[:n])
		if err != nil {
			t.Error(err)
			continue
		}
		if _, err := server.WriteTo(response, address); err != nil {
			t.Error(err)
			return
		}
	}
}

func dnsAResponse(query []byte) ([]byte, error) {
	if len(query) < 12 {
		return nil, errors.New("short DNS query")
	}
	questionEnd := 12
	for {
		if questionEnd >= len(query) {
			return nil, errors.New("invalid DNS question")
		}
		labelLength := int(query[questionEnd])
		questionEnd++
		if labelLength == 0 {
			break
		}
		questionEnd += labelLength
	}
	questionEnd += 4
	if questionEnd > len(query) {
		return nil, errors.New("short DNS question")
	}

	response := append([]byte(nil), query[:questionEnd]...)
	response[2] |= 0x80
	response[3] |= 0x80
	response[6], response[7] = 0, 1
	response[8], response[9] = 0, 0
	response[10], response[11] = 0, 0
	response = append(response,
		0xc0, 0x0c,
		0x00, 0x01,
		0x00, 0x01,
		0x00, 0x00, 0x00, 0x3c,
		0x00, 0x04,
		127, 0, 0, 42,
	)
	return response, nil
}

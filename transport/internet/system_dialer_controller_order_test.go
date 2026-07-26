//go:build darwin || linux || windows

package internet

import (
	"context"
	"strconv"
	"syscall"
	"testing"

	"github.com/xtls/xray-core/common/net"
)

func TestRequiredDialerControllerRunsAfterSocketOptions(t *testing.T) {
	server := listenTCP(t)
	tests := []struct {
		name        string
		destination net.Destination
		option      int
	}{
		{name: "TCP", destination: server.destination, option: syscall.SO_REUSEADDR},
		{name: "UDP", destination: net.UDPDestination(net.LocalHostIP, 53), option: syscall.SO_REUSEADDR},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registration, err := RegisterRequiredDialerController(func(_ string, _ string, c syscall.RawConn) error {
				return setTestSocketOptionInt(c, syscall.SOL_SOCKET, test.option, 1)
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = registration.Close() })

			conn, err := DialSystem(context.Background(), test.destination, &SocketConfig{
				CustomSockopt: []*CustomSockopt{{
					Level: strconv.Itoa(syscall.SOL_SOCKET),
					Opt:   strconv.Itoa(test.option),
					Value: "0",
					Type:  "int",
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })

			value, err := getTestSocketOptionInt(connectionRawConn(t, conn), syscall.SOL_SOCKET, test.option)
			if err != nil {
				t.Fatal(err)
			}
			if value == 0 {
				t.Fatal("required controller socket option was overwritten")
			}
		})
	}
}

func connectionRawConn(t *testing.T, conn net.Conn) syscall.RawConn {
	t.Helper()
	var source any = conn
	if packetConn, ok := conn.(*PacketConnWrapper); ok {
		source = packetConn.PacketConn
	}
	syscallConn, ok := source.(syscall.Conn)
	if !ok {
		t.Fatalf("connection type %T does not expose syscall.RawConn", source)
	}
	raw, err := syscallConn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

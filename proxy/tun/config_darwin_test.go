//go:build darwin

package tun

import (
	"syscall"
	"testing"

	"golang.org/x/net/route"
)

func TestDefaultRouteInterfaceIndexPrefersIPv4Default(t *testing.T) {
	messages := []route.Message{
		&route.RouteMessage{
			Flags: syscall.RTF_UP | syscall.RTF_GATEWAY,
			Index: 17,
			Addrs: []route.Addr{
				&route.Inet6Addr{},
			},
		},
		&route.RouteMessage{
			Flags: syscall.RTF_UP | syscall.RTF_GATEWAY,
			Index: 15,
			Addrs: []route.Addr{
				&route.Inet4Addr{},
			},
		},
	}

	if got := defaultRouteInterfaceIndex(messages, 0); got != 15 {
		t.Fatalf("defaultRouteInterfaceIndex() = %d, want IPv4 default interface 15", got)
	}
}

func TestDefaultRouteInterfaceIndexSkipsTunInterface(t *testing.T) {
	messages := []route.Message{
		&route.RouteMessage{
			Flags: syscall.RTF_UP | syscall.RTF_GATEWAY,
			Index: 21,
			Addrs: []route.Addr{
				&route.Inet4Addr{},
			},
		},
		&route.RouteMessage{
			Flags: syscall.RTF_UP | syscall.RTF_GATEWAY,
			Index: 15,
			Addrs: []route.Addr{
				&route.Inet4Addr{},
			},
		},
	}

	if got := defaultRouteInterfaceIndex(messages, 21); got != 15 {
		t.Fatalf("defaultRouteInterfaceIndex() = %d, want non-tun interface 15", got)
	}
}

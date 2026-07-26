package tun

import (
	"net/netip"
	"time"
)

// Stack interface implement ip protocol stack, bridging raw network packets and data streams
type Stack interface {
	Start() error
	Close() error
}

// StackOptions for the stack implementation
type StackOptions struct {
	Tun         Tun
	IdleTimeout time.Duration
	EchoProber  echoProber
	// Gateway4/Gateway6 are the TUN interface addresses used as the source of
	// ICMP error messages the stack reports to clients (e.g. network
	// unreachable when an Echo probe family has no Outbound carrier).
	Gateway4 netip.Addr
	Gateway6 netip.Addr
}

package tun

import "net/netip"

type echoFamily uint8

const (
	echoIPv4 echoFamily = 4
	echoIPv6 echoFamily = 6
)

type echoRequest struct {
	Family      echoFamily
	Source      netip.Addr
	Destination netip.Addr
	Identifier  uint16
	Sequence    uint16
	Payload     []byte
}

type echoReply struct {
	Identifier uint16
	Sequence   uint16
	Payload    []byte
	TTL        uint8
}

type echoResult struct {
	Reply *echoReply
	Err   error
}

type echoProber interface {
	Probe(echoRequest, func(echoResult)) error
	Close() error
}

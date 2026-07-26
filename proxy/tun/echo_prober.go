package tun

import (
	"net/netip"
	"sync"
)

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

type localEchoProber struct {
	mu     sync.Mutex
	closed bool
}

func newLocalEchoProber() echoProber {
	return &localEchoProber{}
}

func (p *localEchoProber) Probe(request echoRequest, complete func(echoResult)) error {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return errEchoProberClosed
	}
	complete(echoResult{Reply: &echoReply{
		Identifier: request.Identifier,
		Sequence:   request.Sequence,
		Payload:    append([]byte(nil), request.Payload...),
		TTL:        64,
	}})
	return nil
}

func (p *localEchoProber) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

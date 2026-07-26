package tun

import (
	"bytes"
	"encoding/binary"
	stderrors "errors"
	"net/netip"

	"github.com/xtls/xray-core/common/errors"
)

// Maximum size of a Destination Unreachable error message: enough original
// packet for the sender to match the error, bounded by the minimum MTU each
// IP version must accept.
const (
	maxIPv4UnreachableSize = ipv4SmallestMTU - ipv4HeaderSize
	maxIPv6UnreachableSize = ipv6SmallestMTU - ipv6HeaderSize
)

// probeEcho hands the echo request p to the prober and answers it with the
// reply the prober reports, if any. Without an Outbound carrier for the IP
// version, the sender is told that the network is unreachable, as a router
// without a route would.
func (s *stack) probeEcho(p packet) {
	m := p.payload
	family := echoIPv6
	if p.proto == protoICMPv4 {
		family = echoIPv4
	}
	request := echoRequest{
		Family:      family,
		Source:      p.src,
		Destination: p.dst,
		Identifier:  binary.BigEndian.Uint16(m[4:]),
		Sequence:    binary.BigEndian.Uint16(m[6:]),
		// the packet is read into a buffer that is used again, the prober may answer later
		Payload: bytes.Clone(m[8:]),
	}
	proto, src, dst := p.proto, p.src, p.dst
	err := s.echo.Probe(request, func(result echoResult) {
		if result.Reply == nil || !s.beginEcho() {
			return
		}
		defer s.echoes.Done()
		errors.LogDebug(s.ctx, "[tun][icmp] echo reply ", dst, " -> ", src, " id=", request.Identifier, " seq=", request.Sequence)
		if err := s.writeEchoReply(proto, dst, src, result.Reply); err != nil {
			errors.LogInfoInner(s.ctx, err, "[tun] failed to write ICMP Echo reply")
		}
	})
	switch {
	case stderrors.Is(err, errEchoCarrierUnknown):
		s.writeNetworkUnreachable(p)
	case err != nil:
		errors.LogInfoInner(s.ctx, err, "[tun] failed to submit ICMP Echo probe")
	}
}

// beginEcho reports whether an answer of echo may still be written, which
// Close then waits for until echoes.Done
func (s *stack) beginEcho() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.echoes.Add(1)
	return true
}

// writeEchoReply writes reply as the answer from src to an echo request that dst sent
func (s *stack) writeEchoReply(proto byte, src, dst netip.Addr, reply *echoReply) error {
	var b [ipv6HeaderSize]byte
	n := putIPHeader(b[:], proto, src, dst)
	setIPHopLimit(b[:n], reply.TTL)
	m := make([]byte, 8+len(reply.Payload))
	if proto == protoICMPv6 {
		m[0] = 129
	}
	binary.BigEndian.PutUint16(m[4:], reply.Identifier)
	binary.BigEndian.PutUint16(m[6:], reply.Sequence)
	copy(m[8:], reply.Payload)
	binary.BigEndian.PutUint16(m[2:], icmpChecksum(proto, b[:n], m))
	return s.write(b[:n], m)
}

// writeNetworkUnreachable tells the sender of the echo request p that its
// network is unreachable. The error comes from the TUN gateway of the IP
// version, or from the destination itself without one.
func (s *stack) writeNetworkUnreachable(p packet) {
	if !s.beginEcho() {
		return
	}
	defer s.echoes.Done()

	reporter, limit, kind := s.gateway6, maxIPv6UnreachableSize, byte(1)
	if p.proto == protoICMPv4 {
		reporter, limit, kind = s.gateway4, maxIPv4UnreachableSize, 3
	}
	if !reporter.IsValid() {
		reporter = p.dst
	}
	var b [ipv6HeaderSize]byte
	n := putIPHeader(b[:], p.proto, reporter, p.src)

	// the message quotes the request as it came, with a header of its own
	var quoted [ipv6HeaderSize]byte
	q := putIPHeader(quoted[:], p.proto, p.src, p.dst)
	setIPLength(quoted[:q], q+len(p.payload))
	m := make([]byte, 0, limit)
	m = append(m, kind, 0, 0, 0, 0, 0, 0, 0)
	m = append(m, quoted[:q]...)
	m = append(m, p.payload[:min(len(p.payload), limit-len(m))]...)
	binary.BigEndian.PutUint16(m[2:], icmpChecksum(p.proto, b[:n], m))

	errors.LogDebug(s.ctx, "[tun][icmp] network unreachable ", reporter, " -> ", p.src)
	if err := s.write(b[:n], m); err != nil {
		errors.LogInfoInner(s.ctx, err, "[tun] failed to write ICMP Destination Unreachable")
	}
}

// setIPHopLimit sets the TTL or hop limit of the IP header ip
func setIPHopLimit(ip []byte, hops uint8) {
	if len(ip) == ipv4HeaderSize {
		ip[8] = hops
		return
	}
	ip[7] = hops
}

// icmpChecksum is the checksum of the ICMP message m, whose checksum field is
// zero, of a packet with the header ip. ICMPv6 covers a pseudo-header as well
func icmpChecksum(proto byte, ip, m []byte) uint16 {
	if proto == protoICMPv6 {
		return transportChecksum(ip, protoICMPv6, m, nil)
	}
	return checksum(sum(m, 0))
}

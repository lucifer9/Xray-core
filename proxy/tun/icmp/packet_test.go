package icmp

import (
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func TestParseEchoRequest(t *testing.T) {
	t.Run("ipv4 echo", func(t *testing.T) {
		var zero tcpip.Address
		packet := []byte{
			byte(header.ICMPv4Echo), 0,
			0, 0,
			0x12, 0x34,
			0x56, 0x78,
			0xaa, 0xbb,
		}
		if err := RewriteChecksum(header.IPv4ProtocolNumber, packet, zero, zero); err != nil {
			t.Fatal(err)
		}

		ident, sequence, ok := ParseEchoRequest(header.IPv4ProtocolNumber, packet)
		if !ok {
			t.Fatal("expected ipv4 echo request to parse")
		}
		if ident != 0x1234 || sequence != 0x5678 {
			t.Fatalf("unexpected ident/sequence: %x/%x", ident, sequence)
		}
	})

	t.Run("ipv6 echo", func(t *testing.T) {
		packet := []byte{
			byte(header.ICMPv6EchoRequest), 0,
			0, 0,
			0xab, 0xcd,
			0xef, 0x01,
			0xaa, 0xbb,
		}
		src := tcpip.AddrFromSlice([]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
		dst := tcpip.AddrFromSlice([]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2})
		if err := RewriteChecksum(header.IPv6ProtocolNumber, packet, src, dst); err != nil {
			t.Fatal(err)
		}

		ident, sequence, ok := ParseEchoRequest(header.IPv6ProtocolNumber, packet)
		if !ok {
			t.Fatal("expected ipv6 echo request to parse")
		}
		if ident != 0xabcd || sequence != 0xef01 {
			t.Fatalf("unexpected ident/sequence: %x/%x", ident, sequence)
		}
	})
}

func TestRewriteChecksum(t *testing.T) {
	t.Run("ipv4", func(t *testing.T) {
		var zero tcpip.Address
		packet := []byte{
			byte(header.ICMPv4Echo), 0,
			0xff, 0xff,
			0x12, 0x34,
			0x56, 0x78,
			0xaa, 0xbb, 0xcc,
		}
		if err := RewriteChecksum(header.IPv4ProtocolNumber, packet, zero, zero); err != nil {
			t.Fatal(err)
		}

		icmpHdr := header.ICMPv4(packet)
		if got, want := icmpHdr.Checksum(), header.ICMPv4Checksum(icmpHdr[:header.ICMPv4MinimumSize], checksumPayloadV4(icmpHdr.Payload())); got != want {
			t.Fatalf("unexpected ipv4 checksum: got %x want %x", got, want)
		}
	})

	t.Run("ipv6", func(t *testing.T) {
		packet := []byte{
			byte(header.ICMPv6EchoReply), 0,
			0xff, 0xff,
			0x12, 0x34,
			0x56, 0x78,
			0xaa, 0xbb, 0xcc,
		}
		src := tcpip.AddrFromSlice([]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
		dst := tcpip.AddrFromSlice([]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2})
		if err := RewriteChecksum(header.IPv6ProtocolNumber, packet, src, dst); err != nil {
			t.Fatal(err)
		}

		icmpHdr := header.ICMPv6(packet)
		want := header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
			Header:      icmpHdr[:header.ICMPv6MinimumSize],
			Src:         src,
			Dst:         dst,
			PayloadLen:  len(icmpHdr.Payload()),
			PayloadCsum: checksumPayloadV6(icmpHdr.Payload()),
		})
		if got := icmpHdr.Checksum(); got != want {
			t.Fatalf("unexpected ipv6 checksum: got %x want %x", got, want)
		}
	})
}

func TestBuildLocalEchoReply(t *testing.T) {
	t.Run("ipv4", func(t *testing.T) {
		request := []byte{
			byte(header.ICMPv4Echo), 0,
			0, 0,
			0x12, 0x34,
			0x56, 0x78,
			0xaa, 0xbb, 0xcc,
		}
		src := tcpip.Address{}
		dst := tcpip.Address{}
		if err := RewriteChecksum(header.IPv4ProtocolNumber, request, src, dst); err != nil {
			t.Fatal(err)
		}

		reply, err := BuildLocalEchoReply(header.IPv4ProtocolNumber, request, dst, src)
		if err != nil {
			t.Fatal(err)
		}
		if request[0] != byte(header.ICMPv4Echo) {
			t.Fatal("request mutated")
		}
		icmpHdr := header.ICMPv4(reply)
		if icmpHdr.Type() != header.ICMPv4EchoReply || icmpHdr.Code() != header.ICMPv4UnusedCode {
			t.Fatalf("unexpected ipv4 reply type/code: %d/%d", icmpHdr.Type(), icmpHdr.Code())
		}
		if icmpHdr.Ident() != 0x1234 || icmpHdr.Sequence() != 0x5678 {
			t.Fatalf("unexpected ipv4 ident/sequence: %x/%x", icmpHdr.Ident(), icmpHdr.Sequence())
		}
	})

	t.Run("ipv6", func(t *testing.T) {
		request := []byte{
			byte(header.ICMPv6EchoRequest), 0,
			0, 0,
			0xab, 0xcd,
			0xef, 0x01,
			0xaa, 0xbb, 0xcc,
		}
		src := tcpip.AddrFromSlice([]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
		dst := tcpip.AddrFromSlice([]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2})
		if err := RewriteChecksum(header.IPv6ProtocolNumber, request, src, dst); err != nil {
			t.Fatal(err)
		}

		reply, err := BuildLocalEchoReply(header.IPv6ProtocolNumber, request, dst, src)
		if err != nil {
			t.Fatal(err)
		}
		if request[0] != byte(header.ICMPv6EchoRequest) {
			t.Fatal("request mutated")
		}
		icmpHdr := header.ICMPv6(reply)
		if icmpHdr.Type() != header.ICMPv6EchoReply || icmpHdr.Code() != header.ICMPv6UnusedCode {
			t.Fatalf("unexpected ipv6 reply type/code: %d/%d", icmpHdr.Type(), icmpHdr.Code())
		}
		if icmpHdr.Ident() != 0xabcd || icmpHdr.Sequence() != 0xef01 {
			t.Fatalf("unexpected ipv6 ident/sequence: %x/%x", icmpHdr.Ident(), icmpHdr.Sequence())
		}
	})
}

func checksumPayloadV4(payload []byte) uint16 {
	return checksum.Checksum(payload, 0)
}

func checksumPayloadV6(payload []byte) uint16 {
	return checksum.Checksum(payload, 0)
}

func TestBuildDestinationUnreachable(t *testing.T) {
	client := tcpip.AddrFrom4([4]byte{10, 0, 0, 2})
	target := tcpip.AddrFrom4([4]byte{198, 51, 100, 1})
	gateway := tcpip.AddrFrom4([4]byte{172, 18, 0, 1})

	t.Run("ipv4", func(t *testing.T) {
		echo := []byte{byte(header.ICMPv4Echo), 0, 0, 0, 0x12, 0x34, 0x56, 0x78, 0xaa}
		message, err := BuildDestinationUnreachable(header.IPv4ProtocolNumber, echo, client, target, gateway, client)
		if err != nil {
			t.Fatal(err)
		}
		icmpHeader := header.ICMPv4(message)
		if icmpHeader.Type() != header.ICMPv4DstUnreachable || icmpHeader.Code() != header.ICMPv4NetUnreachable {
			t.Fatalf("type/code = %v/%v", icmpHeader.Type(), icmpHeader.Code())
		}
		if icmpHeader.Checksum() == 0 {
			t.Fatal("checksum was not computed")
		}
		quote := icmpHeader.Payload()
		ipHeader := header.IPv4(quote)
		if !ipHeader.IsValid(len(quote)) || ipHeader.SourceAddress() != client || ipHeader.DestinationAddress() != target || ipHeader.TotalLength() != uint16(header.IPv4MinimumSize+len(echo)) {
			t.Fatalf("quoted IPv4 header = %x", quote)
		}
		if string(quote[header.IPv4MinimumSize:]) != string(echo) {
			t.Fatalf("quoted message = %x, want %x", quote[header.IPv4MinimumSize:], echo)
		}
	})

	t.Run("ipv6", func(t *testing.T) {
		client6 := tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 2})
		target6 := tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8, 15: 1})
		gateway6 := tcpip.AddrFrom16([16]byte{0: 0xfd, 1: 0xfe, 15: 1})
		echo := []byte{byte(header.ICMPv6EchoRequest), 0, 0, 0, 0x12, 0x34, 0x56, 0x78, 0xaa}
		message, err := BuildDestinationUnreachable(header.IPv6ProtocolNumber, echo, client6, target6, gateway6, client6)
		if err != nil {
			t.Fatal(err)
		}
		icmpHeader := header.ICMPv6(message)
		if icmpHeader.Type() != header.ICMPv6DstUnreachable || icmpHeader.Code() != header.ICMPv6NetworkUnreachable {
			t.Fatalf("type/code = %v/%v", icmpHeader.Type(), icmpHeader.Code())
		}
		wantChecksum := header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
			Header:      icmpHeader[:header.ICMPv6MinimumSize],
			Src:         gateway6,
			Dst:         client6,
			PayloadCsum: checksum.Checksum(icmpHeader.Payload(), 0),
			PayloadLen:  len(icmpHeader.Payload()),
		})
		if icmpHeader.Checksum() != wantChecksum {
			t.Fatalf("checksum = %x, want %x", icmpHeader.Checksum(), wantChecksum)
		}
		quote := icmpHeader.Payload()
		ipHeader := header.IPv6(quote)
		if ipHeader.SourceAddress() != client6 || ipHeader.DestinationAddress() != target6 || ipHeader.PayloadLength() != uint16(len(echo)) {
			t.Fatalf("quoted IPv6 header = %x", quote)
		}
	})

	t.Run("quote truncation", func(t *testing.T) {
		big := make([]byte, 65535-header.IPv4MinimumSize)
		message, err := BuildDestinationUnreachable(header.IPv4ProtocolNumber, big, client, target, gateway, client)
		if err != nil {
			t.Fatal(err)
		}
		if len(message) > 576-header.IPv4MinimumSize {
			t.Fatalf("message length = %d, exceeds minimum reassembly buffer", len(message))
		}
		quoted := header.IPv4(header.ICMPv4(message).Payload())
		if quoted.TotalLength() != 65535 {
			t.Fatalf("quoted TotalLength = %d, want original 65535", quoted.TotalLength())
		}
	})
}

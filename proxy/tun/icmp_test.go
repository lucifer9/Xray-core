//go:build !windows

package tun

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"testing"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestICMPv4Checksum(t *testing.T) {
	// Build a minimal ICMPv4 Echo Request
	icmp := make([]byte, header.ICMPv4MinimumSize+4) // 4 bytes of data
	icmp[0] = byte(header.ICMPv4Echo)                 // Type: Echo Request
	icmp[1] = 0                                       // Code: 0
	// Checksum: 0 (to be calculated)
	binary.BigEndian.PutUint16(icmp[4:6], 1234) // Identifier
	binary.BigEndian.PutUint16(icmp[6:8], 1)    // Sequence
	copy(icmp[8:], []byte{0xDE, 0xAD, 0xBE, 0xEF})

	hdr := header.ICMPv4(icmp)
	checksum := header.ICMPv4Checksum(hdr, 0)
	hdr.SetChecksum(checksum)

	// Verify checksum is not zero
	if checksum == 0 {
		t.Fatal("ICMPv4 checksum should not be zero")
	}

	// Verify checksum validity: summing the entire ICMP message (as 16-bit words)
	// should yield 0 when the checksum field is included.
	var sum uint32
	for i := 0; i < len(icmp)-1; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(icmp[i : i+2]))
	}
	if len(icmp)%2 == 1 {
		sum += uint32(icmp[len(icmp)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xFFFF) + (sum >> 16)
	}
	if ^uint16(sum) != 0 {
		t.Fatalf("ICMPv4 checksum verification failed: residual = 0x%04x", ^uint16(sum))
	}
}

func TestICMPv6Checksum(t *testing.T) {
	src := tcpip.AddrFrom16([16]byte{0: 0xfd, 1: 0x00, 14: 0x00, 15: 0x02}) // fd00::2
	dst := tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8})   // 2001:db8::

	icmp := make([]byte, header.ICMPv6MinimumSize+4)
	icmp[0] = byte(header.ICMPv6EchoRequest) // Type: Echo Request
	icmp[1] = 0                              // Code: 0
	binary.BigEndian.PutUint16(icmp[4:6], 5678)
	binary.BigEndian.PutUint16(icmp[6:8], 1)
	copy(icmp[8:], []byte{0xCA, 0xFE, 0xBA, 0xBE})

	hdr := header.ICMPv6(icmp)
	checksum := header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
		Header: hdr,
		Src:    src,
		Dst:    dst,
	})
	hdr.SetChecksum(checksum)

	if checksum == 0 {
		t.Fatal("ICMPv6 checksum should not be zero")
	}

	// Verify checksum field was set correctly
	got := hdr.Checksum()
	if got != checksum {
		t.Fatalf("ICMPv6 checksum mismatch: got 0x%04x, want 0x%04x", got, checksum)
	}
}

func TestICMPIdentifierXOR(t *testing.T) {
	tests := []struct {
		name string
		id   uint16
		want uint16
	}{
		{"zero", 0, 0xFFFF},
		{"max", 0xFFFF, 0},
		{"arbitrary", 0x1234, 0x1234 ^ 0xFFFF},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xored := tt.id ^ icmpXORIdent
			if xored != tt.want {
				t.Fatalf("XOR(%d) = %d, want %d", tt.id, xored, tt.want)
			}

			// Roundtrip: XOR twice should return original
			restored := xored ^ icmpXORIdent
			if restored != tt.id {
				t.Fatalf("roundtrip XOR(%d) = %d, want %d", tt.id, restored, tt.id)
			}
		})
	}
}

func TestBuildPayload(t *testing.T) {
	f := &icmpForwarder{}

	transportData := []byte{byte(header.ICMPv4Echo), 0, 0, 0, 0, 1, 0, 2}
	payloadData := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	pkt := buildMockPacketBuffer(t, transportData, payloadData)
	payload := f.buildPayload(pkt)

	if len(payload) != len(transportData)+len(payloadData) {
		t.Fatalf("payload length = %d, want %d", len(payload), len(transportData)+len(payloadData))
	}

	for i, b := range transportData {
		if payload[i] != b {
			t.Fatalf("payload[%d] = 0x%02x, want 0x%02x", i, payload[i], b)
		}
	}
	for i, b := range payloadData {
		if payload[len(transportData)+i] != b {
			t.Fatalf("payload[%d] = 0x%02x, want 0x%02x", len(transportData)+i, payload[len(transportData)+i], b)
		}
	}
}

func TestProcessReplyIPv4(t *testing.T) {
	originalSrc := netip.MustParseAddr("10.0.0.2")

	// Build a complete IPv4 + ICMP Echo Reply packet
	totalLen := header.IPv4MinimumSize + header.ICMPv4MinimumSize + 4
	data := make([]byte, totalLen)

	// Build IPv4 header
	ipHdr := header.IPv4(data)
	ipHdr.Encode(&header.IPv4Fields{
		TotalLength: uint16(totalLen),
		TTL:         64,
		Protocol:    uint8(header.ICMPv4ProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4([4]byte{8, 8, 8, 8}),
		DstAddr:     tcpip.AddrFrom4([4]byte{192, 168, 1, 1}), // will be overwritten
	})
	ipHdr.SetChecksum(^ipHdr.CalculateChecksum())

	// Build ICMPv4 Echo Reply with XORed identifier
	icmpHdr := header.ICMPv4(ipHdr.Payload())
	icmpHdr[0] = byte(header.ICMPv4EchoReply) // Type: Echo Reply
	icmpHdr[1] = 0                             // Code: 0
	binary.BigEndian.PutUint16(icmpHdr[4:6], 0x1234^icmpXORIdent) // XORed identifier
	binary.BigEndian.PutUint16(icmpHdr[6:8], 1)                   // Sequence
	copy(icmpHdr[8:], []byte{0xDE, 0xAD, 0xBE, 0xEF})
	icmpHdr.SetChecksum(header.ICMPv4Checksum(icmpHdr, 0))

	gStack, err := createTestStack()
	if err != nil {
		t.Fatal("failed to create test stack:", err)
	}

	f := &icmpForwarder{gStack: gStack}
	ok := f.processReplyIPv4(originalSrc, data)
	if !ok {
		t.Fatal("processReplyIPv4 returned false")
	}

	// Verify the destination was changed to original source
	resultHdr := header.IPv4(data)
	gotDst := netip.AddrFrom4(resultHdr.DestinationAddress().As4())
	if gotDst != originalSrc {
		t.Fatalf("destination = %s, want %s", gotDst, originalSrc)
	}

	// Verify the identifier was XORed back to original
	resultICMP := header.ICMPv4(resultHdr.Payload())
	gotIdent := binary.BigEndian.Uint16(resultICMP[4:6])
	if gotIdent != 0x1234 {
		t.Fatalf("identifier = 0x%04x, want 0x%04x", gotIdent, 0x1234)
	}

	// Verify source is unchanged
	gotSrc := netip.AddrFrom4(resultHdr.SourceAddress().As4())
	wantSrc := netip.MustParseAddr("8.8.8.8")
	if gotSrc != wantSrc {
		t.Fatalf("source = %s, want %s", gotSrc, wantSrc)
	}

	// Verify IP checksum is valid: save it, zero it, recalculate, compare
	storedChecksum := resultHdr.Checksum()
	resultHdr.SetChecksum(0)
	recalculated := ^resultHdr.CalculateChecksum()
	if storedChecksum != recalculated {
		t.Fatalf("IP checksum mismatch: stored=0x%04x, recalculated=0x%04x", storedChecksum, recalculated)
	}
}

func TestProcessReplyIPv6(t *testing.T) {
	originalSrc := netip.MustParseAddr("fd00::2")

	payloadLen := header.ICMPv6MinimumSize + 4
	totalLen := header.IPv6MinimumSize + payloadLen
	data := make([]byte, totalLen)

	// Build IPv6 header
	srcAddr := tcpip.AddrFrom16([16]byte{0: 0x20, 1: 0x01, 2: 0x0d, 3: 0xb8}) // 2001:db8::
	dstAddr := tcpip.AddrFrom16([16]byte{0: 0xfd, 1: 0x01})                     // fd01::
	ipHdr := header.IPv6(data)
	ipHdr.Encode(&header.IPv6Fields{
		PayloadLength:     uint16(payloadLen),
		TransportProtocol: header.ICMPv6ProtocolNumber,
		HopLimit:          64,
		SrcAddr:           srcAddr,
		DstAddr:           dstAddr,
	})

	// Build ICMPv6 Echo Reply with XORed identifier
	icmpHdr := header.ICMPv6(ipHdr.Payload())
	icmpHdr[0] = byte(header.ICMPv6EchoReply) // Type: Echo Reply
	icmpHdr[1] = 0                             // Code: 0
	binary.BigEndian.PutUint16(icmpHdr[4:6], 0x5678^icmpXORIdent)
	binary.BigEndian.PutUint16(icmpHdr[6:8], 1)
	copy(icmpHdr[8:], []byte{0xCA, 0xFE, 0xBA, 0xBE})
	icmpHdr.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
		Header: icmpHdr,
		Src:    srcAddr,
		Dst:    dstAddr,
	}))

	gStack, err := createTestStack()
	if err != nil {
		t.Fatal("failed to create test stack:", err)
	}

	f := &icmpForwarder{gStack: gStack}
	ok := f.processReplyIPv6(originalSrc, data)
	if !ok {
		t.Fatal("processReplyIPv6 returned false")
	}

	// Verify destination was changed
	resultHdr := header.IPv6(data)
	gotDst := netip.AddrFrom16(resultHdr.DestinationAddress().As16())
	if gotDst != originalSrc {
		t.Fatalf("destination = %s, want %s", gotDst, originalSrc)
	}

	// Verify identifier was XORed back
	resultICMP := header.ICMPv6(resultHdr.Payload())
	gotIdent := binary.BigEndian.Uint16(resultICMP[4:6])
	if gotIdent != 0x5678 {
		t.Fatalf("identifier = 0x%04x, want 0x%04x", gotIdent, 0x5678)
	}
}

func TestProcessReplyRejectsNonEchoReply(t *testing.T) {
	originalSrc := netip.MustParseAddr("10.0.0.2")

	totalLen := header.IPv4MinimumSize + header.ICMPv4MinimumSize + 4
	data := make([]byte, totalLen)

	ipHdr := header.IPv4(data)
	ipHdr.Encode(&header.IPv4Fields{
		TotalLength: uint16(totalLen),
		TTL:         64,
		Protocol:    uint8(header.ICMPv4ProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4([4]byte{8, 8, 8, 8}),
		DstAddr:     tcpip.AddrFrom4([4]byte{10, 0, 0, 2}),
	})
	ipHdr.SetChecksum(^ipHdr.CalculateChecksum())

	icmpHdr := header.ICMPv4(ipHdr.Payload())
	icmpHdr[0] = 3 // Destination Unreachable (not Echo Reply)
	icmpHdr[1] = 0

	gStack, err := createTestStack()
	if err != nil {
		t.Fatal("failed to create test stack:", err)
	}

	f := &icmpForwarder{gStack: gStack}
	ok := f.processReplyIPv4(originalSrc, data)
	if ok {
		t.Fatal("processReplyIPv4 should return false for non-Echo Reply")
	}
}

func TestProcessReplyRejectsTooSmallPacket(t *testing.T) {
	originalSrc := netip.MustParseAddr("10.0.0.2")

	gStack, err := createTestStack()
	if err != nil {
		t.Fatal("failed to create test stack:", err)
	}

	f := &icmpForwarder{gStack: gStack}

	// Too small for IPv4 header + ICMP header
	ok := f.processReplyIPv4(originalSrc, make([]byte, 10))
	if ok {
		t.Fatal("processReplyIPv4 should return false for too-small packet")
	}

	// Too small for IPv6 header + ICMP header
	ok = f.processReplyIPv6(originalSrc, make([]byte, 10))
	if ok {
		t.Fatal("processReplyIPv6 should return false for too-small packet")
	}
}

// createTestStack creates a minimal gVisor stack for testing.
func createTestStack() (*stack.Stack, error) {
	opts := stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{},
	}
	gStack := stack.New(opts)
	ep := channel.New(256, 1500, "")
	if err := gStack.CreateNIC(defaultNIC, ep); err != nil {
		return nil, fmt.Errorf("CreateNIC: %v", err)
	}
	gStack.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: defaultNIC},
		{Destination: header.IPv6EmptySubnet, NIC: defaultNIC},
	})
	_ = gStack.SetSpoofing(defaultNIC, true)
	_ = gStack.SetPromiscuousMode(defaultNIC, true)
	return gStack, nil
}

// buildMockPacketBuffer creates a gVisor PacketBuffer with given transport header and data.
func buildMockPacketBuffer(t *testing.T, transportHeader, payload []byte) *stack.PacketBuffer {
	t.Helper()
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: len(transportHeader),
		Payload:            buffer.MakeWithData(payload),
	})
	pkt.TransportHeader().Push(len(transportHeader))
	copy(pkt.TransportHeader().Slice(), transportHeader)
	return pkt
}

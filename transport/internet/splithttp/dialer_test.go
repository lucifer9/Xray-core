package splithttp

import (
	"context"
	gonet "net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apernet/quic-go/http3"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
)

func countQuicTransportListeners() int {
	stacks := make([]byte, 1<<20)
	for {
		n := runtime.Stack(stacks, true)
		if n < len(stacks) {
			return strings.Count(string(stacks[:n]), "quic-go.(*Transport).listen")
		}
		stacks = make([]byte, 2*len(stacks))
	}
}

// A failed HTTP/3 dial must release its QUIC transport and UDP socket.
func TestH3DialFailureReleasesTransport(t *testing.T) {
	// a server that never answers, so the handshake can only fail
	silent, err := gonet.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		b := make([]byte, 2048)
		for {
			if _, _, err := silent.ReadFrom(b); err != nil {
				return
			}
		}
	}()

	port := net.Port(silent.LocalAddr().(*gonet.UDPAddr).Port)
	client := createHTTPClient(net.UDPDestination(net.LocalHostIP, port), &internet.MemoryStreamConfig{
		ProtocolName:     "splithttp",
		ProtocolSettings: &Config{Path: "shs"},
		SecurityType:     "tls",
		SecuritySettings: &tls.Config{
			ServerName:   "localhost",
			NextProtocol: []string{"h3"},
		},
	}).(*DefaultDialerClient)
	tr := client.client.Transport.(*http3.Transport)

	before := countQuicTransportListeners()
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := tr.Dial(ctx, "", tr.TLSClientConfig, tr.QUICConfig)
		cancel()
		if err == nil {
			t.Fatal("dial to a silent server succeeded")
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for countQuicTransportListeners() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d QUIC transports still listening after failed dials", countQuicTransportListeners()-before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

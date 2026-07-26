package conf

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/xtls/xray-core/proxy/tun"
)

func TestTunConfigBuildNetworkPathSafetySettings(t *testing.T) {
	message, err := (&TunConfig{
		Name:                          "utun99",
		AutoSystemRoutingTable:        []string{"0.0.0.0/0"},
		AutoSystemRoutingTableExclude: []string{"100.64.0.0/10"},
		EnableIcmpEchoForwarding:      true,
	}).Build()
	if err != nil {
		t.Fatal(err)
	}
	config := message.(*tun.Config)
	if !config.EnableIcmpEchoForwarding {
		t.Fatal("enableIcmpEchoForwarding was not preserved")
	}
	if len(config.AutoSystemRoutingTableExclude) != 1 || config.AutoSystemRoutingTableExclude[0] != "100.64.0.0/10" {
		t.Fatalf("excluded routes = %v", config.AutoSystemRoutingTableExclude)
	}
	if config.AutoOutboundsInterface != "auto" {
		t.Fatalf("implicit Outbound carrier selection = %q, want auto", config.AutoOutboundsInterface)
	}
}

func TestTunConfigSystemDNSAndEchoAreIndependent(t *testing.T) {
	for _, test := range []struct {
		name      string
		json      string
		wire      []byte
		dns, echo bool
	}{
		{"dns", `{"name":"utun99","autoSystemDNS":true}`, []byte{0x48, 1}, true, false},
		{"echo", `{"name":"utun99","enableIcmpEchoForwarding":true}`, []byte{0x58, 1}, false, true},
		{"both", `{"name":"utun99","autoSystemDNS":true,"enableIcmpEchoForwarding":true}`, []byte{0x48, 1, 0x58, 1}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var input TunConfig
			if err := json.Unmarshal([]byte(test.json), &input); err != nil {
				t.Fatal(err)
			}
			message, err := input.Build()
			if err != nil {
				t.Fatal(err)
			}
			built := message.(*tun.Config)
			var decoded tun.Config
			// Field 9 belongs to upstream system DNS; Direct Echo uses field 11.
			if err := proto.Unmarshal(test.wire, &decoded); err != nil {
				t.Fatal(err)
			}
			for _, config := range []*tun.Config{built, &decoded} {
				if config.AutoSystemDns != test.dns || config.EnableIcmpEchoForwarding != test.echo {
					t.Fatalf("DNS/Echo settings changed: %v", config)
				}
			}
		})
	}
}

func TestTunConfigBuildRejectsInvalidAutomaticRoutes(t *testing.T) {
	for _, config := range []*TunConfig{
		{Name: "utun99", AutoSystemRoutingTable: []string{"invalid"}},
		{Name: "utun99", AutoSystemRoutingTable: []string{"10.0.0.0/8"}, AutoSystemRoutingTableExclude: []string{"invalid"}},
	} {
		if _, err := config.Build(); err == nil {
			t.Fatal("expected invalid route error")
		}
	}
}

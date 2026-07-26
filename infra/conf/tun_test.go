package conf_test

import (
	"encoding/json"
	"runtime"
	"testing"

	. "github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/tun"
	"google.golang.org/protobuf/proto"
)

func TestTunConfigAutoSystem(t *testing.T) {
	creator := func() Buildable {
		return new(TunConfig)
	}

	runMultiTestCase(t, []TestCase{
		{
			Input:  `{"name": "xray0"}`,
			Parser: loadJSON(creator),
			Output: &tun.Config{Name: "xray0", Desc: "Wintun", MTU: 1500},
		},
		{
			Input:  `{"name": "xray0", "gateway": ["10.0.0.1/24"], "autoSystemDnsToGateway": true}`,
			Parser: loadJSON(creator),
			Output: &tun.Config{Name: "xray0", Desc: "Wintun", MTU: 1500, Gateway: []string{"10.0.0.1/24"}, AutoSystemDnsToGateway: true},
		},
		{
			Input:  `{"name": "xray0", "dns": ["1.1.1.1"], "autoSystemRoutingTable": ["0.0.0.0/0"], "autoSystemWfpBlockLeak": ["dns", "misconfigtun"]}`,
			Parser: loadJSON(creator),
			Output: &tun.Config{Name: "xray0", Desc: "Wintun", MTU: 1500, DNS: []string{"1.1.1.1"}, AutoSystemRoutingTable: []string{"0.0.0.0/0"}, AutoOutboundsInterface: "auto", AutoSystemWfpBlockLeak: []string{"dns", "misconfigtun"}},
		},
		{
			Input:  `{"name": "xray0", "dns": ["1.1.1.1"], "autoSystemRoutingTable": ["0.0.0.0/0"], "autoSystemWfpBlockLeak": ["DNS"]}`,
			Parser: loadJSON(creator),
			Output: &tun.Config{Name: "xray0", Desc: "Wintun", MTU: 1500, DNS: []string{"1.1.1.1"}, AutoSystemRoutingTable: []string{"0.0.0.0/0"}, AutoOutboundsInterface: "auto", AutoSystemWfpBlockLeak: []string{"dns"}},
		},
	})
}

// TestTunConfigAutoSystemNeeds checks that an option is rejected without the
// setting it needs, only on the system it takes effect on.
func TestTunConfigAutoSystemNeeds(t *testing.T) {
	for _, c := range []struct {
		input string
		goos  string // where it is rejected
	}{
		{`{"name": "xray0", "autoSystemWfpBlockLeak": ["misconfigtun"]}`, "windows"},
		{`{"name": "xray0", "autoSystemRoutingTable": ["0.0.0.0/0"], "autoSystemWfpBlockLeak": ["misconfigtun"]}`, ""},
		{`{"name": "xray0", "autoSystemRoutingTable": ["0.0.0.0/0"], "autoSystemWfpBlockLeak": ["dns"]}`, "windows"},
		{`{"name": "xray0", "autoSystemDnsToGateway": true}`, "linux"},
	} {
		config := new(TunConfig)
		if err := json.Unmarshal([]byte(c.input), config); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Build(); (err != nil) != (runtime.GOOS == c.goos) {
			t.Errorf("%s on %s: error = %v", c.input, runtime.GOOS, err)
		}
	}
}

func TestTunConfigAutoSystemWfpBlockLeakUnknown(t *testing.T) {
	config := new(TunConfig)
	if err := json.Unmarshal([]byte(`{"name": "xray0", "autoSystemWfpBlockLeak": ["dns", "ip"]}`), config); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Build(); err == nil {
		t.Error("an unknown autoSystemWfpBlockLeak value was accepted")
	}
}

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
		{"dns", `{"name":"utun99","gateway":["10.0.0.1/24"],"autoSystemDnsToGateway":true}`, []byte{0x48, 1}, true, false},
		{"echo", `{"name":"utun99","enableIcmpEchoForwarding":true}`, []byte{0x58, 1}, false, true},
		{"both", `{"name":"utun99","gateway":["10.0.0.1/24"],"autoSystemDnsToGateway":true,"enableIcmpEchoForwarding":true}`, []byte{0x48, 1, 0x58, 1}, true, true},
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
				if config.AutoSystemDnsToGateway != test.dns || config.EnableIcmpEchoForwarding != test.echo {
					t.Fatalf("DNS/Echo settings changed: %v", config)
				}
			}
		})
	}
}

func TestTunConfigWFPAndExcludedRoutesAreIndependent(t *testing.T) {
	// Upstream WFP uses field 10; local exclusions use field 12.
	wire := append([]byte{0x52, 3}, "dns"...)
	wire = append(wire, 0x62, 13)
	wire = append(wire, "100.64.0.0/10"...)
	var config tun.Config
	if err := proto.Unmarshal(wire, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.AutoSystemWfpBlockLeak) != 1 || config.AutoSystemWfpBlockLeak[0] != "dns" {
		t.Fatalf("WFP settings = %v", config.AutoSystemWfpBlockLeak)
	}
	if len(config.AutoSystemRoutingTableExclude) != 1 || config.AutoSystemRoutingTableExclude[0] != "100.64.0.0/10" {
		t.Fatalf("excluded routes = %v", config.AutoSystemRoutingTableExclude)
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

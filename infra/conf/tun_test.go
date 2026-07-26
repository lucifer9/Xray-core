package conf

import (
	"testing"

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

package conf

import (
	"github.com/xtls/xray-core/proxy/tun"
	"google.golang.org/protobuf/proto"
)

type TunConfig struct {
	Name                   string   `json:"name"`
	MTU                    uint32   `json:"mtu"`
	Gateway                []string `json:"gateway"`
	DNS                    []string `json:"dns"`
	UserLevel              uint32   `json:"userLevel"`
	EnableICMPForwarding   bool     `json:"enableICMPForwarding"`
	AutoSystemRoutingTable []string `json:"autoSystemRoutingTable"`
	AutoOutboundsInterface *string  `json:"autoOutboundsInterface"`
	AutoRoute              bool     `json:"autoRoute"`
	// RouteExclude lists target CIDRs excluded from the tun routing table.
	RouteExclude []string `json:"routeExclude"`
}

func (v *TunConfig) Build() (proto.Message, error) {
	config := &tun.Config{
		Name:                   v.Name,
		MTU:                    v.MTU,
		Gateway:                v.Gateway,
		DNS:                    v.DNS,
		UserLevel:              v.UserLevel,
		EnableIcmpForwarding:   v.EnableICMPForwarding,
		AutoSystemRoutingTable: v.AutoSystemRoutingTable,
		AutoRoute:              v.AutoRoute,
		RouteExclude:           v.RouteExclude,
	}
	if v.AutoOutboundsInterface != nil {
		config.AutoOutboundsInterface = *v.AutoOutboundsInterface
	} else if v.AutoRoute {
		config.AutoOutboundsInterface = "auto"
	} else if len(v.AutoSystemRoutingTable) > 0 {
		config.AutoOutboundsInterface = "auto"
	}

	if config.MTU == 0 {
		config.MTU = 1500
	}
	if config.AutoRoute && len(config.Gateway) == 0 {
		config.Gateway = []string{"198.18.0.1/16"}
	}
	return config, nil
}

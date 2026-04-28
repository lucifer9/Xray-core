package conf_test

import (
	"testing"

	. "github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/tun"
)

func TestTunConfigAutoRoute(t *testing.T) {
	creator := func() Buildable {
		return new(TunConfig)
	}

	runMultiTestCase(t, []TestCase{
		{
			Input: `{
				"name": "xray0",
				"autoRoute": true
			}`,
			Parser: loadJSON(creator),
			Output: &tun.Config{
				Name:                   "xray0",
				MTU:                    1500,
				AutoRoute:              true,
				AutoOutboundsInterface: "auto",
			},
		},
		{
			Input: `{
				"name": "xray0",
				"autoRoute": false
			}`,
			Parser: loadJSON(creator),
			Output: &tun.Config{
				Name:  "xray0",
				MTU:   1500,
				AutoRoute: false,
			},
		},
		{
			Input: `{
				"name": "xray0",
				"autoRoute": true,
				"autoOutboundsInterface": "eth0"
			}`,
			Parser: loadJSON(creator),
			Output: &tun.Config{
				Name:                   "xray0",
				MTU:                    1500,
				AutoRoute:              true,
				AutoOutboundsInterface: "eth0",
			},
		},
		{
			Input: `{
				"name": "xray0",
				"autoSystemRoutingTable": ["100"]
			}`,
			Parser: loadJSON(creator),
			Output: &tun.Config{
				Name:                   "xray0",
				MTU:                    1500,
				AutoSystemRoutingTable: []string{"100"},
				AutoOutboundsInterface: "auto",
			},
		},
		{
			Input: `{
				"name": "xray0",
				"autoRoute": true,
				"autoSystemRoutingTable": ["100"]
			}`,
			Parser: loadJSON(creator),
			Output: &tun.Config{
				Name:                   "xray0",
				MTU:                    1500,
				AutoRoute:              true,
				AutoSystemRoutingTable: []string{"100"},
				AutoOutboundsInterface: "auto",
			},
		},
	})
}

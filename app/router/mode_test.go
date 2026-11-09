package router_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/google/go-cmp/cmp"
	. "github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/geodata"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/outbound"
	routing_session "github.com/xtls/xray-core/features/routing/session"
	"github.com/xtls/xray-core/proxy"
	proxydns "github.com/xtls/xray-core/proxy/dns"
	"github.com/xtls/xray-core/testing/mocks"
)

// fakeHandler is an outbound handler that only reports its proxy.
type fakeHandler struct {
	outbound.Handler
	out proxy.Outbound
}

func (h *fakeHandler) GetOutbound() proxy.Outbound { return h.out }

func port(p uint32) *net.PortList {
	return &net.PortList{Range: []*net.PortRange{{From: p, To: p}}}
}

func modeConfig(strategy Config_DomainStrategy, bSelector string) *Config {
	return &Config{
		DomainStrategy: strategy,
		Rule: []*RoutingRule{
			{TargetTag: &RoutingRule_Tag{Tag: "dns-out"}, PortList: port(53), RuleTag: "dns"},
			{TargetTag: &RoutingRule_Tag{Tag: "direct"}, RuleTag: "lan", Ip: []*geodata.IPRule{{
				Value: &geodata.IPRule_Custom{Custom: &geodata.CIDRRule{Cidr: &geodata.CIDR{Ip: []byte{192, 168, 0, 0}, Prefix: 16}}},
			}}},
			{TargetTag: &RoutingRule_Tag{Tag: "blocked"}, Networks: []net.Network{net.Network_UDP}, PortList: port(443), RuleTag: "quic"},
			{TargetTag: &RoutingRule_BalancingTag{BalancingTag: "a"}, Networks: []net.Network{net.Network_TCP}, RuleTag: "final"},
		},
		BalancingRule: []*BalancingRule{
			{Tag: "a", OutboundSelector: []string{"a-"}},
			{Tag: "b", OutboundSelector: []string{bSelector}},
		},
	}
}

// newModeRouter builds a router whose DNS client fails the test on any
// lookup, so tests can assert that global mode resolves nothing.
func newModeRouter(t *testing.T, strategy Config_DomainStrategy) *Router {
	mockCtl := gomock.NewController(t)
	handlers := map[string]outbound.Handler{
		"dns-out": &fakeHandler{out: new(proxydns.Handler)},
		"direct":  &fakeHandler{},
		"blocked": &fakeHandler{},
	}
	mockOhm := mocks.NewOutboundManager(mockCtl)
	mockOhm.EXPECT().GetHandler(gomock.Any()).DoAndReturn(func(tag string) outbound.Handler {
		return handlers[tag]
	}).AnyTimes()
	mockHs := mocks.NewOutboundHandlerSelector(mockCtl)
	for _, prefix := range []string{"a-", "b-", "c-"} {
		mockHs.EXPECT().Select(gomock.Eq([]string{prefix})).Return([]string{prefix + "1"}).AnyTimes()
	}

	r := new(Router)
	common.Must(r.Init(context.TODO(), modeConfig(strategy, "b-"), mocks.NewDNSClient(mockCtl), &mockOutboundManager{
		Manager:         mockOhm,
		HandlerSelector: mockHs,
	}, nil))
	return r
}

type pickResult struct {
	Outbound, RuleTag string
}

func pick(t *testing.T, r *Router, dest net.Destination) (pickResult, error) {
	t.Helper()
	ctx := session.ContextWithOutbounds(context.Background(), []*session.Outbound{{Target: dest}})
	route, err := r.PickRoute(routing_session.AsRoutingContext(ctx))
	if err != nil {
		return pickResult{}, err
	}
	return pickResult{route.GetOutboundTag(), route.GetRuleTag()}, nil
}

func expectRoutes(t *testing.T, r *Router, want map[net.Destination]pickResult) {
	t.Helper()
	for dest, w := range want {
		got, err := pick(t, r, dest)
		if err != nil {
			t.Error(dest, ": ", err)
		} else if got != w {
			t.Errorf("%v: want %+v, got %+v", dest, w, got)
		}
	}
}

var (
	tcpDomain = net.TCPDestination(net.DomainAddress("example.com"), 443)
	udpLAN    = net.UDPDestination(net.ParseAddress("192.168.1.1"), 443)
	udpOther  = net.UDPDestination(net.ParseAddress("8.8.8.8"), 123)
	udpDNS    = net.UDPDestination(net.ParseAddress("1.1.1.1"), 53)
)

func TestRoutingModeProxyKeepsOnlyDNSRules(t *testing.T) {
	r := newModeRouter(t, Config_AsIs)
	common.Must(r.SetRoutingMode(RoutingModeState{Mode: RoutingModeProxy, Target: "b"}))

	expectRoutes(t, r, map[net.Destination]pickResult{
		tcpDomain: {"b-1", ""},
		udpLAN:    {"b-1", ""},
		udpOther:  {"b-1", ""},
		udpDNS:    {"dns-out", "dns"},
	})
	if got := r.GetRoutingMode(); got != (RoutingModeState{Mode: RoutingModeProxy, Target: "b"}) {
		t.Error("unexpected mode ", got)
	}
}

func TestRoutingModeDirectAndBackToConfig(t *testing.T) {
	r := newModeRouter(t, Config_AsIs)
	common.Must(r.SetRoutingMode(RoutingModeState{Mode: RoutingModeDirect, Target: "direct"}))
	expectRoutes(t, r, map[net.Destination]pickResult{
		tcpDomain: {"direct", ""},
		udpOther:  {"direct", ""},
		udpDNS:    {"dns-out", "dns"},
	})

	common.Must(r.SetRoutingMode(RoutingModeState{Mode: RoutingModeConfig}))
	expectRoutes(t, r, map[net.Destination]pickResult{
		tcpDomain: {"a-1", "final"},
		udpLAN:    {"direct", "lan"},
	})
	if _, err := pick(t, r, udpOther); err != common.ErrNoClue {
		t.Error("expect unmatched traffic to have no route in config mode, got ", err)
	}
	if got := r.GetRoutingMode(); got != (RoutingModeState{Mode: RoutingModeConfig}) {
		t.Error("unexpected mode ", got)
	}
}

func TestRoutingModeSkipsDNSLookups(t *testing.T) {
	udpDomain := net.UDPDestination(net.DomainAddress("example.com"), 123)
	for _, strategy := range []Config_DomainStrategy{Config_IpOnDemand, Config_IpIfNonMatch} {
		r := newModeRouter(t, strategy)
		common.Must(r.SetRoutingMode(RoutingModeState{Mode: RoutingModeProxy, Target: "b"}))
		expectRoutes(t, r, map[net.Destination]pickResult{
			tcpDomain: {"b-1", ""},
			udpDomain: {"b-1", ""},
		})
	}
}

func TestRoutingModeFollowsReload(t *testing.T) {
	r := newModeRouter(t, Config_AsIs)
	common.Must(r.SetRoutingMode(RoutingModeState{Mode: RoutingModeProxy, Target: "b"}))

	common.Must(r.ReloadRules(modeConfig(Config_AsIs, "c-"), false))
	expectRoutes(t, r, map[net.Destination]pickResult{tcpDomain: {"c-1", ""}})

	withoutB := modeConfig(Config_AsIs, "b-")
	withoutB.BalancingRule = withoutB.BalancingRule[:1]
	common.Must(r.ReloadRules(withoutB, false))
	if _, err := pick(t, r, tcpDomain); err == nil || err == common.ErrNoClue {
		t.Error("expect an error once the proxy balancer is gone, got ", err)
	}
	if got := r.GetRoutingMode(); got != (RoutingModeState{Mode: RoutingModeProxy, Target: "b"}) {
		t.Error("reload must not change the mode, got ", got)
	}
}

func TestRoutingModeRejectsInvalidStates(t *testing.T) {
	r := newModeRouter(t, Config_AsIs)
	want := RoutingModeState{Mode: RoutingModeProxy, Target: "b"}
	common.Must(r.SetRoutingMode(want))

	for name, s := range map[string]RoutingModeState{
		"unknown mode":         {Mode: "global", Target: "b"},
		"config with target":   {Mode: RoutingModeConfig, Target: "b"},
		"proxy without target": {Mode: RoutingModeProxy},
		"missing balancer":     {Mode: RoutingModeProxy, Target: "missing"},
		"missing outbound":     {Mode: RoutingModeDirect, Target: "missing"},
	} {
		if err := r.SetRoutingMode(s); err == nil {
			t.Error(name, ": expected an error")
		}
	}
	if got := r.GetRoutingMode(); got != want {
		t.Error("rejected updates changed the mode to ", got)
	}
}

func TestRoutingModeRejectsScriptRouter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "route.lua")
	common.Must(os.WriteFile(path, []byte(`function HandleRoute() return "direct" end`), 0o600))
	r := new(Router)
	common.Must(r.Init(context.Background(), &Config{Script: path}, nil, nil, nil))

	if err := r.SetRoutingMode(RoutingModeState{Mode: RoutingModeDirect, Target: "direct"}); err == nil {
		t.Error("expect routing mode to be rejected with a routing script")
	}
}

func TestListRuleTargetsMarksKeptRules(t *testing.T) {
	r := newModeRouter(t, Config_AsIs)
	want := []RuleTarget{
		{RuleTag: "dns", OutboundTag: "dns-out", KeptInGlobalMode: true},
		{RuleTag: "lan", OutboundTag: "direct"},
		{RuleTag: "quic", OutboundTag: "blocked"},
		{RuleTag: "final", BalancerTag: "a"},
	}
	if diff := cmp.Diff(want, r.ListRuleTargets()); diff != "" {
		t.Error("unexpected rule targets (-want +got):\n", diff)
	}
}

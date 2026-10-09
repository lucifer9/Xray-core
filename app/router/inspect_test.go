package router_test

import (
	"context"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/google/go-cmp/cmp"
	. "github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	routing_session "github.com/xtls/xray-core/features/routing/session"
	"github.com/xtls/xray-core/testing/mocks"
)

func newInspectRouter(t *testing.T) *Router {
	config := &Config{
		Rule: []*RoutingRule{
			{
				TargetTag: &RoutingRule_Tag{Tag: "direct"},
				Networks:  []net.Network{net.Network_UDP},
				RuleTag:   "udp",
			},
			{
				TargetTag: &RoutingRule_BalancingTag{BalancingTag: "a"},
				Networks:  []net.Network{net.Network_TCP},
				RuleTag:   "final",
			},
		},
		BalancingRule: []*BalancingRule{
			{Tag: "b", OutboundSelector: []string{"b-"}, Strategy: "roundRobin"},
			{Tag: "a", OutboundSelector: []string{"a-"}},
		},
	}

	mockCtl := gomock.NewController(t)
	mockHs := mocks.NewOutboundHandlerSelector(mockCtl)
	mockHs.EXPECT().Select(gomock.Eq([]string{"a-"})).Return([]string{"a-1"}).AnyTimes()
	mockHs.EXPECT().Select(gomock.Eq([]string{"b-"})).Return([]string{"b-1", "b-2"}).AnyTimes()

	r := new(Router)
	common.Must(r.Init(context.TODO(), config, mocks.NewDNSClient(mockCtl), &mockOutboundManager{
		Manager:         mocks.NewOutboundManager(mockCtl),
		HandlerSelector: mockHs,
	}, nil))
	return r
}

func pickTCP(t *testing.T, r *Router) string {
	t.Helper()
	ctx := session.ContextWithOutbounds(context.Background(), []*session.Outbound{{
		Target: net.TCPDestination(net.DomainAddress("example.com"), 443),
	}})
	route, err := r.PickRoute(routing_session.AsRoutingContext(ctx))
	common.Must(err)
	return route.GetOutboundTag()
}

func TestListBalancers(t *testing.T) {
	r := newInspectRouter(t)
	common.Must(r.SetOverrideTarget("b", "b-2"))

	got, err := r.ListBalancers()
	common.Must(err)
	want := []BalancerState{
		{Tag: "a", Selectors: []string{"a-"}, Strategy: "random", Candidates: []string{"a-1"}, Principle: []string{"a-1"}},
		{Tag: "b", Selectors: []string{"b-"}, Strategy: "roundRobin", Candidates: []string{"b-1", "b-2"}, Override: "b-2", Principle: []string{"b-1", "b-2"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Error("unexpected balancers (-want +got):\n", diff)
	}
}

func TestSetRuleBalancer(t *testing.T) {
	r := newInspectRouter(t)
	if tag := pickTCP(t, r); tag != "a-1" {
		t.Fatal("expect a-1 before switching, got ", tag)
	}

	common.Must(r.SetRuleBalancer("final", "b"))

	if tag := pickTCP(t, r); tag != "b-1" && tag != "b-2" {
		t.Error("expect a b-* outbound after switching, got ", tag)
	}
	want := []RuleTarget{
		{RuleTag: "udp", OutboundTag: "direct"},
		{RuleTag: "final", BalancerTag: "b"},
	}
	if diff := cmp.Diff(want, r.ListRuleTargets()); diff != "" {
		t.Error("unexpected rule targets (-want +got):\n", diff)
	}
}

func TestSetRuleBalancerRejectsInvalidTargets(t *testing.T) {
	r := newInspectRouter(t)
	before := r.ListRuleTargets()

	for name, args := range map[string][2]string{
		"unknown balancer": {"final", "missing"},
		"unknown rule":     {"missing", "b"},
		"outbound rule":    {"udp", "b"},
		"empty rule tag":   {"", "b"},
	} {
		if err := r.SetRuleBalancer(args[0], args[1]); err == nil {
			t.Error(name, ": expected an error")
		}
	}
	if diff := cmp.Diff(before, r.ListRuleTargets()); diff != "" {
		t.Error("rules changed after rejected updates (-before +after):\n", diff)
	}
}

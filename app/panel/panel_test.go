package panel_test

import (
	"bytes"
	"encoding/json"
	stdnet "net"
	"net/http"
	"strings"
	"testing"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/app/metrics"
	"github.com/xtls/xray-core/app/proxyman"
	_ "github.com/xtls/xray-core/app/proxyman/inbound"
	_ "github.com/xtls/xray-core/app/proxyman/outbound"
	"github.com/xtls/xray-core/app/router"
	appstats "github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	proxydns "github.com/xtls/xray-core/proxy/dns"
	"github.com/xtls/xray-core/proxy/freedom"
)

// startServer runs an Xray instance whose "final" rule uses balancer "a"
// and returns the panel's base URL.
func startServer(t *testing.T) string {
	t.Helper()
	return startServerWith(t)
}

// startServerWith is startServer with extra rules placed before "final".
func startServerWith(t *testing.T, rules ...*router.RoutingRule) string {
	t.Helper()
	listener, err := stdnet.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	outbound := func(tag string) *core.OutboundHandlerConfig {
		return &core.OutboundHandlerConfig{Tag: tag, ProxySettings: serial.ToTypedMessage(&freedom.Config{})}
	}
	server, err := core.New(&core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(&appstats.Config{}),
			serial.ToTypedMessage(&router.Config{
				Rule: append(rules, &router.RoutingRule{
					TargetTag: &router.RoutingRule_BalancingTag{BalancingTag: "a"},
					Networks:  []net.Network{net.Network_TCP},
					RuleTag:   "final",
				}),
				BalancingRule: []*router.BalancingRule{
					{Tag: "a", OutboundSelector: []string{"a-"}},
					{Tag: "b", OutboundSelector: []string{"b-"}},
				},
			}),
			serial.ToTypedMessage(&metrics.Config{Listen: addr}),
		},
		Outbound: []*core.OutboundHandlerConfig{
			outbound("direct"), outbound("a-1"), outbound("b-1"),
			{Tag: "dns-out", ProxySettings: serial.ToTypedMessage(&proxydns.Config{})},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return "http://" + addr + "/panel/"
}

func post(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

type mode struct {
	Mode   string `json:"mode"`
	Target string `json:"target"`
}

type status struct {
	Mode            mode     `json:"mode"`
	DirectOutbounds []string `json:"directOutbounds"`
	Rules           []struct {
		RuleTag      string `json:"ruleTag"`
		Balancer     string `json:"balancer"`
		KeptInGlobal bool   `json:"keptInGlobal"`
	} `json:"rules"`
	Balancers []struct {
		Tag        string   `json:"tag"`
		Candidates []string `json:"candidates"`
	} `json:"balancers"`
	ObservatoryError string `json:"observatoryError"`
	CanCheck         bool   `json:"canCheck"`
}

func getStatus(t *testing.T, base string) status {
	t.Helper()
	resp, err := http.Get(base + "api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("status returned ", resp.StatusCode)
	}
	var s status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPanelServesPage(t *testing.T) {
	base := startServer(t)
	resp, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatal("unexpected page response ", resp.StatusCode, " ", resp.Header.Get("Content-Type"))
	}
}

func TestPanelSwitchesRuleBalancer(t *testing.T) {
	base := startServer(t)

	s := getStatus(t, base)
	if len(s.Rules) != 1 || s.Rules[0].Balancer != "a" {
		t.Fatalf("expect final -> a, got %+v", s.Rules)
	}
	if len(s.Balancers) != 2 || s.Balancers[1].Tag != "b" || len(s.Balancers[1].Candidates) != 1 || s.Balancers[1].Candidates[0] != "b-1" {
		t.Fatalf("unexpected balancers %+v", s.Balancers)
	}
	if s.ObservatoryError == "" || s.CanCheck {
		t.Fatal("expect observatory to be reported missing without an observatory")
	}

	if code, _ := post(t, base+"api/rule", map[string]string{"ruleTag": "final", "balancer": "b"}); code != http.StatusOK {
		t.Fatal("switching returned ", code)
	}
	if s := getStatus(t, base); s.Rules[0].Balancer != "b" {
		t.Fatalf("expect final -> b after switching, got %+v", s.Rules)
	}
	code, route := post(t, base+"api/route", map[string]string{"target": "example.com:443", "network": "tcp"})
	if code != http.StatusOK || route["outbound"] != "b-1" || route["ruleTag"] != "final" {
		t.Fatal("unexpected route ", code, " ", route)
	}

	if code, body := post(t, base+"api/rule", map[string]string{"ruleTag": "final", "balancer": "missing"}); code != http.StatusBadRequest || body["error"] == "" {
		t.Fatal("expect unknown balancer to be rejected, got ", code, " ", body)
	}
	if code, _ := post(t, base+"api/check", map[string]any{"tags": []string{"b-1"}}); code != http.StatusBadRequest {
		t.Fatal("expect check to be rejected without burstObservatory, got ", code)
	}
}

func TestPanelRejectsForeignRequests(t *testing.T) {
	base := startServer(t)

	resp, err := http.Post(base+"api/rule", "text/plain", strings.NewReader(`{"ruleTag":"final","balancer":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Error("expect non-JSON POST to be rejected, got ", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, base+"api/status", nil)
	req.Host = "evil.example:80"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Error("expect foreign Host to be rejected, got ", resp.StatusCode)
	}

	if s := getStatus(t, base); s.Rules[0].Balancer != "a" {
		t.Error("rejected requests must not change rules")
	}
}

func TestPanelRoutingMode(t *testing.T) {
	port := func(p uint32) *net.PortList { return &net.PortList{Range: []*net.PortRange{{From: p, To: p}}} }
	base := startServerWith(t,
		&router.RoutingRule{TargetTag: &router.RoutingRule_Tag{Tag: "dns-out"}, PortList: port(53), RuleTag: "dns"},
		&router.RoutingRule{TargetTag: &router.RoutingRule_Tag{Tag: "a-1"}, PortList: port(8080), RuleTag: "pinned"},
	)
	route := func(target, network, wantOutbound, wantRule string) {
		t.Helper()
		code, body := post(t, base+"api/route", map[string]string{"target": target, "network": network})
		if code != http.StatusOK || body["outbound"] != wantOutbound || body["ruleTag"] != wantRule {
			t.Errorf("%s/%s: want %s via [%s], got %d %v", target, network, wantOutbound, wantRule, code, body)
		}
	}
	setMode := func(m mode) {
		t.Helper()
		if code, body := post(t, base+"api/mode", m); code != http.StatusOK {
			t.Fatal("setting mode ", m, " returned ", code, " ", body)
		}
		if got := getStatus(t, base).Mode; got != m {
			t.Fatalf("want mode %+v, got %+v", m, got)
		}
	}

	s := getStatus(t, base)
	if s.Mode != (mode{Mode: "config"}) {
		t.Fatal("expect config mode at start, got ", s.Mode)
	}
	if strings.Join(s.DirectOutbounds, ",") != "a-1,b-1,direct" {
		t.Error("expect freedom outbounds only, got ", s.DirectOutbounds)
	}
	for _, r := range s.Rules {
		if r.KeptInGlobal != (r.RuleTag == "dns") {
			t.Errorf("rule %s: keptInGlobal = %v", r.RuleTag, r.KeptInGlobal)
		}
	}
	route("1.2.3.4:8080", "tcp", "a-1", "pinned")

	setMode(mode{Mode: "proxy", Target: "b"})
	route("example.com:443", "tcp", "b-1", "")
	route("1.2.3.4:8080", "tcp", "b-1", "")
	route("1.1.1.1:53", "udp", "dns-out", "dns")

	setMode(mode{Mode: "direct", Target: "direct"})
	route("example.com:443", "tcp", "direct", "")
	route("1.1.1.1:53", "udp", "dns-out", "dns")

	for _, body := range []any{
		mode{Mode: "direct", Target: "dns-out"},
		mode{Mode: "direct", Target: "missing"},
		mode{Mode: "proxy", Target: "missing"},
		mode{Mode: "proxy"},
		mode{Mode: "config", Target: "b"},
		mode{Mode: "global", Target: "b"},
		map[string]string{"mode": "proxy", "balancer": "b"},
	} {
		if code, _ := post(t, base+"api/mode", body); code != http.StatusBadRequest {
			t.Errorf("expect %v to be rejected, got %d", body, code)
		}
	}
	if got := getStatus(t, base).Mode; got != (mode{Mode: "direct", Target: "direct"}) {
		t.Error("rejected requests changed the mode to ", got)
	}

	setMode(mode{Mode: "config"})
	route("example.com:443", "tcp", "a-1", "final")
}

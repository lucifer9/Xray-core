package router

import (
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/proxy"
	proxydns "github.com/xtls/xray-core/proxy/dns"
)

// RoutingMode decides how much of the rule list applies.
type RoutingMode string

const (
	// RoutingModeConfig routes by the configured rules.
	RoutingModeConfig RoutingMode = "config"
	// RoutingModeProxy sends all traffic to a balancer.
	RoutingModeProxy RoutingMode = "proxy"
	// RoutingModeDirect sends all traffic to an outbound.
	RoutingModeDirect RoutingMode = "direct"
)

// RoutingModeState is the active routing mode. In proxy and direct mode only
// rules that route to a DNS outbound still apply, so DNS hijacking keeps
// working; all other traffic goes to Target.
type RoutingModeState struct {
	Mode RoutingMode
	// Target is a balancer tag in proxy mode, an outbound tag in direct mode
	// and empty in config mode.
	Target string
}

// SetRoutingMode switches the routing mode for new connections.
func (r *Router) SetRoutingMode(s RoutingModeState) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch s.Mode {
	case RoutingModeConfig:
		if s.Target != "" {
			return errors.New("config mode takes no target")
		}
		r.mode.Store(nil)
		return nil
	case RoutingModeProxy, RoutingModeDirect:
	default:
		return errors.New("unknown routing mode ", string(s.Mode))
	}
	if r.script != nil {
		return errors.New("routing mode is unavailable with a routing script")
	}
	if s.Target == "" {
		return errors.New(string(s.Mode), " mode needs a target")
	}
	if s.Mode == RoutingModeProxy {
		if _, found := (*r.balancers.Load())[s.Target]; !found {
			return errors.New("balancer ", s.Target, " not found")
		}
	} else if r.ohm.GetHandler(s.Target) == nil {
		return errors.New("outbound ", s.Target, " not found")
	}
	r.mode.Store(&s)
	return nil
}

// GetRoutingMode returns the active routing mode.
func (r *Router) GetRoutingMode() RoutingModeState {
	if m := r.mode.Load(); m != nil {
		return *m
	}
	return RoutingModeState{Mode: RoutingModeConfig}
}

// keptInGlobalMode reports whether rule still applies in proxy and direct
// mode, which is the case for rules that route to a DNS outbound. It is
// evaluated per pick so it follows rule and outbound changes.
func (r *Router) keptInGlobalMode(rule *Rule) bool {
	if rule.Balancer != nil {
		return false
	}
	h, ok := r.ohm.GetHandler(rule.Tag).(proxy.GetOutbound)
	if !ok {
		return false
	}
	_, ok = h.GetOutbound().(*proxydns.Handler)
	return ok
}

// modeOutbound picks the outbound for traffic that hits no kept rule.
func (r *Router) modeOutbound(m *RoutingModeState) (string, error) {
	if m.Mode == RoutingModeDirect {
		return m.Target, nil
	}
	b, found := (*r.balancers.Load())[m.Target]
	if !found {
		return "", errors.New("balancer ", m.Target, " of proxy mode not found")
	}
	return b.PickOutbound()
}

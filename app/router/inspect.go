package router

import (
	"slices"
	"strings"

	"github.com/xtls/xray-core/common/errors"
)

// BalancerState is a snapshot of a balancer's configuration and selection.
type BalancerState struct {
	Tag         string
	Selectors   []string
	Strategy    string
	FallbackTag string
	// Candidates are the outbounds currently matched by Selectors.
	Candidates []string
	// Override is the target forced by OverrideBalancerTarget, empty if unset.
	Override string
	// Principle is what the strategy would pick without an override.
	Principle []string
}

// RuleTarget describes where a routing rule sends matched traffic. Exactly
// one of OutboundTag and BalancerTag is set.
type RuleTarget struct {
	RuleTag     string
	OutboundTag string
	BalancerTag string
}

func strategyName(s BalancingStrategy) string {
	switch s.(type) {
	case *LeastPingStrategy:
		return "leastPing"
	case *LeastLoadStrategy:
		return "leastLoad"
	case *RoundRobinStrategy:
		return "roundRobin"
	case *RandomStrategy:
		return "random"
	default:
		return "unknown"
	}
}

// ListBalancers returns the state of all balancers, sorted by tag.
func (r *Router) ListBalancers() ([]BalancerState, error) {
	balancers := *r.balancers.Load()
	states := make([]BalancerState, 0, len(balancers))
	for tag, b := range balancers {
		candidates, err := b.SelectOutbounds()
		if err != nil {
			return nil, errors.New("balancer ", tag, ": unable to select outbounds").Base(err)
		}
		state := BalancerState{
			Tag:         tag,
			Selectors:   slices.Clone(b.selectors),
			Strategy:    strategyName(b.strategy),
			FallbackTag: b.fallbackTag,
			Candidates:  slices.Clone(candidates),
			Override:    b.override.Get(),
		}
		if s, ok := b.strategy.(BalancingPrincipleTarget); ok {
			state.Principle = slices.Clone(s.GetPrincipleTarget(candidates))
		}
		states = append(states, state)
	}
	slices.SortFunc(states, func(a, b BalancerState) int { return strings.Compare(a.Tag, b.Tag) })
	return states, nil
}

// ListRuleTargets returns the target of every rule in matching order.
func (r *Router) ListRuleTargets() []RuleTarget {
	rules := *r.rules.Load()
	balancers := *r.balancers.Load()
	targets := make([]RuleTarget, 0, len(rules))
	for _, rule := range rules {
		t := RuleTarget{RuleTag: rule.RuleTag, OutboundTag: rule.Tag}
		if rule.Balancer != nil {
			for tag, b := range balancers {
				if b == rule.Balancer {
					t.BalancerTag = tag
					break
				}
			}
		}
		targets = append(targets, t)
	}
	return targets
}

// SetRuleBalancer points the balancer rule tagged ruleTag at another balancer.
// The rule list is swapped in one step, so unlike RemoveRule followed by
// AddRule there is no moment where traffic misses the rule.
func (r *Router) SetRuleBalancer(ruleTag, balancerTag string) error {
	if ruleTag == "" {
		return errors.New("empty rule tag")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	balancer, found := (*r.balancers.Load())[balancerTag]
	if !found {
		return errors.New("balancer ", balancerTag, " not found")
	}

	oldRules := *r.rules.Load()
	newRules := slices.Clone(oldRules)
	matched := false
	for i, rule := range oldRules {
		if rule.RuleTag != ruleTag {
			continue
		}
		if rule.Balancer == nil {
			return errors.New("rule ", ruleTag, " routes to outbound ", rule.Tag, ", not a balancer")
		}
		updated := *rule
		updated.Balancer = balancer
		newRules[i] = &updated
		matched = true
	}
	if !matched {
		return errors.New("rule ", ruleTag, " not found")
	}
	r.rules.Store(&newRules)
	return nil
}

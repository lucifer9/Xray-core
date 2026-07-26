//go:build windows

package tun

import (
	"context"
	"sync"

	"github.com/xtls/xray-core/common/errors"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// outboundGuard keeps Windows to the binding of autoOutboundsInterface, which
// keeps Xray's own connections out of the TUN. With weak host send or
// forwarding on for an IP version on the bound interface, Windows sends them
// where the routes lead, into the TUN, from that interface's address, and
// drops what comes back to that address through the TUN, so they stall.
//
// For the IP versions routed to the TUN, weak host send is turned off on that
// version's Outbound carrier interface while the TUN runs, and turned on again
// when the TUN stops or another interface takes over. Forwarding is what
// Mobile Hotspot and Internet Connection Sharing need, so it is only reported.
type outboundGuard struct {
	sync.Mutex
	policy   *outboundCarrierPolicy
	families []winipcfg.AddressFamily
	bound    map[winipcfg.AddressFamily]guardedCarrier
	stopped  bool
}

// guardedCarrier is the interface last checked for an IP version.
type guardedCarrier struct {
	luid       winipcfg.LUID
	name       string
	turnedOff  bool // whether weak host send was turned off on it
	forwarding bool // whether forwarding was on there
}

// start guards families, the IP versions routed to the TUN, on the carriers
// of policy.
func (g *outboundGuard) start(policy *outboundCarrierPolicy, families []winipcfg.AddressFamily) {
	g.Lock()
	g.policy = policy
	g.families = families
	g.bound = make(map[winipcfg.AddressFamily]guardedCarrier, len(families))
	g.Unlock()
	g.check()
}

// check turns weak host send off on the carrier interfaces, and warns when
// forwarding comes on there, but not again while it stays on.
func (g *outboundGuard) check() {
	g.Lock()
	defer g.Unlock()
	if g.stopped || g.policy == nil {
		return
	}
	for _, family := range g.families {
		var luid winipcfg.LUID
		var name string
		if carrier := g.policy.carrier(carrierFamilyOf(family)); carrier != nil {
			luid, _ = winipcfg.LUIDFromIndex(uint32(carrier.Index))
			name = carrier.Name
		}
		bound := g.bound[family]
		if luid != bound.luid {
			restoreWeakHostSend(family, bound)
			bound = guardedCarrier{luid: luid, name: name}
		}
		g.bound[family] = g.checkCarrier(family, bound)
	}
}

func (g *outboundGuard) checkCarrier(family winipcfg.AddressFamily, bound guardedCarrier) guardedCarrier {
	if bound.luid == 0 {
		return bound
	}
	row, err := bound.luid.IPInterface(family)
	if err != nil {
		return bound // the interface lacks that IP version
	}
	if row.ForwardingEnabled && !bound.forwarding {
		errors.LogWarning(context.Background(), "[tun] forwarding is on for ", familyName(family), " on ", bound.name, " (Mobile Hotspot and Internet Connection Sharing turn it on), so Windows ignores autoOutboundsInterface there, and Xray's own connections go into the TUN and stall: turn the hotspot off, or have it share the TUN instead of ", bound.name)
	}
	bound.forwarding = row.ForwardingEnabled
	if !row.WeakHostSend {
		return bound
	}
	if err := setWeakHostSend(row, false); err != nil {
		errors.LogWarningInner(context.Background(), err, "[tun] unable to turn weak host send off for ", familyName(family), " on ", bound.name)
		return bound
	}
	if !bound.turnedOff {
		bound.turnedOff = true
		errors.LogInfo(context.Background(), "[tun] weak host send turned off for ", familyName(family), " on ", bound.name, " while the TUN runs, as Windows would ignore autoOutboundsInterface")
	}
	return bound
}

// restore turns weak host send on again where check turned it off, for good.
func (g *outboundGuard) restore() {
	g.Lock()
	defer g.Unlock()
	for family, bound := range g.bound {
		restoreWeakHostSend(family, bound)
	}
	g.bound = nil
	g.stopped = true
}

func restoreWeakHostSend(family winipcfg.AddressFamily, bound guardedCarrier) {
	if !bound.turnedOff {
		return
	}
	row, err := bound.luid.IPInterface(family)
	if err == nil {
		err = setWeakHostSend(row, true)
	}
	if err != nil {
		errors.LogWarningInner(context.Background(), err, "[tun] unable to turn weak host send on again for ", familyName(family), " on ", bound.name)
	}
}

func carrierFamilyOf(family winipcfg.AddressFamily) carrierFamily {
	if family == windows.AF_INET {
		return carrierIPv4
	}
	return carrierIPv6
}

func setWeakHostSend(row *winipcfg.MibIPInterfaceRow, on bool) error {
	row.WeakHostSend = on
	if row.Family == windows.AF_INET {
		row.SitePrefixLength = 0 // as SetIpInterfaceEntry requires for IPv4
	}
	return row.Set()
}

func familyName(family winipcfg.AddressFamily) string {
	if family == windows.AF_INET {
		return "IPv4"
	}
	return "IPv6"
}

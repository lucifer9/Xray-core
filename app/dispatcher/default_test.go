package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
)

func TestUDPSniffProtocolCache(t *testing.T) {
	cache := &udpSniffProtocolCache{}
	now := time.Now()
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{
		Tag:    "all-in",
		Source: net.UDPDestination(net.ParseAddress("172.18.0.1"), 56569),
	})
	destination := net.UDPDestination(net.ParseAddress("35.190.80.1"), 443)

	cache.remember(ctx, destination, "quic", now)

	lookupTime := now.Add(time.Second)
	if protocol := cache.lookup(ctx, destination, lookupTime); protocol != "quic" {
		t.Fatalf("lookup() = %q, want quic", protocol)
	}

	otherDestination := net.UDPDestination(net.ParseAddress("35.190.80.2"), 443)
	if protocol := cache.lookup(ctx, otherDestination, now.Add(time.Second)); protocol != "" {
		t.Fatalf("lookup() for different destination = %q, want empty", protocol)
	}

	if protocol := cache.lookup(ctx, destination, lookupTime.Add(udpSniffProtocolTTL+time.Nanosecond)); protocol != "" {
		t.Fatalf("lookup() after expiry = %q, want empty", protocol)
	}
}

func TestUDPSniffProtocolCacheRefreshesTTLOnLookup(t *testing.T) {
	cache := &udpSniffProtocolCache{}
	now := time.Now()
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{
		Tag:    "all-in",
		Source: net.UDPDestination(net.ParseAddress("172.18.0.1"), 65130),
	})
	destination := net.UDPDestination(net.ParseAddress("3.165.39.9"), 443)

	cache.remember(ctx, destination, "quic", now)

	firstLookup := now.Add(udpSniffProtocolTTL - 100*time.Millisecond)
	if protocol := cache.lookup(ctx, destination, firstLookup); protocol != "quic" {
		t.Fatalf("lookup() = %q, want quic", protocol)
	}

	secondLookup := firstLookup.Add(udpSniffProtocolTTL - 100*time.Millisecond)
	if protocol := cache.lookup(ctx, destination, secondLookup); protocol != "quic" {
		t.Fatalf("lookup() after refreshed TTL = %q, want quic", protocol)
	}

	expiredLookup := secondLookup.Add(udpSniffProtocolTTL + time.Nanosecond)
	if protocol := cache.lookup(ctx, destination, expiredLookup); protocol != "" {
		t.Fatalf("lookup() after refreshed TTL expiry = %q, want empty", protocol)
	}
}

func TestUDPSniffProtocolCacheOnlyRemembersQUIC(t *testing.T) {
	cache := &udpSniffProtocolCache{}
	now := time.Now()
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{
		Tag:    "all-in",
		Source: net.UDPDestination(net.ParseAddress("172.18.0.1"), 56569),
	})
	destination := net.UDPDestination(net.ParseAddress("35.190.80.1"), 443)

	cache.remember(ctx, destination, "bittorrent", now)

	if protocol := cache.lookup(ctx, destination, now.Add(time.Second)); protocol != "" {
		t.Fatalf("lookup() = %q, want empty", protocol)
	}
}

package tun

import "testing"

func TestBuildWindowsAutomaticRoutesConsumesCommonPlan(t *testing.T) {
	plan, err := PlanAutomaticSystemRoutes(
		[]string{"10.0.0.0/8", "10.0.0.0/9", "2001:db8::/32"},
		[]string{"10.0.0.0/9", "2001:db8:8000::/33"},
		RouteCapacity{IPv4: maxAutomaticSystemRoutesPerFamily, IPv6: maxAutomaticSystemRoutesPerFamily},
	)
	if err != nil {
		t.Fatal(err)
	}
	routes, hasIPv4, hasIPv6 := buildWindowsAutomaticRoutes(append(plan.IPv4, plan.IPv6...))
	if !hasIPv4 || !hasIPv6 || len(routes) != 2 {
		t.Fatalf("routes=%v hasIPv4=%v hasIPv6=%v", routes, hasIPv4, hasIPv6)
	}
	if routes[0].Destination.String() != "10.128.0.0/9" || routes[0].NextHop.String() != "0.0.0.0" {
		t.Fatalf("IPv4 route = %+v", routes[0])
	}
	if routes[1].Destination.String() != "2001:db8::/33" || routes[1].NextHop.String() != "::" {
		t.Fatalf("IPv6 route = %+v", routes[1])
	}
}

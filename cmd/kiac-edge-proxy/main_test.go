package main

import "testing"

func TestRouteForPrefersLocalEndpoint(t *testing.T) {
	state, svc, port := routeTestState()

	route := state.routeFor("node-a", "15080", svc, port, false)
	if route.dial != "10.0.0.10:8080" || route.tunnelTarget != "" {
		t.Fatalf("route = %+v, want direct local endpoint", route)
	}
}

func TestRouteForTunnelsToRemoteEndpointNode(t *testing.T) {
	state, svc, port := routeTestState()
	state.endpointSlices[0].Endpoints = state.endpointSlices[0].Endpoints[1:]

	route := state.routeFor("node-c", "15080", svc, port, false)
	if route.dial != "192.168.65.11:15080" || route.tunnelTarget != "10.0.1.10:8080" {
		t.Fatalf("route = %+v, want tunnel to node-b proxy for remote endpoint", route)
	}
}

// TestRouteForIPv6TunnelsOverIPv6NodeIP checks that a v6 flow to a remote
// endpoint tunnels over the endpoint node's IPv6 InternalIP, not its v4,
// and carries the bracketed v6 pod address as the tunnel target.
func TestRouteForIPv6TunnelsOverIPv6NodeIP(t *testing.T) {
	state, svc, port := routeTestStateV6()

	route := state.routeFor("node-c", "15080", svc, port, true)
	if route.dial != "[fd00:65::11]:15080" || route.tunnelTarget != "[fd00:1::10]:8080" {
		t.Fatalf("route = %+v, want tunnel to node-b IPv6 proxy for remote v6 endpoint", route)
	}
}

// TestRouteForFamilyIsolation checks that routing one family never picks
// an endpoint of the other: a v6 route with only a v4 endpoint present
// yields no dial.
func TestRouteForFamilyIsolation(t *testing.T) {
	state, svc, port := routeTestState() // IPv4-only endpoints
	route := state.routeFor("node-a", "15080", svc, port, true)
	if route.dial != "" || route.tunnelTarget != "" {
		t.Fatalf("route = %+v, want empty route (no v6 endpoint)", route)
	}
}

func TestServicePortMatchesNamedTargetPort(t *testing.T) {
	name := "http"
	port := 9090
	if !servicePortMatchesEndpoint(
		servicePort{TargetPort: intOrString{Set: true, StrVal: "http"}},
		endpointPort{Name: &name, Port: &port},
	) {
		t.Fatal("named Service targetPort should match EndpointSlice port name")
	}
}

func routeTestState() (clusterState, serviceItem, servicePort) {
	svc := serviceItem{}
	svc.Metadata.Namespace = "default"
	svc.Metadata.Name = "upload"
	port := servicePort{
		Port:       8080,
		NodePort:   32080,
		TargetPort: intOrString{Set: true, IntVal: 8080},
	}

	ready := true
	epPort := 8080
	slice := endpointSliceItem{}
	slice.Metadata.Namespace = "default"
	slice.Metadata.Labels = map[string]string{"kubernetes.io/service-name": "upload"}
	slice.Ports = []endpointPort{{Port: &epPort}}
	slice.Endpoints = []endpointEntry{
		{Addresses: []string{"10.0.0.10"}, NodeName: "node-a"},
		{Addresses: []string{"10.0.1.10"}, NodeName: "node-b"},
	}
	for i := range slice.Endpoints {
		slice.Endpoints[i].Conditions.Ready = &ready
	}

	state := clusterState{
		endpointSlices: []endpointSliceItem{slice},
		nodeIPs: map[string]nodeAddrs{
			"node-a": {v4: "192.168.65.10"},
			"node-b": {v4: "192.168.65.11"},
		},
	}
	return state, svc, port
}

// routeTestStateV6 mirrors routeTestState with an IPv6 EndpointSlice and
// dual-stack node addresses, for the v6 routing/tunnel tests.
func routeTestStateV6() (clusterState, serviceItem, servicePort) {
	svc := serviceItem{}
	svc.Metadata.Namespace = "default"
	svc.Metadata.Name = "upload"
	port := servicePort{
		Port:       8080,
		NodePort:   32080,
		TargetPort: intOrString{Set: true, IntVal: 8080},
	}

	ready := true
	epPort := 8080
	slice := endpointSliceItem{}
	slice.Metadata.Namespace = "default"
	slice.Metadata.Labels = map[string]string{"kubernetes.io/service-name": "upload"}
	slice.AddressType = "IPv6"
	slice.Ports = []endpointPort{{Port: &epPort}}
	slice.Endpoints = []endpointEntry{
		{Addresses: []string{"fd00:1::10"}, NodeName: "node-b"},
	}
	slice.Endpoints[0].Conditions.Ready = &ready

	state := clusterState{
		endpointSlices: []endpointSliceItem{slice},
		nodeIPs: map[string]nodeAddrs{
			"node-a": {v4: "192.168.65.10", v6: "fd00:65::10"},
			"node-b": {v4: "192.168.65.11", v6: "fd00:65::11"},
		},
	}
	return state, svc, port
}

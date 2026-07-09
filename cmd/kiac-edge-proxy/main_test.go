package main

import "testing"

func TestRouteForPrefersLocalEndpoint(t *testing.T) {
	state, svc, port := routeTestState()

	route := state.routeFor("node-a", "15080", svc, port)
	if route.dial != "10.0.0.10:8080" || route.tunnelTarget != "" {
		t.Fatalf("route = %+v, want direct local endpoint", route)
	}
}

func TestRouteForTunnelsToRemoteEndpointNode(t *testing.T) {
	state, svc, port := routeTestState()
	state.endpointSlices[0].Endpoints = state.endpointSlices[0].Endpoints[1:]

	route := state.routeFor("node-c", "15080", svc, port)
	if route.dial != "192.168.65.11:15080" || route.tunnelTarget != "10.0.1.10:8080" {
		t.Fatalf("route = %+v, want tunnel to node-b proxy for remote endpoint", route)
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
		nodeIPs: map[string]string{
			"node-a": "192.168.65.10",
			"node-b": "192.168.65.11",
		},
	}
	return state, svc, port
}

package main

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

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

func TestTunnelHeaderRequiresTokenAndValidTarget(t *testing.T) {
	token := strings.Repeat("a", 64)
	target := "10.0.0.10:8080"
	valid := tunnelProtocol + " " + token + " " + target + "\n"

	if got, ok := authenticateTunnelHeader(valid, token); !ok || got != target {
		t.Fatalf("valid header = %q, %v", got, ok)
	}
	for _, header := range []string{
		"KIACEDGE/1 " + target + "\n",
		tunnelProtocol + " wrong-token " + target + "\n",
		tunnelProtocol + " " + token + " localhost:8080\n",
		tunnelProtocol + " " + token + " " + target + " extra\n",
	} {
		if _, ok := authenticateTunnelHeader(header, token); ok {
			t.Errorf("accepted invalid tunnel header %q", header)
		}
	}
}

func TestTunnelAllowlistContainsOnlyLocalReadyEndpoints(t *testing.T) {
	state, svc, port := routeTestState()
	routes := routeTable{allowedTargets: map[string]struct{}{}}
	allowLocalTargets(routes, state.matchingEndpoints(svc, port, false), "node-a")

	app := &proxyApp{routes: routes}
	if !app.tunnelTargetAllowed("10.0.0.10:8080") {
		t.Fatal("local endpoint should be an allowed tunnel target")
	}
	if app.tunnelTargetAllowed("10.0.1.10:8080") {
		t.Fatal("endpoint on another node must not be an allowed tunnel target")
	}
	if app.tunnelTargetAllowed("169.254.169.254:80") {
		t.Fatal("arbitrary address must not be an allowed tunnel target")
	}
}

func TestProxyTCPPreservesResponseAfterClientHalfClose(t *testing.T) {
	testClient, proxyClient := tcpPair(t)
	proxyBackend, testBackend := tcpPair(t)
	for _, conn := range []*net.TCPConn{testClient, proxyClient, proxyBackend, testBackend} {
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer proxyClient.Close()
		defer proxyBackend.Close()
		proxyTCP(proxyClient, proxyBackend, proxyClient)
	}()

	backendErr := make(chan error, 1)
	go func() {
		request, err := io.ReadAll(testBackend)
		if err != nil {
			backendErr <- err
			return
		}
		if string(request) != "request-body" {
			backendErr <- &unexpectedPayload{got: string(request), want: "request-body"}
			return
		}
		// The proxy must remain alive after the request half closes so a
		// delayed response can still travel in the reverse direction.
		time.Sleep(50 * time.Millisecond)
		if _, err := testBackend.Write([]byte("response-body")); err != nil {
			backendErr <- err
			return
		}
		if err := testBackend.CloseWrite(); err != nil {
			backendErr <- err
			return
		}
		backendErr <- nil
	}()

	if _, err := testClient.Write([]byte("request-body")); err != nil {
		t.Fatal(err)
	}
	if err := testClient.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(testClient)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != "response-body" {
		t.Fatalf("response = %q, want response-body", response)
	}
	if err := <-backendErr; err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("proxy did not finish after both directions closed")
	}
}

type unexpectedPayload struct {
	got  string
	want string
}

func (e *unexpectedPayload) Error() string {
	return "payload = " + e.got + ", want " + e.want
}

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	dialed, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := ln.Accept()
	if err != nil {
		dialed.Close()
		t.Fatal(err)
	}
	return dialed.(*net.TCPConn), accepted.(*net.TCPConn)
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

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type proxyApp struct {
	listenAddr    string
	proxyPort     string
	nodePortRange string
	backendMSS    int
	kubeconfig    string
	iptables      string
	kubectl       []string
	lastSnapshot  string
	routeMu       sync.RWMutex
	routes        routeTable
	nodeName      string
}

type routeTable struct {
	loadBalancers map[string]backendRoute
	nodePorts     map[int]backendRoute
}

type backendRoute struct {
	dial         string
	tunnelTarget string
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	app := &proxyApp{}
	flag.StringVar(&app.listenAddr, "listen", "0.0.0.0:15080", "TCP address to receive redirected Service traffic")
	flag.StringVar(&app.proxyPort, "proxy-port", "15080", "local redirect port")
	flag.StringVar(&app.nodePortRange, "nodeport-range", "30000:32767", "NodePort range to redirect")
	flag.IntVar(&app.backendMSS, "backend-mss", 1200, "TCP_MAXSEG value for backend connections; 0 disables MSS clamping")
	flag.StringVar(&app.kubeconfig, "kubeconfig", "", "kubeconfig path for polling LoadBalancer Services")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.run(ctx); err != nil {
		log.Fatal(err)
	}
}

func (a *proxyApp) run(ctx context.Context) error {
	relinkLegacyIptables()

	ipt, err := lookPath("iptables-legacy", "iptables")
	if err != nil {
		return err
	}
	a.iptables = ipt

	a.kubectl = []string{"kubectl"}
	if _, err := exec.LookPath("kubectl"); err != nil {
		a.kubectl = []string{"k3s", "kubectl"}
	}
	a.nodeName, _ = os.Hostname()

	if err := a.resetHook(); err != nil {
		return err
	}
	defer a.cleanup()

	a.lastSnapshot = "__kiac_unset__"
	if err := a.syncRules(); err != nil {
		return err
	}
	go a.syncLoop(ctx)

	return a.serve(ctx)
}

func relinkLegacyIptables() {
	if _, err := os.Stat("/bin/aux/xtables-legacy-multi"); err != nil {
		return
	}
	for _, name := range []string{"iptables", "iptables-save", "iptables-restore", "ip6tables", "ip6tables-save", "ip6tables-restore"} {
		path := "/bin/aux/" + name
		_ = os.Remove(path)
		_ = os.Symlink("xtables-legacy-multi", path)
	}
}

func lookPath(names ...string) (string, error) {
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("none of %s found on PATH", strings.Join(names, ", "))
}

func (a *proxyApp) serve(ctx context.Context) error {
	ln, err := net.Listen("tcp4", a.listenAddr)
	if err != nil {
		return err
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	log.Printf("listening on %s", a.listenAddr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || isClosedNetErr(err) {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				log.Printf("accept: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return err
		}
		tcp, ok := conn.(*net.TCPConn)
		if !ok {
			_ = conn.Close()
			continue
		}
		go a.handleConn(tcp)
	}
}

func (a *proxyApp) handleConn(client *net.TCPConn) {
	defer client.Close()

	dst, err := originalDst(client)
	if err != nil {
		log.Printf("original dst: %v", err)
		return
	}
	if isProxyPort(dst, a.proxyPort) {
		a.handleTunnel(client)
		return
	}

	route := a.backendRoute(dst)
	if route.dial == "" {
		route.dial = dst
	}
	backendConn, err := a.dialBackend(route.dial)
	if err != nil {
		log.Printf("dial %s: %v", route.dial, err)
		return
	}
	backend := backendConn.(*net.TCPConn)
	defer backend.Close()

	if route.tunnelTarget != "" {
		if _, err := fmt.Fprintf(backend, "KIACEDGE/1 %s\n", route.tunnelTarget); err != nil {
			log.Printf("tunnel header to %s for %s: %v", route.dial, route.tunnelTarget, err)
			return
		}
	}
	proxyTCP(client, backend, client)
}

func (a *proxyApp) handleTunnel(client *net.TCPConn) {
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Printf("tunnel deadline: %v", err)
		return
	}
	br := bufio.NewReader(client)
	line, err := br.ReadString('\n')
	_ = client.SetReadDeadline(time.Time{})
	if err != nil {
		log.Printf("tunnel header: %v", err)
		return
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(line), "KIACEDGE/1 ")
	if !ok || !validBackendTarget(target) {
		log.Printf("invalid tunnel header %q", strings.TrimSpace(line))
		return
	}
	backendConn, err := a.dialBackend(target)
	if err != nil {
		log.Printf("tunnel dial %s: %v", target, err)
		return
	}
	backend := backendConn.(*net.TCPConn)
	defer backend.Close()
	proxyTCP(client, backend, br)
}

func (a *proxyApp) dialBackend(dst string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			if a.backendMSS <= 0 {
				return nil
			}
			return setTCPMaxSeg(c, a.backendMSS)
		},
	}
	return dialer.Dial("tcp4", dst)
}

func isProxyPort(dst, proxyPort string) bool {
	_, port, err := net.SplitHostPort(dst)
	return err == nil && port == proxyPort
}

func validBackendTarget(target string) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil || net.ParseIP(host).To4() == nil {
		return false
	}
	p, err := strconv.Atoi(port)
	return err == nil && p > 0 && p <= 65535
}

func (a *proxyApp) backendRoute(original string) backendRoute {
	a.routeMu.RLock()
	defer a.routeMu.RUnlock()
	if route := a.routes.loadBalancers[original]; route.dial != "" {
		return route
	}
	_, port, err := net.SplitHostPort(original)
	if err != nil {
		return backendRoute{}
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return backendRoute{}
	}
	return a.routes.nodePorts[p]
}

func (a *proxyApp) setRoutes(routes routeTable) {
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	a.routes = routes
}

func proxyTCP(client, backend *net.TCPConn, clientReader io.Reader) {
	errc := make(chan error, 2)
	go copyTCP(backend, clientReader, func() {
		_ = backend.CloseWrite()
		_ = client.CloseRead()
	}, errc)
	go copyTCP(client, backend, func() {
		_ = client.CloseWrite()
		_ = backend.CloseRead()
	}, errc)
	<-errc
}

func copyTCP(dst *net.TCPConn, src io.Reader, done func(), errc chan<- error) {
	_, err := io.Copy(dst, src)
	done()
	errc <- err
}

func isClosedNetErr(err error) bool {
	return strings.Contains(err.Error(), "use of closed network connection")
}

func (a *proxyApp) syncLoop(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.syncRules(); err != nil {
				log.Printf("sync rules: %v", err)
			}
		}
	}
}

func (a *proxyApp) ipt(args ...string) error {
	cmdArgs := append([]string{"-w", "-t", "nat"}, args...)
	out, err := exec.Command(a.iptables, cmdArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *proxyApp) iptOK(args ...string) bool {
	return a.ipt(args...) == nil
}

func (a *proxyApp) resetHook() error {
	_ = a.ipt("-N", "KIAC-EDGE")
	_ = a.ipt("-N", "KIAC-EDGE-OUTPUT")
	for a.ipt("-D", "PREROUTING", "-j", "KIAC-EDGE") == nil {
	}
	for a.ipt("-D", "OUTPUT", "-j", "KIAC-EDGE-OUTPUT") == nil {
	}
	if err := a.ipt("-I", "PREROUTING", "1", "-j", "KIAC-EDGE"); err != nil {
		return err
	}
	return a.ipt("-I", "OUTPUT", "1", "-j", "KIAC-EDGE-OUTPUT")
}

func (a *proxyApp) ensureHook() error {
	_ = a.ipt("-N", "KIAC-EDGE")
	_ = a.ipt("-N", "KIAC-EDGE-OUTPUT")
	if !a.iptOK("-C", "PREROUTING", "-j", "KIAC-EDGE") {
		if err := a.ipt("-I", "PREROUTING", "1", "-j", "KIAC-EDGE"); err != nil {
			return err
		}
	}
	if !a.iptOK("-C", "OUTPUT", "-j", "KIAC-EDGE-OUTPUT") {
		if err := a.ipt("-I", "OUTPUT", "1", "-j", "KIAC-EDGE-OUTPUT"); err != nil {
			return err
		}
	}
	return nil
}

func (a *proxyApp) cleanup() {
	for a.ipt("-D", "PREROUTING", "-j", "KIAC-EDGE") == nil {
	}
	for a.ipt("-D", "OUTPUT", "-j", "KIAC-EDGE-OUTPUT") == nil {
	}
	_ = a.ipt("-F", "KIAC-EDGE")
	_ = a.ipt("-X", "KIAC-EDGE")
	_ = a.ipt("-F", "KIAC-EDGE-OUTPUT")
	_ = a.ipt("-X", "KIAC-EDGE-OUTPUT")
}

func (a *proxyApp) syncRules() error {
	if err := a.ensureHook(); err != nil {
		return err
	}
	snapshot := a.clusterSnapshot()
	if snapshot == a.lastSnapshot && !a.rulesMissing() {
		return nil
	}
	if err := a.ipt("-F", "KIAC-EDGE"); err != nil {
		return err
	}
	if err := a.ipt("-F", "KIAC-EDGE-OUTPUT"); err != nil {
		return err
	}
	if err := a.ipt("-A", "KIAC-EDGE", "-p", "tcp", "--dport", a.proxyPort, "-j", "RETURN"); err != nil {
		return err
	}
	if err := a.ipt("-A", "KIAC-EDGE-OUTPUT", "-p", "tcp", "--dport", a.proxyPort, "-j", "RETURN"); err != nil {
		return err
	}
	if err := a.ipt("-A", "KIAC-EDGE", "-p", "tcp", "-m", "addrtype", "--dst-type", "LOCAL", "--dport", a.nodePortRange, "-j", "REDIRECT", "--to-ports", a.proxyPort); err != nil {
		return err
	}
	routes := routeTable{
		loadBalancers: map[string]backendRoute{},
		nodePorts:     map[int]backendRoute{},
	}
	if snapshot != "" {
		if err := a.addServiceRules(snapshot, routes); err != nil {
			log.Printf("service rules: %v", err)
		}
	}
	a.setRoutes(routes)
	a.lastSnapshot = snapshot
	return nil
}

func (a *proxyApp) rulesMissing() bool {
	return !a.iptOK("-C", "KIAC-EDGE", "-p", "tcp", "--dport", a.proxyPort, "-j", "RETURN") ||
		!a.iptOK("-C", "KIAC-EDGE-OUTPUT", "-p", "tcp", "--dport", a.proxyPort, "-j", "RETURN") ||
		!a.iptOK("-C", "KIAC-EDGE", "-p", "tcp", "-m", "addrtype", "--dst-type", "LOCAL", "--dport", a.nodePortRange, "-j", "REDIRECT", "--to-ports", a.proxyPort)
}

func (a *proxyApp) clusterSnapshot() string {
	args := append([]string{}, a.kubectl...)
	if a.kubeconfig != "" {
		args = append(args, "--kubeconfig", a.kubeconfig)
	}
	args = append(args, "get", "svc,endpointslice,nodes", "-A", "-o", "json")
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		log.Printf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		return ""
	}
	return string(out)
}

type kubeList struct {
	Items []json.RawMessage `json:"items"`
}

type kubeKind struct {
	Kind string `json:"kind"`
}

type objectMeta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
}

type serviceItem struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Type  string        `json:"type"`
		Ports []servicePort `json:"ports"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP string `json:"ip"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

type servicePort struct {
	Name       string      `json:"name"`
	Port       int         `json:"port"`
	NodePort   int         `json:"nodePort"`
	TargetPort intOrString `json:"targetPort"`
}

type intOrString struct {
	IntVal int
	StrVal string
	Set    bool
}

func (v *intOrString) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" || len(raw) == 0 {
		return nil
	}
	v.Set = true
	if raw[0] == '"' {
		return json.Unmarshal(raw, &v.StrVal)
	}
	return json.Unmarshal(raw, &v.IntVal)
}

type endpointSliceItem struct {
	Metadata  objectMeta      `json:"metadata"`
	Ports     []endpointPort  `json:"ports"`
	Endpoints []endpointEntry `json:"endpoints"`
}

type endpointPort struct {
	Name     *string `json:"name"`
	Port     *int    `json:"port"`
	Protocol *string `json:"protocol"`
}

type endpointEntry struct {
	Addresses  []string `json:"addresses"`
	NodeName   string   `json:"nodeName"`
	Conditions struct {
		Ready *bool `json:"ready"`
	} `json:"conditions"`
}

type nodeItem struct {
	Metadata objectMeta `json:"metadata"`
	Status   struct {
		Addresses []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
	} `json:"status"`
}

type clusterState struct {
	services       []serviceItem
	endpointSlices []endpointSliceItem
	nodeIPs        map[string]string
}

type endpointTarget struct {
	address  string
	nodeName string
}

func (a *proxyApp) addServiceRules(raw string, routes routeTable) error {
	state, err := parseClusterState(raw)
	if err != nil {
		return err
	}
	for _, svc := range state.services {
		for _, port := range svc.Spec.Ports {
			if port.NodePort > 0 {
				routes.nodePorts[port.NodePort] = state.routeFor(a.nodeName, a.proxyPort, svc, port)
			}
		}
		if svc.Spec.Type != "LoadBalancer" {
			continue
		}
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if net.ParseIP(ingress.IP).To4() == nil {
				continue
			}
			for _, port := range svc.Spec.Ports {
				if port.Port <= 0 || strconv.Itoa(port.Port) == a.proxyPort {
					continue
				}
				if err := a.ipt("-A", "KIAC-EDGE", "-p", "tcp", "-d", ingress.IP, "--dport", strconv.Itoa(port.Port), "-j", "REDIRECT", "--to-ports", a.proxyPort); err != nil {
					return err
				}
				if err := a.ipt("-A", "KIAC-EDGE-OUTPUT", "-p", "tcp", "-d", ingress.IP, "--dport", strconv.Itoa(port.Port), "-j", "REDIRECT", "--to-ports", a.proxyPort); err != nil {
					return err
				}
				route := state.routeFor(a.nodeName, a.proxyPort, svc, port)
				if route.dial == "" && port.NodePort > 0 {
					route.dial = net.JoinHostPort(ingress.IP, strconv.Itoa(port.NodePort))
				}
				routes.loadBalancers[net.JoinHostPort(ingress.IP, strconv.Itoa(port.Port))] = route
			}
		}
	}
	return nil
}

func parseClusterState(raw string) (clusterState, error) {
	state := clusterState{nodeIPs: map[string]string{}}
	var list kubeList
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return state, err
	}
	for _, item := range list.Items {
		var kind kubeKind
		if err := json.Unmarshal(item, &kind); err != nil {
			return state, err
		}
		switch kind.Kind {
		case "Service":
			var svc serviceItem
			if err := json.Unmarshal(item, &svc); err != nil {
				return state, err
			}
			state.services = append(state.services, svc)
		case "EndpointSlice":
			var slice endpointSliceItem
			if err := json.Unmarshal(item, &slice); err != nil {
				return state, err
			}
			state.endpointSlices = append(state.endpointSlices, slice)
		case "Node":
			var node nodeItem
			if err := json.Unmarshal(item, &node); err != nil {
				return state, err
			}
			for _, addr := range node.Status.Addresses {
				if addr.Type == "InternalIP" && net.ParseIP(addr.Address).To4() != nil {
					state.nodeIPs[node.Metadata.Name] = addr.Address
					break
				}
			}
		}
	}
	return state, nil
}

func (s clusterState) routeFor(localNode, proxyPort string, svc serviceItem, port servicePort) backendRoute {
	endpoints := s.matchingEndpoints(svc, port)
	for _, ep := range endpoints {
		if ep.nodeName == "" || ep.nodeName == localNode {
			return backendRoute{dial: ep.address}
		}
	}
	for _, ep := range endpoints {
		if nodeIP := s.nodeIPs[ep.nodeName]; nodeIP != "" {
			return backendRoute{
				dial:         net.JoinHostPort(nodeIP, proxyPort),
				tunnelTarget: ep.address,
			}
		}
	}
	if len(endpoints) > 0 {
		return backendRoute{dial: endpoints[0].address}
	}
	return backendRoute{}
}

func (s clusterState) matchingEndpoints(svc serviceItem, port servicePort) []endpointTarget {
	var out []endpointTarget
	for _, slice := range s.endpointSlices {
		if slice.Metadata.Namespace != svc.Metadata.Namespace ||
			slice.Metadata.Labels["kubernetes.io/service-name"] != svc.Metadata.Name {
			continue
		}
		for _, epPort := range slice.Ports {
			if epPort.Port == nil || !servicePortMatchesEndpoint(port, epPort) {
				continue
			}
			for _, ep := range slice.Endpoints {
				if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
					continue
				}
				for _, addr := range ep.Addresses {
					if net.ParseIP(addr).To4() == nil {
						continue
					}
					out = append(out, endpointTarget{
						address:  net.JoinHostPort(addr, strconv.Itoa(*epPort.Port)),
						nodeName: ep.NodeName,
					})
				}
			}
		}
	}
	return out
}

func servicePortMatchesEndpoint(svcPort servicePort, epPort endpointPort) bool {
	if epPort.Protocol != nil && *epPort.Protocol != "" && *epPort.Protocol != "TCP" {
		return false
	}
	if svcPort.TargetPort.Set {
		if svcPort.TargetPort.StrVal != "" {
			return epPort.Name != nil && *epPort.Name == svcPort.TargetPort.StrVal
		}
		return epPort.Port != nil && *epPort.Port == svcPort.TargetPort.IntVal
	}
	return epPort.Port != nil && *epPort.Port == svcPort.Port
}

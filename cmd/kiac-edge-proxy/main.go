package main

import (
	"bufio"
	"context"
	"crypto/subtle"
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
	listenAddr6   string
	proxyPort     string
	nodePortRange string
	backendMSS    int
	kubeconfig    string
	tokenFile     string
	iptables      string
	ip6tables     string // "" when the node kernel has no IPv6 netfilter
	kubectl       []string
	lastSnapshot  string
	routeMu       sync.RWMutex
	routes        routeTable
	nodeName      string
	tunnelToken   string
}

type routeTable struct {
	loadBalancers  map[string]backendRoute // key: "host:port" (v6 host bracketed)
	nodePorts      map[nodePortKey]backendRoute
	allowedTargets map[string]struct{}
}

// nodePortKey routes a NodePort per family: a dual-stack Service is
// reached over both v4 and v6, and each family may resolve to a
// different endpoint or tunnel target.
type nodePortKey struct {
	port int
	v6   bool
}

type backendRoute struct {
	dial         string
	tunnelTarget string
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	app := &proxyApp{}
	flag.StringVar(&app.listenAddr, "listen", "0.0.0.0:15080", "IPv4 TCP address to receive redirected Service traffic")
	flag.StringVar(&app.listenAddr6, "listen6", "[::]:15080", "IPv6 TCP address to receive redirected Service traffic")
	flag.StringVar(&app.proxyPort, "proxy-port", "15080", "local redirect port")
	flag.StringVar(&app.nodePortRange, "nodeport-range", "30000:32767", "NodePort range to redirect")
	flag.IntVar(&app.backendMSS, "backend-mss", 1200, "TCP_MAXSEG value for backend connections; 0 disables MSS clamping")
	flag.StringVar(&app.kubeconfig, "kubeconfig", "", "kubeconfig path for polling LoadBalancer Services")
	flag.StringVar(&app.tokenFile, "token-file", "", "root-readable file containing the inter-node tunnel token")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.run(ctx); err != nil {
		log.Fatal(err)
	}
}

func (a *proxyApp) run(ctx context.Context) error {
	token, err := readTunnelToken(a.tokenFile)
	if err != nil {
		return err
	}
	a.tunnelToken = token

	relinkLegacyIptables()

	ipt, err := lookPath("iptables-legacy", "iptables")
	if err != nil {
		return err
	}
	a.iptables = ipt
	// IPv6 is best-effort: on a stock (IPv4-only) kernel ip6tables cannot
	// initialize its tables, so a dual-stack cluster is not possible and
	// there is nothing to intercept. Detect that and run IPv4-only rather
	// than failing, so the proxy still fixes v4 uploads there.
	if ip6t, err := lookPath("ip6tables-legacy", "ip6tables"); err == nil {
		if ip6tablesUsable(ip6t) {
			a.ip6tables = ip6t
		} else {
			log.Printf("ip6tables has no usable nat table (IPv4-only kernel); running IPv4-only")
		}
	}

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

// ip6tablesUsable reports whether the IPv6 nat table initializes. On a
// kernel without IPv6 netfilter the first table access fails ("Table
// does not exist"), which is exactly issue #10's symptom on the stock
// kernel; the full kernel initializes it.
func ip6tablesUsable(bin string) bool {
	return exec.Command(bin, "-w", "-t", "nat", "-L", "-n").Run() == nil
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
	errc := make(chan error, 2)
	var wg sync.WaitGroup

	start := func(network, addr string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.listenAndServe(ctx, network, addr); err != nil {
				errc <- err
			}
		}()
	}
	start("tcp4", a.listenAddr)
	if a.ip6tables != "" {
		start("tcp6", a.listenAddr6)
	}

	go func() { wg.Wait(); close(errc) }()
	// Return the first listener error; ctx cancellation closes listeners
	// and the loops return nil, so errc closing with no value is success.
	if err, ok := <-errc; ok {
		return err
	}
	return nil
}

func (a *proxyApp) listenAndServe(ctx context.Context, network, addr string) error {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	log.Printf("listening on %s (%s)", addr, network)
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

	v6 := connIsV6(client)
	dst, err := originalDst(client, v6)
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
		if _, err := fmt.Fprintf(backend, "%s %s %s\n", tunnelProtocol, a.tunnelToken, route.tunnelTarget); err != nil {
			log.Printf("tunnel header to %s for %s: %v", route.dial, route.tunnelTarget, err)
			return
		}
	}
	proxyTCP(client, backend, client)
}

// connIsV6 reports whether the accepted connection arrived over IPv6.
// The listeners are family-specific (tcp4/tcp6), so the remote address
// family is authoritative.
func connIsV6(c *net.TCPConn) bool {
	ra, ok := c.RemoteAddr().(*net.TCPAddr)
	return ok && ra.IP.To4() == nil
}

func (a *proxyApp) handleTunnel(client *net.TCPConn) {
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Printf("tunnel deadline: %v", err)
		return
	}
	br := bufio.NewReaderSize(client, 512)
	line, err := br.ReadSlice('\n')
	_ = client.SetReadDeadline(time.Time{})
	if err != nil {
		log.Printf("tunnel header: %v", err)
		return
	}
	target, ok := authenticateTunnelHeader(string(line), a.tunnelToken)
	if !ok {
		log.Printf("rejected unauthenticated tunnel request")
		return
	}
	if !a.tunnelTargetAllowed(target) {
		log.Printf("rejected tunnel target not local to a ready Service endpoint")
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
	// "tcp" so the same dial path serves IPv4 and IPv6 backends; the MSS
	// clamp is set on the socket regardless of family.
	return dialer.Dial("tcp", dst)
}

func isProxyPort(dst, proxyPort string) bool {
	_, port, err := net.SplitHostPort(dst)
	return err == nil && port == proxyPort
}

func validBackendTarget(target string) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil || net.ParseIP(host) == nil {
		return false
	}
	p, err := strconv.Atoi(port)
	return err == nil && p > 0 && p <= 65535
}

const tunnelProtocol = "KIACEDGE/2"

func readTunnelToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("--token-file is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading tunnel token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("tunnel token must contain at least 32 non-whitespace characters")
	}
	return token, nil
}

func authenticateTunnelHeader(line, token string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != tunnelProtocol ||
		subtle.ConstantTimeCompare([]byte(fields[1]), []byte(token)) != 1 ||
		!validBackendTarget(fields[2]) {
		return "", false
	}
	return fields[2], true
}

func (a *proxyApp) backendRoute(original string) backendRoute {
	a.routeMu.RLock()
	defer a.routeMu.RUnlock()
	if route := a.routes.loadBalancers[original]; route.dial != "" {
		return route
	}
	host, port, err := net.SplitHostPort(original)
	if err != nil {
		return backendRoute{}
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return backendRoute{}
	}
	return a.routes.nodePorts[nodePortKey{port: p, v6: isV6Host(host)}]
}

func (a *proxyApp) setRoutes(routes routeTable) {
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	a.routes = routes
}

func (a *proxyApp) tunnelTargetAllowed(target string) bool {
	a.routeMu.RLock()
	defer a.routeMu.RUnlock()
	_, ok := a.routes.allowedTargets[target]
	return ok
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
	// A clean EOF is a TCP half-close: keep the opposite direction alive
	// so request bodies can be followed by delayed responses. On a real
	// copy error, force both goroutines out rather than waiting forever on
	// a peer that may never close its remaining half.
	if err := <-errc; err != nil {
		now := time.Now()
		_ = client.SetDeadline(now)
		_ = backend.SetDeadline(now)
	}
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

// families returns the iptables binaries to program: always IPv4, plus
// IPv6 when the kernel supports it.
func (a *proxyApp) families() []string {
	bins := []string{a.iptables}
	if a.ip6tables != "" {
		bins = append(bins, a.ip6tables)
	}
	return bins
}

// binFor returns the iptables binary for an address family.
func (a *proxyApp) binFor(v6 bool) string {
	if v6 {
		return a.ip6tables
	}
	return a.iptables
}

// ipt runs one nat-table command against a specific xtables binary.
func (a *proxyApp) ipt(bin string, args ...string) error {
	cmdArgs := append([]string{"-w", "-t", "nat"}, args...)
	out, err := exec.Command(bin, cmdArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *proxyApp) iptOK(bin string, args ...string) bool {
	return a.ipt(bin, args...) == nil
}

func (a *proxyApp) resetHook() error {
	for _, bin := range a.families() {
		_ = a.ipt(bin, "-N", "KIAC-EDGE")
		_ = a.ipt(bin, "-N", "KIAC-EDGE-OUTPUT")
		for a.ipt(bin, "-D", "PREROUTING", "-j", "KIAC-EDGE") == nil {
		}
		for a.ipt(bin, "-D", "OUTPUT", "-j", "KIAC-EDGE-OUTPUT") == nil {
		}
		if err := a.ipt(bin, "-I", "PREROUTING", "1", "-j", "KIAC-EDGE"); err != nil {
			return err
		}
		if err := a.ipt(bin, "-I", "OUTPUT", "1", "-j", "KIAC-EDGE-OUTPUT"); err != nil {
			return err
		}
	}
	return nil
}

func (a *proxyApp) ensureHook() error {
	for _, bin := range a.families() {
		_ = a.ipt(bin, "-N", "KIAC-EDGE")
		_ = a.ipt(bin, "-N", "KIAC-EDGE-OUTPUT")
		if !a.iptOK(bin, "-C", "PREROUTING", "-j", "KIAC-EDGE") {
			if err := a.ipt(bin, "-I", "PREROUTING", "1", "-j", "KIAC-EDGE"); err != nil {
				return err
			}
		}
		if !a.iptOK(bin, "-C", "OUTPUT", "-j", "KIAC-EDGE-OUTPUT") {
			if err := a.ipt(bin, "-I", "OUTPUT", "1", "-j", "KIAC-EDGE-OUTPUT"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *proxyApp) cleanup() {
	for _, bin := range a.families() {
		for a.ipt(bin, "-D", "PREROUTING", "-j", "KIAC-EDGE") == nil {
		}
		for a.ipt(bin, "-D", "OUTPUT", "-j", "KIAC-EDGE-OUTPUT") == nil {
		}
		_ = a.ipt(bin, "-F", "KIAC-EDGE")
		_ = a.ipt(bin, "-X", "KIAC-EDGE")
		_ = a.ipt(bin, "-F", "KIAC-EDGE-OUTPUT")
		_ = a.ipt(bin, "-X", "KIAC-EDGE-OUTPUT")
	}
}

func (a *proxyApp) syncRules() error {
	if err := a.ensureHook(); err != nil {
		return err
	}
	snapshot := a.clusterSnapshot()
	if snapshot == a.lastSnapshot && !a.rulesMissing() {
		return nil
	}
	// Base rules per family: skip the proxy's own port, then redirect the
	// NodePort range destined to a local address into the proxy.
	for _, bin := range a.families() {
		if err := a.ipt(bin, "-F", "KIAC-EDGE"); err != nil {
			return err
		}
		if err := a.ipt(bin, "-F", "KIAC-EDGE-OUTPUT"); err != nil {
			return err
		}
		if err := a.ipt(bin, "-A", "KIAC-EDGE", "-p", "tcp", "--dport", a.proxyPort, "-j", "RETURN"); err != nil {
			return err
		}
		if err := a.ipt(bin, "-A", "KIAC-EDGE-OUTPUT", "-p", "tcp", "--dport", a.proxyPort, "-j", "RETURN"); err != nil {
			return err
		}
		if err := a.ipt(bin, "-A", "KIAC-EDGE", "-p", "tcp", "-m", "addrtype", "--dst-type", "LOCAL", "--dport", a.nodePortRange, "-j", "REDIRECT", "--to-ports", a.proxyPort); err != nil {
			return err
		}
	}
	routes := routeTable{
		loadBalancers:  map[string]backendRoute{},
		nodePorts:      map[nodePortKey]backendRoute{},
		allowedTargets: map[string]struct{}{},
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
	for _, bin := range a.families() {
		if !a.iptOK(bin, "-C", "KIAC-EDGE", "-p", "tcp", "--dport", a.proxyPort, "-j", "RETURN") ||
			!a.iptOK(bin, "-C", "KIAC-EDGE-OUTPUT", "-p", "tcp", "--dport", a.proxyPort, "-j", "RETURN") ||
			!a.iptOK(bin, "-C", "KIAC-EDGE", "-p", "tcp", "-m", "addrtype", "--dst-type", "LOCAL", "--dport", a.nodePortRange, "-j", "REDIRECT", "--to-ports", a.proxyPort) {
			return true
		}
	}
	return false
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
	Metadata    objectMeta      `json:"metadata"`
	AddressType string          `json:"addressType"`
	Ports       []endpointPort  `json:"ports"`
	Endpoints   []endpointEntry `json:"endpoints"`
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

// nodeAddrs holds a node's InternalIP for each family, so a tunnel to a
// remote endpoint node uses the node address of the endpoint's family.
type nodeAddrs struct {
	v4 string
	v6 string
}

func (n nodeAddrs) forFamily(v6 bool) string {
	if v6 {
		return n.v6
	}
	return n.v4
}

type clusterState struct {
	services       []serviceItem
	endpointSlices []endpointSliceItem
	nodeIPs        map[string]nodeAddrs
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
			for _, v6 := range []bool{false, true} {
				if v6 && a.ip6tables == "" {
					continue
				}
				allowLocalTargets(routes, state.matchingEndpoints(svc, port, v6), a.nodeName)
			}
			if port.NodePort > 0 {
				// Route each family independently: a dual-stack Service is
				// reached over both, and the proxy must pick a same-family
				// endpoint (and same-family tunnel node IP).
				routes.nodePorts[nodePortKey{port: port.NodePort, v6: false}] =
					state.routeFor(a.nodeName, a.proxyPort, svc, port, false)
				if a.ip6tables != "" {
					routes.nodePorts[nodePortKey{port: port.NodePort, v6: true}] =
						state.routeFor(a.nodeName, a.proxyPort, svc, port, true)
				}
			}
		}
		if svc.Spec.Type != "LoadBalancer" {
			continue
		}
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			ip := net.ParseIP(ingress.IP)
			if ip == nil {
				continue
			}
			v6 := ip.To4() == nil
			if v6 && a.ip6tables == "" {
				continue // no IPv6 netfilter on this kernel
			}
			bin := a.binFor(v6)
			for _, port := range svc.Spec.Ports {
				if port.Port <= 0 || strconv.Itoa(port.Port) == a.proxyPort {
					continue
				}
				portStr := strconv.Itoa(port.Port)
				if err := a.ipt(bin, "-A", "KIAC-EDGE", "-p", "tcp", "-d", ingress.IP, "--dport", portStr, "-j", "REDIRECT", "--to-ports", a.proxyPort); err != nil {
					return err
				}
				if err := a.ipt(bin, "-A", "KIAC-EDGE-OUTPUT", "-p", "tcp", "-d", ingress.IP, "--dport", portStr, "-j", "REDIRECT", "--to-ports", a.proxyPort); err != nil {
					return err
				}
				route := state.routeFor(a.nodeName, a.proxyPort, svc, port, v6)
				if route.dial == "" && port.NodePort > 0 {
					route.dial = net.JoinHostPort(ingress.IP, strconv.Itoa(port.NodePort))
				}
				routes.loadBalancers[net.JoinHostPort(ingress.IP, portStr)] = route
			}
		}
	}
	return nil
}

func allowLocalTargets(routes routeTable, endpoints []endpointTarget, nodeName string) {
	for _, ep := range endpoints {
		if ep.nodeName == nodeName {
			routes.allowedTargets[ep.address] = struct{}{}
		}
	}
}

func parseClusterState(raw string) (clusterState, error) {
	state := clusterState{nodeIPs: map[string]nodeAddrs{}}
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
			addrs := state.nodeIPs[node.Metadata.Name]
			for _, addr := range node.Status.Addresses {
				if addr.Type != "InternalIP" {
					continue
				}
				ip := net.ParseIP(addr.Address)
				if ip == nil {
					continue
				}
				if ip.To4() != nil {
					if addrs.v4 == "" {
						addrs.v4 = addr.Address
					}
				} else if addrs.v6 == "" {
					addrs.v6 = addr.Address
				}
			}
			state.nodeIPs[node.Metadata.Name] = addrs
		}
	}
	return state, nil
}

func (s clusterState) routeFor(localNode, proxyPort string, svc serviceItem, port servicePort, v6 bool) backendRoute {
	endpoints := s.matchingEndpoints(svc, port, v6)
	for _, ep := range endpoints {
		if ep.nodeName == "" || ep.nodeName == localNode {
			return backendRoute{dial: ep.address}
		}
	}
	for _, ep := range endpoints {
		if nodeIP := s.nodeIPs[ep.nodeName].forFamily(v6); nodeIP != "" {
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

func (s clusterState) matchingEndpoints(svc serviceItem, port servicePort, v6 bool) []endpointTarget {
	var out []endpointTarget
	for _, slice := range s.endpointSlices {
		if slice.Metadata.Namespace != svc.Metadata.Namespace ||
			slice.Metadata.Labels["kubernetes.io/service-name"] != svc.Metadata.Name {
			continue
		}
		// EndpointSlices are per-family (addressType IPv4/IPv6); skip the
		// ones that do not match the family being routed.
		if slice.AddressType != "" && (slice.AddressType == "IPv6") != v6 {
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
					ip := net.ParseIP(addr)
					if ip == nil || (ip.To4() == nil) != v6 {
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

// isV6Host reports whether a bare host (no port) is an IPv6 literal.
func isV6Host(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() == nil
}

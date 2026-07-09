package main

import (
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
	lbMu          sync.RWMutex
	lbTargets     map[string]string
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

	backendDst := a.backendDst(dst)
	backendConn, err := a.dialBackend(backendDst)
	if err != nil {
		log.Printf("dial %s: %v", backendDst, err)
		return
	}
	backend := backendConn.(*net.TCPConn)
	defer backend.Close()

	errc := make(chan error, 2)
	go copyTCP(backend, client, errc)
	go copyTCP(client, backend, errc)
	<-errc
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

func (a *proxyApp) backendDst(original string) string {
	a.lbMu.RLock()
	defer a.lbMu.RUnlock()
	if dst := a.lbTargets[original]; dst != "" {
		return dst
	}
	return original
}

func (a *proxyApp) setLBTargets(targets map[string]string) {
	a.lbMu.Lock()
	defer a.lbMu.Unlock()
	a.lbTargets = targets
}

func copyTCP(dst, src *net.TCPConn, errc chan<- error) {
	_, err := io.Copy(dst, src)
	_ = dst.CloseWrite()
	_ = src.CloseRead()
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
	snapshot := a.serviceSnapshot()
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
	lbTargets := map[string]string{}
	if snapshot != "" {
		if err := a.addLoadBalancerRules(snapshot, lbTargets); err != nil {
			log.Printf("loadbalancer rules: %v", err)
		}
	}
	a.setLBTargets(lbTargets)
	a.lastSnapshot = snapshot
	return nil
}

func (a *proxyApp) rulesMissing() bool {
	return !a.iptOK("-C", "KIAC-EDGE", "-p", "tcp", "--dport", a.proxyPort, "-j", "RETURN") ||
		!a.iptOK("-C", "KIAC-EDGE-OUTPUT", "-p", "tcp", "--dport", a.proxyPort, "-j", "RETURN") ||
		!a.iptOK("-C", "KIAC-EDGE", "-p", "tcp", "-m", "addrtype", "--dst-type", "LOCAL", "--dport", a.nodePortRange, "-j", "REDIRECT", "--to-ports", a.proxyPort)
}

func (a *proxyApp) serviceSnapshot() string {
	args := append([]string{}, a.kubectl...)
	if a.kubeconfig != "" {
		args = append(args, "--kubeconfig", a.kubeconfig)
	}
	args = append(args, "get", "svc", "-A", "-o", "json")
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		log.Printf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		return ""
	}
	return string(out)
}

type serviceList struct {
	Items []struct {
		Spec struct {
			Type  string `json:"type"`
			Ports []struct {
				Port     int `json:"port"`
				NodePort int `json:"nodePort"`
			} `json:"ports"`
		} `json:"spec"`
		Status struct {
			LoadBalancer struct {
				Ingress []struct {
					IP string `json:"ip"`
				} `json:"ingress"`
			} `json:"loadBalancer"`
		} `json:"status"`
	} `json:"items"`
}

func (a *proxyApp) addLoadBalancerRules(raw string, targets map[string]string) error {
	var services serviceList
	if err := json.Unmarshal([]byte(raw), &services); err != nil {
		return err
	}
	for _, svc := range services.Items {
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
				if port.NodePort > 0 {
					targets[net.JoinHostPort(ingress.IP, strconv.Itoa(port.Port))] = net.JoinHostPort(ingress.IP, strconv.Itoa(port.NodePort))
				}
			}
		}
	}
	return nil
}

package cluster

import (
	"net"
	"time"

	"github.com/saiyam1814/kiac/pkg/ui"
)

// Create writes a kubeconfig that points the host's kubectl at the
// control-plane VM's IP on :6443. That endpoint is healthy inside the VM,
// but apple/container's vmnet can leave a freshly-booted node VM
// unreachable *from the Mac* (a stale ARP entry on lease reuse, or the
// same host-to-VM path that drops after a single-node restart). When that
// happens kubectl fails with "no route to host" even though the cluster
// is fine, so Create probes the endpoint before it tells the user to run
// kubectl and, on failure, prints how to use the cluster and how to
// restore host access instead of a hint that would only error out.
// See docs/docs/troubleshooting.html#host-api-unreachable and issue #6.

// hostReachAPI reports whether this Mac can open a TCP connection to the
// cluster's API server at ip:6443.
func hostReachAPI(ip string, timeout time.Duration) bool {
	return hostCanDial(net.JoinHostPort(ip, "6443"), timeout)
}

// hostCanDial retries a short TCP dial to addr until it succeeds or
// timeout elapses. vmnet ARP resolution can take a second or two to
// settle after a VM boots, so a single dial would report false too early.
func hostCanDial(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// warnAPIUnreachable prints the guidance shown when the API server is
// healthy inside the VM but the Mac cannot reach the control-plane VM IP.
// The cluster is real and fully usable from inside the VM; resetting
// vmnet (a full container-system restart) and resuming restores host
// access. name is the kiac cluster name, cp its control-plane container.
func warnAPIUnreachable(cp, name, ip string) {
	ui.Warnf("Your Mac cannot reach the API server at %s:6443, though it is healthy inside the VM.", ip)
	ui.Warnf("apple/container's vmnet did not make the control-plane VM host-reachable; the cluster itself is fine.")
	ui.Infof("use the cluster now, from inside the control-plane VM:")
	ui.Hintf("container exec %s kubectl get nodes", cp)
	ui.Infof("restore kubectl access from your Mac by resetting vmnet, then resuming:")
	ui.Hintf("container system stop && container system start")
	ui.Hintf("kiac resume cluster --name %s", name)
}

// warnGPUAPIUnreachable avoids prescribing an apple/container vmnet reset for
// a krunkit cluster. There is no public raw SSH command in Kiac, so verify and
// the bounded support bundle are the safe diagnostic entry points.
func warnGPUAPIUnreachable(name, ip string) {
	ui.Warnf("Your Mac cannot reach the API server at %s:6443, though it is healthy inside the VM.", ip)
	ui.Warnf("The krunkit/vmnet-helper cluster was left running; apple/container is not involved in this network path.")
	ui.Infof("inspect the internal API and VM services with:")
	ui.Hintf("kiac verify cluster --name %s", name)
	ui.Hintf("kiac support bundle --name %s", name)
}

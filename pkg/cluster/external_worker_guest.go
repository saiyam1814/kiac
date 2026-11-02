package cluster

import (
	"archive/tar"
	"bytes"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const externalWorkerGuestDir = "/etc/kiac/external-worker"

func externalWorkerWGConfig(w workerWireGuard) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Interface]\nPrivateKey = %s\n\n[Peer]\nPublicKey = %s\nEndpoint = %s\n", w.PrivateKey, w.PublicKey, w.Endpoint)
	if w.PresharedKey != "" {
		fmt.Fprintf(&b, "PresharedKey = %s\n", w.PresharedKey)
	}
	var routes []string
	for _, route := range w.AllowedIPs {
		routes = append(routes, route.String())
	}
	fmt.Fprintf(&b, "AllowedIPs = %s\nPersistentKeepalive = %d\n", strings.Join(routes, ", "), w.Keepalive)
	return b.String()
}

func externalWorkerNetworkScript(w workerWireGuard) string {
	var routes strings.Builder
	for _, route := range w.AllowedIPs {
		fmt.Fprintf(&routes, "ip -4 route add %s dev %s\n", shQuote(route.String()), externalWireGuardInterface)
	}
	return `#!/bin/sh
set -eu
case "$1" in
up)
  ip link add ` + externalWireGuardInterface + ` type wireguard
  trap 'ip link delete ` + externalWireGuardInterface + ` 2>/dev/null || true' EXIT
  wg setconf ` + externalWireGuardInterface + ` ` + externalWorkerGuestDir + `/wireguard.conf
  ip -4 address add ` + shQuote(w.Address) + ` dev ` + externalWireGuardInterface + `
  ip link set ` + externalWireGuardInterface + ` mtu ` + fmt.Sprint(w.MTU) + ` up
` + routes.String() + `  trap - EXIT
  ;;
down) ip link delete ` + externalWireGuardInterface + ` 2>/dev/null || true ;;
*) exit 2 ;;
esac
`
}

const externalWorkerNetworkUnit = `[Unit]
Description=Kiac external worker WireGuard network
Wants=network-online.target
After=network-online.target
Before=k3s.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/etc/kiac/external-worker/network.sh up
ExecStop=/etc/kiac/external-worker/network.sh down
TimeoutStartSec=20s
TimeoutStopSec=10s

[Install]
WantedBy=multi-user.target
`

func externalWorkerK3sUnit() string {
	return strings.Replace(k3sGPUUnit("agent"), "After=network-online.target", "After=network-online.target kiac-worker-network.service\nRequires=kiac-worker-network.service", 1)
}

func externalWorkerArchive(state ExternalWorkerState, p preparedExternalWorker) ([]byte, error) {
	config := map[string]any{
		"server": p.config.Server, "token-file": externalWorkerGuestDir + "/token",
		"node-name": state.VMName, "node-ip": state.NodeIP, "flannel-iface": externalWireGuardInterface,
		"node-label": []string{"kiac.dev/external-worker=true", "kiac.dev/worker-owner=" + state.OwnerID},
		"node-taint": []string{"kiac.dev/external-worker=true:NoSchedule"},
	}
	yamlConfig, err := yaml.Marshal(config)
	if err != nil {
		return nil, err
	}
	files := []struct {
		name, content string
		mode          int64
	}{
		{"token", p.token + "\n", 0o600},
		{"wireguard.conf", externalWorkerWGConfig(p.wg), 0o600},
		{"network.sh", externalWorkerNetworkScript(p.wg), 0o700},
		{"k3s.yaml", string(yamlConfig), 0o600},
		{"owner", state.OwnerID + "\n", 0o600},
		{"network.service", externalWorkerNetworkUnit, 0o600},
		{"k3s.service", externalWorkerK3sUnit(), 0o600},
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, file := range files {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.content)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(file.content)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return archive.Bytes(), nil
}

func (m *ExternalWorkerManager) installExternalWorker(state *ExternalWorkerState, p preparedExternalWorker) error {
	archive, err := externalWorkerArchive(*state, p)
	if err != nil {
		return workerFailure("private configuration preparation")
	}
	// The only secret transport is stdin. Even a command failure that echoes
	// stdin cannot put the token or key in a returned CommandError.
	if err := m.backend.ExecStdinTimeout(state.VMName, transferBudget(p.config.WaitTimeout), bytes.NewReader(archive),
		"sh", "-euc", "install -d -m 0700 "+externalWorkerGuestDir+"; tar -xf - -C "+externalWorkerGuestDir); err != nil {
		return workerFailure("private configuration transfer")
	}
	state.Phase = "joining"
	if err := m.write(*state); err != nil {
		return err
	}
	return m.startExternalWorkerAgent(*state)
}

func (m *ExternalWorkerManager) startExternalWorkerAgent(state ExternalWorkerState) error {
	if _, err := m.backend.ExecTimeout(state.VMName, 35*time.Second, "sh", "-euc", `
install -d -m 0700 /etc/rancher/k3s
install -m 0600 /etc/kiac/external-worker/k3s.yaml /etc/rancher/k3s/config.yaml
install -m 0644 /etc/kiac/external-worker/network.service /etc/systemd/system/kiac-worker-network.service
install -m 0644 /etc/kiac/external-worker/k3s.service /etc/systemd/system/k3s.service
systemctl daemon-reload
systemctl enable kiac-worker-network.service k3s.service
systemctl start kiac-worker-network.service
systemctl --no-block start k3s.service
`); err != nil {
		return workerFailure("network and agent startup")
	}
	return nil
}

func externalWorkerObserveScript(state ExternalWorkerState) string {
	return fmt.Sprintf(`import json, subprocess
def kubectl(*args):
    raw = subprocess.check_output(["/usr/local/bin/k3s", "kubectl", "--kubeconfig", "/var/lib/rancher/k3s/agent/kubelet.kubeconfig", "--request-timeout=8s", *args], timeout=10, stderr=subprocess.DEVNULL)
    return json.loads(raw)
node = kubectl("get", "node", %q, "-o", "json")
version = kubectl("get", "--raw", "/version")
with open("/var/lib/rancher/k3s/agent/etc/flannel/net-conf.json") as source:
    network = json.load(source)
with open("/proc/sys/kernel/random/boot_id") as source:
    guest_boot_id = source.read().strip()
ips = [a["address"] for a in node["status"].get("addresses", []) if a["type"] == "InternalIP"]
result = {"Name": node["metadata"]["name"], "UID": node["metadata"]["uid"],
    "OwnerID": node["metadata"].get("labels", {}).get("kiac.dev/worker-owner", ""),
    "NodeIP": %q if %q in ips else "",
    "KubeletVersion": node["status"]["nodeInfo"]["kubeletVersion"], "APIVersion": version["gitVersion"],
    "GuestBootID": guest_boot_id, "NodeBootID": node["status"]["nodeInfo"].get("bootID", ""),
    "PodNetwork": network["Network"], "Backend": network["Backend"]["Type"],
    "IPv4Only": network.get("EnableIPv4") is True and network.get("EnableIPv6") is False,
    "Tainted": any(t.get("key") == "kiac.dev/external-worker" and t.get("value") == "true" and t.get("effect") == "NoSchedule" for t in node.get("spec", {}).get("taints", [])),
    "Ready": any(c["type"] == "Ready" and c["status"] == "True" for c in node["status"].get("conditions", []))}
print(json.dumps(result))
`, state.VMName, state.NodeIP, state.NodeIP)
}

// Redact inside the guest before journal output crosses the management channel.
// Refuse log retrieval if either private configuration cannot be read.
const externalWorkerLogsScript = `import re, subprocess
with open("/etc/kiac/external-worker/token") as source:
    token = source.read().strip()
with open("/etc/kiac/external-worker/wireguard.conf") as source:
    wireguard = source.read()
credentials = token.partition("::")[2]
secrets = [token, credentials]
if credentials.partition(":")[0] in ("node", "server"):
    secrets.append(credentials.partition(":")[2])
else:
    secrets.append(credentials.partition(".")[2])
secrets += re.findall(r"^(?:PrivateKey|PresharedKey)\s*=\s*(\S+)", wireguard, re.M)
raw = subprocess.check_output(["journalctl", "--no-pager", "-n", "100", "-u", "k3s", "-u", "kiac-worker-network"], timeout=8, stderr=subprocess.DEVNULL).decode("utf-8", "replace")
for secret in sorted(set(secrets), key=len, reverse=True):
    if secret:
        raw = raw.replace(secret, "[redacted]")
raw = re.sub(r"K10[0-9a-fA-F]{64}::\S+", "[redacted]", raw)
print(raw, end="")
`

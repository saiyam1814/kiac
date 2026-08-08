package cluster

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/ui"
)

const (
	edgeProxyNodePath       = "/usr/local/bin/kiac-edge-proxy"
	edgeProxyKubeconfigPath = "/etc/kiac/kubeconfig"
	edgeProxyTokenPath      = "/etc/kiac/tunnel.token"
	edgeProxyLogPath        = "/var/log/kiac-edge-proxy.log"
	edgeProxySupervisorPID  = "/var/run/kiac-edge-proxy-supervisor.pid"
)

// The bug behind issue #8 is below Kubernetes: a sibling Apple Container
// VM can hand vmnet a TSO-deferred TCP stream that survives until
// kube-proxy DNATs/forwards it into a pod. The kiac edge proxy reads
// SO_ORIGINAL_DST, terminates that external TCP connection on the node,
// watches Services/EndpointSlices, and opens a fresh connection to a
// ready endpoint. If the selected endpoint lives on another node, the
// first proxy tunnels to that node's proxy so the final pod connection is
// still local to the endpoint node. The bad offloaded sender stream
// never crosses the veth into the pod.
func (m *Manager) installEdgeProxy(cp string, cfg Config, nodes []string) error {
	return ui.Step("Installing edge proxy (large upload fix)", func() error {
		kubeconfig, err := m.edgeProxyKubeconfig(cp, cfg.Name, adminConf)
		if err != nil {
			return err
		}
		tunnelToken, err := newEdgeProxyTunnelToken()
		if err != nil {
			return err
		}
		if err := m.installEdgeProxyFiles(nodes, kubeconfig, tunnelToken); err != nil {
			return err
		}
		if err := m.installEdgeProxySystemd(nodes); err != nil {
			return err
		}
		return m.waitEdgeProxyRules(nodes)
	})
}

func (m *Manager) installEdgeProxyK3s(cp string, cfg Config, nodes []string) error {
	return ui.Step("Installing edge proxy (large upload fix)", func() error {
		kubeconfig, err := m.edgeProxyKubeconfig(cp, cfg.Name, k3sKubeconfig)
		if err != nil {
			return err
		}
		tunnelToken, err := newEdgeProxyTunnelToken()
		if err != nil {
			return err
		}
		if err := m.installEdgeProxyFiles(nodes, kubeconfig, tunnelToken); err != nil {
			return err
		}
		if err := m.startEdgeProxySupervised(nodes); err != nil {
			return err
		}
		return m.waitEdgeProxyRules(nodes)
	})
}

func (m *Manager) edgeProxyKubeconfig(cp, name, path string) (string, error) {
	args := edgeProxyKubectl(path, "apply", "-f", "-")
	if err := m.rt.ExecStdin(cp, strings.NewReader(edgeProxyRBAC), args...); err != nil {
		return "", fmt.Errorf("installing edge proxy RBAC: %w", err)
	}
	serviceAccountToken, err := m.waitEdgeProxyServiceAccountToken(cp, path)
	if err != nil {
		return "", err
	}
	raw, err := m.rt.Exec(cp, "cat", path)
	if err != nil {
		return "", err
	}
	serverIP, err := m.rt.IP(cp)
	if err != nil {
		return "", err
	}
	return serviceAccountKubeconfig(name, raw, serverIP, serviceAccountToken)
}

func edgeProxyKubectl(kubeconfig string, args ...string) []string {
	base := []string{"kubectl", "--kubeconfig", kubeconfig}
	if kubeconfig == k3sKubeconfig {
		base = []string{"k3s", "kubectl", "--kubeconfig", kubeconfig}
	}
	return append(base, args...)
}

func (m *Manager) waitEdgeProxyServiceAccountToken(cp, kubeconfig string) (string, error) {
	args := edgeProxyKubectl(kubeconfig, "get", "secret", "kiac-edge-proxy-token", "-n", "kube-system", "-o", "jsonpath={.data.token}")
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		encoded, err := m.rt.Exec(cp, args...)
		if err == nil && strings.TrimSpace(encoded) != "" {
			token, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
			if decodeErr != nil {
				return "", fmt.Errorf("decoding edge proxy service-account token: %w", decodeErr)
			}
			return string(token), nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		return "", fmt.Errorf("waiting for edge proxy service-account token: %w", lastErr)
	}
	return "", fmt.Errorf("waiting for edge proxy service-account token: token was not populated within 30s")
}

func newEdgeProxyTunnelToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating edge proxy tunnel token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func edgeProxyBinary() ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(edgeProxyLinuxARM64Gzip))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(gr)
}

func (m *Manager) installEdgeProxyFiles(nodes []string, kubeconfig, tunnelToken string) error {
	bin, err := edgeProxyBinary()
	if err != nil {
		return fmt.Errorf("reading embedded edge proxy: %w", err)
	}
	return inParallel(len(nodes), func(i int) error {
		node := nodes[i]
		if err := m.rt.ExecStdin(node, bytes.NewReader(bin), "sh", "-euc", `
mkdir -p /usr/local/bin
tmp="$(mktemp /usr/local/bin/kiac-edge-proxy.XXXXXX)"
cat > "$tmp"
chmod 0755 "$tmp"
mv "$tmp" /usr/local/bin/kiac-edge-proxy
`); err != nil {
			return err
		}
		if err := m.rt.ExecStdin(node, strings.NewReader(kubeconfig), "sh", "-euc", `
install -d -m 0700 /etc/kiac
cat > /etc/kiac/kubeconfig
chmod 0600 /etc/kiac/kubeconfig
`); err != nil {
			return err
		}
		return m.rt.ExecStdin(node, strings.NewReader(tunnelToken), "sh", "-euc", `
cat > /etc/kiac/tunnel.token
chmod 0600 /etc/kiac/tunnel.token
`)
	})
}

func (m *Manager) installEdgeProxySystemd(nodes []string) error {
	unit := `[Unit]
Description=kiac edge proxy (large upload fix)
After=network-online.target kubelet.service containerd.service
Wants=network-online.target

[Service]
Environment=KUBECONFIG=` + edgeProxyKubeconfigPath + `
ExecStart=` + edgeProxyNodePath + ` --kubeconfig ` + edgeProxyKubeconfigPath + ` --token-file ` + edgeProxyTokenPath + `
Restart=always
RestartSec=1s

[Install]
WantedBy=multi-user.target
`
	return inParallel(len(nodes), func(i int) error {
		node := nodes[i]
		if err := m.rt.ExecStdin(node, strings.NewReader(unit), "sh", "-euc", `
cat > /etc/systemd/system/kiac-edge-proxy.service
chmod 0644 /etc/systemd/system/kiac-edge-proxy.service
`); err != nil {
			return err
		}
		_, err := m.rt.Exec(node, "systemctl", "daemon-reload")
		if err != nil {
			return err
		}
		_, err = m.rt.Exec(node, "systemctl", "enable", "--now", "kiac-edge-proxy.service")
		return err
	})
}

func (m *Manager) startEdgeProxySupervised(nodes []string) error {
	return inParallel(len(nodes), func(i int) error {
		_, err := m.rt.Exec(nodes[i], "sh", "-euc", edgeProxySupervisorScript)
		return err
	})
}

func (m *Manager) waitEdgeProxyRules(nodes []string) error {
	return inParallel(len(nodes), func(i int) error {
		_, err := m.rt.Exec(nodes[i], "sh", "-euc", `
PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/bin/aux:$PATH"
IPT="$(command -v iptables-legacy || command -v iptables)"
for i in $(seq 1 20); do
  if "$IPT" -w -t nat -C PREROUTING -j KIAC-EDGE 2>/dev/null &&
     "$IPT" -w -t nat -C OUTPUT -j KIAC-EDGE-OUTPUT 2>/dev/null; then
    exit 0
  fi
  sleep 1
done
echo "kiac-edge-proxy did not install the PREROUTING hook" >&2
if command -v systemctl >/dev/null 2>&1; then
  systemctl status kiac-edge-proxy.service --no-pager || true
fi
tail -50 `+edgeProxyLogPath+` 2>/dev/null || true
exit 1
`)
		return err
	})
}

const edgeProxySupervisorScript = `
mkdir -p /var/log /var/run
if [ -r ` + edgeProxySupervisorPID + ` ]; then
  old="$(cat ` + edgeProxySupervisorPID + ` 2>/dev/null || true)"
  if [ -n "$old" ] && kill -0 "$old" 2>/dev/null; then
    exit 0
  fi
fi
nohup sh -c 'while :; do ` + edgeProxyNodePath + ` --kubeconfig ` + edgeProxyKubeconfigPath + ` --token-file ` + edgeProxyTokenPath + ` >>` + edgeProxyLogPath + ` 2>&1; sleep 1; done' >/dev/null 2>&1 &
echo $! > ` + edgeProxySupervisorPID + `
`

const edgeProxyRBAC = `apiVersion: v1
kind: ServiceAccount
metadata:
  name: kiac-edge-proxy
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kiac-edge-proxy
rules:
  - apiGroups: [""]
    resources: [services, nodes]
    verbs: [get, list]
  - apiGroups: [discovery.k8s.io]
    resources: [endpointslices]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kiac-edge-proxy
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kiac-edge-proxy
subjects:
  - kind: ServiceAccount
    name: kiac-edge-proxy
    namespace: kube-system
---
apiVersion: v1
kind: Secret
metadata:
  name: kiac-edge-proxy-token
  namespace: kube-system
  annotations:
    kubernetes.io/service-account.name: kiac-edge-proxy
type: kubernetes.io/service-account-token
`

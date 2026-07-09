package cluster

import (
	"strings"

	"github.com/saiyam1814/kiac/pkg/ui"
)

// kiac-lb replaces MetalLB: vmnet only delivers frames addressed to a
// VM's real IP, so floating VIPs never work here and the only usable
// LoadBalancer addresses are the node IPs themselves. That collapses the
// whole controller to "patch Service status with an eligible node IP",
// which a shell loop on the control plane does with the node's own
// kubectl and /etc/kubernetes/admin.conf. No pods, no images, no RBAC,
// no webhook to wait for, and node restarts self-heal: a stale ingress
// IP is re-pointed on the next pass.

const (
	kiacLBScriptPath       = "/usr/local/bin/kiac-lb.sh"
	kiacLBUnitPath         = "/etc/systemd/system/kiac-lb.service"
	kiacLBK3sLogPath       = "/var/log/kiac-lb.log"
	kiacLBK3sSupervisorPID = "/var/run/kiac-lb-supervisor.pid"
)

// kiacLBScript is the controller itself. POSIX sh only, and no jq: the
// node image does not ship it, so kubectl go-template/jsonpath output is
// parsed with awk/grep. The loop must never exit; any kubectl failure is
// simply retried on the next pass.
const kiacLBScript = `#!/bin/sh
# kiac-lb: the LoadBalancer controller for kiac clusters.
#
# Runs inside the control-plane VM as a systemd service and assigns node
# IPs to Services of type LoadBalancer by patching their status with the
# node's own kubectl. vmnet only delivers frames addressed to a VM's
# assigned IP, so floating VIPs can never work here; node IPs are the
# only host-reachable addresses.
#
# Constraints: POSIX sh, no jq in the node image (kubectl go-template and
# jsonpath do the parsing), and the loop never exits: a kubectl failure
# waits for the next pass, which also re-points Services whose node died
# or came back from a restart with a new vmnet IP.

: "${KUBECONFIG:=/etc/kubernetes/admin.conf}"
export KUBECONFIG
DIR=/run/kiac-lb
INTERVAL=3

NODE_FMT='{range .items[*]}{.metadata.name} {.status.addresses[?(@.type=="InternalIP")].address} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'
SVC_FMT='{{range .items}}{{if eq .spec.type "LoadBalancer"}}{{.metadata.namespace}}|{{.metadata.name}}|{{if .status.loadBalancer}}{{if .status.loadBalancer.ingress}}{{range .status.loadBalancer.ingress}}{{if .ip}}{{.ip}} {{end}}{{end}}{{end}}{{end}}|{{range .spec.ports}}{{.port}} {{end}}{{"\n"}}{{end}}{{end}}'
EP_FMT='{{range .items}}{{if .endpoints}}{{range .endpoints}}{{if .conditions}}{{if .conditions.ready}}{{if .nodeName}}{{.nodeName}}{{"\n"}}{{end}}{{end}}{{end}}{{end}}{{end}}{{end}}'

# eligible_nodes prints "name ip" for each node whose IP may carry
# LoadBalancer traffic: Ready workers when the cluster has workers, the
# control plane only when it is the sole node (keeps :6443 off the LB
# surface). $NF guards nodes still missing an address or Ready condition.
eligible_nodes() {
    out=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath="$NODE_FMT") || return 1
    if [ -z "$out" ]; then
        out=$(kubectl get nodes -o jsonpath="$NODE_FMT") || return 1
    fi
    printf '%s\n' "$out" | awk 'NF >= 3 && $NF == "True" { print $1, $2 }'
}

# is_eligible_ip IP: succeeds when IP belongs to an eligible node.
is_eligible_ip() {
    awk -v ip="$1" '$2 == ip { found = 1 } END { exit !found }' "$DIR/nodes"
}

# claim IP PORT...: records IP+port pairs taken by an assigned Service.
claim() {
    c_ip=$1; shift
    for c_port in "$@"; do
        printf '%s %s\n' "$c_ip" "$c_port" >> "$DIR/used"
    done
}

# conflicts IP PORT...: succeeds when any port is already claimed on IP.
# Sharing one IP is fine while port sets stay disjoint (Grafana on 3000
# next to Traefik on 80/443 rides a single-worker cluster this way).
conflicts() {
    q_ip=$1; shift
    for q_port in "$@"; do
        if grep -qFx "$q_ip $q_port" "$DIR/used"; then
            return 0
        fi
    done
    return 1
}

# choose_ip NS NAME "PORTS": prints the best conflict-free eligible node
# IP. Preference order: a node hosting a ready endpoint of the Service
# (delivery stays pod-local; traffic NATed to another node crosses
# vmnet's slow forwarding path), then the kiac.io/lb-primary node, then
# any eligible node.
choose_ip() {
    sel_ns=$1; sel_name=$2; sel_ports=$3
    eps=$(kubectl get endpointslices -n "$sel_ns" \
        -l "kubernetes.io/service-name=$sel_name" \
        -o go-template="$EP_FMT" 2>/dev/null)
    for cand in $eps $(cat "$DIR/primary" 2>/dev/null) $(awk '{ print $1 }' "$DIR/nodes"); do
        cand_ip=$(awk -v n="$cand" '$1 == n { print $2; exit }' "$DIR/nodes")
        [ -n "$cand_ip" ] || continue
        if ! conflicts "$cand_ip" $sel_ports; then
            printf '%s\n' "$cand_ip"
            return 0
        fi
    done
    return 1
}

# patch_svc NS NAME IP: writes the ingress IP into the Service status.
patch_svc() {
    kubectl patch svc -n "$1" "$2" --subresource=status --type=merge \
        -p '{"status":{"loadBalancer":{"ingress":[{"ip":"'"$3"'"}]}}}'
}

pass() {
    eligible_nodes > "$DIR/nodes" || return 1
    [ -s "$DIR/nodes" ] || return 0

    kubectl get svc -A -o go-template="$SVC_FMT" > "$DIR/svcs" || return 1
    kubectl get nodes -l 'kiac.io/lb-primary=true' \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
        | head -n 1 > "$DIR/primary"

    # First sweep: Services whose ingress IP is still an eligible node IP
    # keep it; record their IP+port claims so the second sweep only
    # shares an IP with a disjoint port set.
    : > "$DIR/used"
    while IFS='|' read -r ns name ips ports; do
        [ -n "$ns" ] || continue
        set -- $ips
        cur=${1:-}
        [ -n "$cur" ] || continue
        if is_eligible_ip "$cur"; then
            claim "$cur" $ports
        fi
    done < "$DIR/svcs"

    # Second sweep: assign Services with no IP and re-point Services
    # whose IP stopped being an eligible node IP (node gone, or back
    # from a restart with a new vmnet address).
    while IFS='|' read -r ns name ips ports; do
        [ -n "$ns" ] || continue
        set -- $ips
        cur=${1:-}
        if [ -n "$cur" ] && is_eligible_ip "$cur"; then
            continue
        fi
        new=$(choose_ip "$ns" "$name" "$ports") || {
            echo "kiac-lb: no conflict-free node IP for $ns/$name (ports: $ports)"
            continue
        }
        if patch_svc "$ns" "$name" "$new"; then
            claim "$new" $ports
            echo "kiac-lb: $ns/$name -> $new"
        fi
    done < "$DIR/svcs"
}

mkdir -p "$DIR" 2>/dev/null || DIR=/tmp/kiac-lb
mkdir -p "$DIR"
while :; do
    pass
    sleep "$INTERVAL"
done
`

// kiacLBUnit keeps the loop alive across crashes and control-plane VM
// restarts (the unit is enabled, so a stop/start of the node brings the
// controller back without kiac's help).
const kiacLBUnit = `# kiac-lb assigns node IPs to Services of type LoadBalancer.
# Installed by kiac create; logs: journalctl -u kiac-lb
[Unit]
Description=kiac LoadBalancer controller
After=kubelet.service

[Service]
ExecStart=` + kiacLBScriptPath + `
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`

// lbInstallStep is one exec into the control-plane VM; stdin is piped
// when non-empty. Kept as data so tests can check the argv construction
// without a VM.
type lbInstallStep struct {
	stdin string
	args  []string
}

// lbInstallSteps writes the script and unit, then enables the service.
// `cat > file` via stdin avoids quoting the payloads through exec argv.
func lbInstallSteps() []lbInstallStep {
	return []lbInstallStep{
		{stdin: kiacLBScript, args: []string{"sh", "-c",
			"cat > " + kiacLBScriptPath + " && chmod 0755 " + kiacLBScriptPath}},
		{stdin: kiacLBUnit, args: []string{"sh", "-c",
			"cat > " + kiacLBUnitPath}},
		{args: []string{"sh", "-c",
			"systemctl daemon-reload && systemctl enable --now kiac-lb.service"}},
	}
}

// installKiacLB installs the controller inside the control-plane VM.
// There is no readiness to wait for: the first pass runs within seconds
// of `systemctl enable --now`.
func (m *Manager) installKiacLB(cp string) error {
	return ui.Step("Installing LoadBalancer (kiac-lb)", func() error {
		for _, s := range lbInstallSteps() {
			if s.stdin != "" {
				if err := m.rt.ExecStdin(cp, strings.NewReader(s.stdin), s.args...); err != nil {
					return err
				}
				continue
			}
			if _, err := m.rt.Exec(cp, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Manager) installKiacLBK3s(cp string) error {
	return ui.Step("Installing LoadBalancer (kiac-lb)", func() error {
		if err := m.rt.ExecStdin(cp, strings.NewReader(kiacLBScript), "sh", "-c",
			"mkdir -p /usr/local/bin && cat > "+kiacLBScriptPath+" && chmod 0755 "+kiacLBScriptPath); err != nil {
			return err
		}
		_, err := m.rt.Exec(cp, "sh", "-euc", kiacLBK3sSupervisorScript)
		return err
	})
}

const kiacLBK3sSupervisorScript = `
mkdir -p /var/log /var/run
if [ -r ` + kiacLBK3sSupervisorPID + ` ]; then
  old="$(cat ` + kiacLBK3sSupervisorPID + ` 2>/dev/null || true)"
  if [ -n "$old" ] && kill -0 "$old" 2>/dev/null; then
    exit 0
  fi
fi
nohup sh -c 'while :; do KUBECONFIG=` + k3sKubeconfig + ` ` + kiacLBScriptPath + ` >>` + kiacLBK3sLogPath + ` 2>&1; sleep 1; done' >/dev/null 2>&1 &
echo $! > ` + kiacLBK3sSupervisorPID + `
`

// labelLBPrimary marks the first LB-eligible node (worker-1 when workers
// exist, else the control plane). Bundled addons (Grafana, Traefik) pin
// their pods to this label with a nodeSelector and kiac-lb prefers it
// when assigning IPs, so traffic that enters the node is delivered to a
// local pod instead of being NATed across vmnet's slow forwarding path
// (~100x worse for bulk transfers).
func (m *Manager) labelLBPrimary(cp string, cfg Config, nodes []string) error {
	primary := nodes[0]
	if cfg.Workers > 0 {
		primary = nodes[1]
	}
	_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
		"label", "node", primary, "kiac.io/lb-primary=true", "--overwrite")
	return err
}

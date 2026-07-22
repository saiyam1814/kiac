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
SVC_FMT='{{range .items}}{{if eq .spec.type "LoadBalancer"}}{{.metadata.namespace}}|{{.metadata.name}}|{{if .status.loadBalancer}}{{if .status.loadBalancer.ingress}}{{range .status.loadBalancer.ingress}}{{if .ip}}{{.ip}} {{end}}{{end}}{{end}}{{end}}|{{range .spec.ports}}{{.port}} {{end}}|{{range .spec.ipFamilies}}{{.}} {{end}}{{"\n"}}{{end}}{{end}}'
EP_FMT='{{range .items}}{{if .endpoints}}{{range .endpoints}}{{if .conditions}}{{if .conditions.ready}}{{if .nodeName}}{{.nodeName}}{{"\n"}}{{end}}{{end}}{{end}}{{end}}{{end}}{{end}}'

# fam_of IP: prints v4 or v6 for an address (a colon means IPv6). Used to
# match a Service's requested ipFamily to a node's address of that family.
fam_of() {
    case "$1" in
        *:*) echo v6 ;;
        *)   echo v4 ;;
    esac
}

# eligible_nodes prints "name v4 v6" for each node whose IPs may carry
# LoadBalancer traffic: Ready workers when the cluster has workers, the
# control plane only when it is the sole node (keeps :6443 off the LB
# surface). A dual-stack node reports both an IPv4 and an IPv6
# InternalIP; either column is "-" when that family is absent. $NF is the
# Ready status, so addresses are every field between the name and it.
eligible_nodes() {
    out=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath="$NODE_FMT") || return 1
    if [ -z "$out" ]; then
        out=$(kubectl get nodes -o jsonpath="$NODE_FMT") || return 1
    fi
    printf '%s\n' "$out" | awk 'NF >= 3 && $NF == "True" {
        v4 = "-"; v6 = "-"
        for (i = 2; i < NF; i++) {
            if (index($i, ":") > 0) { v6 = $i } else { v4 = $i }
        }
        print $1, v4, v6
    }'
}

# node_addr NAME FAMILY: prints the eligible node NAME address for FAMILY
# (v4 or v6), or nothing when the node has no address of that family.
node_addr() {
    awk -v n="$1" -v fam="$2" '$1 == n {
        addr = (fam == "v6") ? $3 : $2
        if (addr != "-") print addr
        exit
    }' "$DIR/nodes"
}

# is_eligible_ip IP: succeeds when IP is an eligible node address of
# either family.
is_eligible_ip() {
    awk -v ip="$1" '$2 == ip || $3 == ip { found = 1 } END { exit !found }' "$DIR/nodes"
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

# choose_addr NS NAME FAMILY "PORTS": prints the best conflict-free
# eligible node address of FAMILY (v4/v6). Preference order: a node
# hosting a ready endpoint of the Service (delivery stays pod-local;
# traffic NATed to another node crosses vmnet's slow forwarding path),
# then the kiac.io/lb-primary node, then any eligible node. A node with
# no address of FAMILY is skipped.
choose_addr() {
    sel_ns=$1; sel_name=$2; sel_fam=$3; sel_ports=$4
    eps=$(kubectl get endpointslices -n "$sel_ns" \
        -l "kubernetes.io/service-name=$sel_name" \
        -o go-template="$EP_FMT" 2>/dev/null)
    for cand in $eps $(cat "$DIR/primary" 2>/dev/null) $(awk '{ print $1 }' "$DIR/nodes"); do
        cand_ip=$(node_addr "$cand" "$sel_fam")
        [ -n "$cand_ip" ] || continue
        if ! conflicts "$cand_ip" $sel_ports; then
            printf '%s\n' "$cand_ip"
            return 0
        fi
    done
    return 1
}

# svc_families "FAMILIES": echoes the requested families as v4/v6 tokens,
# defaulting to v4 when a Service declares none (single-stack IPv4, the
# only case on a v4 cluster). Keeps output stable so v4 clusters assign
# exactly one IPv4 ingress as before.
svc_families() {
    got=""
    for f in $1; do
        case "$f" in
            IPv4) got="$got v4" ;;
            IPv6) got="$got v6" ;;
        esac
    done
    [ -n "$got" ] && echo "$got" || echo v4
}

# patch_svc NS NAME IP...: writes one ingress entry per IP into status.
patch_svc() {
    p_ns=$1; p_name=$2; shift 2
    ingress=""
    for p_ip in "$@"; do
        [ -n "$ingress" ] && ingress="$ingress,"
        ingress="$ingress{\"ip\":\"$p_ip\"}"
    done
    kubectl patch svc -n "$p_ns" "$p_name" --subresource=status --type=merge \
        -p '{"status":{"loadBalancer":{"ingress":['"$ingress"']}}}'
}

pass() {
    eligible_nodes > "$DIR/nodes" || return 1
    [ -s "$DIR/nodes" ] || return 0

    kubectl get svc -A -o go-template="$SVC_FMT" > "$DIR/svcs" || return 1
    kubectl get nodes -l 'kiac.io/lb-primary=true' \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
        | head -n 1 > "$DIR/primary"

    # First sweep: every ingress IP that is still an eligible node address
    # keeps its port claims, so the second sweep only shares an IP with a
    # disjoint port set (across both families).
    : > "$DIR/used"
    while IFS='|' read -r ns name ips ports families; do
        [ -n "$ns" ] || continue
        for cur in $ips; do
            if is_eligible_ip "$cur"; then
                claim "$cur" $ports
            fi
        done
    done < "$DIR/svcs"

    # Second sweep: for each requested family, keep an existing eligible
    # ingress IP of that family or assign a new one, then patch the
    # Service once with the full set. A Service is re-pointed when a node
    # died or came back from a restart with a new vmnet address.
    while IFS='|' read -r ns name ips ports families; do
        [ -n "$ns" ] || continue
        want_ips=""
        changed=0
        for fam in $(svc_families "$families"); do
            keep=""
            for cur in $ips; do
                if [ "$(fam_of "$cur")" = "$fam" ] && is_eligible_ip "$cur"; then
                    keep=$cur
                    break
                fi
            done
            if [ -z "$keep" ]; then
                keep=$(choose_addr "$ns" "$name" "$fam" "$ports") || {
                    echo "kiac-lb: no conflict-free $fam node IP for $ns/$name (ports: $ports)"
                    continue
                }
                changed=1
            fi
            want_ips="$want_ips $keep"
            claim "$keep" $ports
        done
        [ -n "$want_ips" ] || continue
        # Re-point when the assigned set differs from what status holds
        # (order-insensitive): a family was added, or an address changed.
        for cur in $ips; do
            case " $want_ips " in
                *" $cur "*) ;;
                *) changed=1 ;;
            esac
        done
        [ "$changed" = 1 ] || continue
        if patch_svc "$ns" "$name" $want_ips; then
            echo "kiac-lb: $ns/$name ->$want_ips"
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

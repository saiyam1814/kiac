#!/usr/bin/env bash
# Run a real two-cluster k8gb failover lab on kiac without Docker or k3d.
set -euo pipefail

K8GB_VERSION="v0.20.0"
EDGE_IMAGE="docker.io/ubuntu/bind9@sha256:948f400e4a6034973dabfdc0cbf08dddd3a4cd891c92925a25ae6f4510527f89"
APP_IMAGE="docker.io/library/nginx@sha256:42a516af16b852e33b7682d5ef8acbd5d13fe08fecadc7ed98605ba5e3b26ab8"
TSIG_SECRET="96Ah/a2g0/nLeFGK+d/0tzQcccf9hCEIy34PoXX2Qg8="

EDGE_NAME="kiac-k8gb-edgedns"
EU_CLUSTER="k8gb-eu"
US_CLUSTER="k8gb-us"
EU_CONTEXT="kiac-${EU_CLUSTER}"
US_CONTEXT="kiac-${US_CLUSTER}"
HOSTNAME="demo.cloud.example.com"
STATE_DIR="${KIAC_K8GB_STATE_DIR:-${HOME}/.kiac/labs/k8gb}"

info() { printf '    %s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<EOF
Usage: $0 [up|verify|failover|cleanup]

  up        Create two small k3s clusters, install k8gb, and deploy the demo
  verify    Query authoritative DNS and send traffic to the healthy region
  failover  Scale the EU app down, prove US failover, then prove EU failback
  cleanup   Delete only the clusters and edge DNS VM created by this script

State: ${STATE_DIR}
EOF
}

require_tools() {
  local tool
  for tool in kiac container kubectl helm curl dig jq; do
    command -v "${tool}" >/dev/null 2>&1 || fail "missing tool: ${tool}"
  done
  kiac doctor >/dev/null 2>&1 || fail "kiac preflight failed; run: kiac doctor"
}

container_exists() {
  container inspect "$1" >/dev/null 2>&1
}

container_state() {
  container inspect "$1" | jq -r '.[0].status.state'
}

container_ipv4() {
  container inspect "$1" \
    | jq -r '.[0].status.networks[0].ipv4Address // empty | split("/")[0]'
}

context_exists() {
  kubectl config get-contexts -o name 2>/dev/null | grep -Fqx "$1"
}

context_ready() {
  kubectl --context "$1" get --raw=/readyz >/dev/null 2>&1
}

write_edge_config() {
  local edge_dir="${STATE_DIR}/edge"
  mkdir -p "${edge_dir}"

  cat > "${edge_dir}/named.conf" <<'EOF'
include "/kiac-k8gb/ddns.key";
options {
  directory "/kiac-k8gb";
  listen-on port 1053 { any; };
  listen-on-v6 { none; };
  allow-query { any; };
  allow-recursion { any; };
  recursion yes;
  dnssec-validation no;
};
zone "example.com" {
  type master;
  file "/kiac-k8gb/k8s.zone";
  allow-transfer { key "externaldns-key"; };
  update-policy { grant externaldns-key zonesub ANY; };
};
EOF

  cat > "${edge_dir}/ddns.key" <<EOF
key "externaldns-key" {
  algorithm hmac-sha256;
  secret "${TSIG_SECRET}";
};
EOF

  if [[ ! -f "${edge_dir}/k8s.zone" ]]; then
    cat > "${edge_dir}/k8s.zone" <<'EOF'
$TTL 10
@ IN SOA example.com. root.example.com. (
  1 10 10 120 10
)
  IN NS ns.example.com.
ns IN A 127.0.0.1
EOF
  fi

  chmod 0777 "${edge_dir}"
  chmod 0666 "${edge_dir}/k8s.zone"
  chmod 0644 "${edge_dir}/named.conf" "${edge_dir}/ddns.key"
}

ensure_edge_dns() {
  local marker="${STATE_DIR}/edge.created"
  local edge_ip=""

  write_edge_config
  if container_exists "${EDGE_NAME}"; then
    [[ -f "${marker}" ]] || fail "container ${EDGE_NAME} already exists and is not owned by this lab"
    if [[ "$(container_state "${EDGE_NAME}")" != "running" ]]; then
      step "Starting the existing edge DNS VM"
      container start "${EDGE_NAME}" >/dev/null
    fi
  else
    step "Starting the 512 MiB edge DNS VM"
    touch "${marker}"
    container run --detach \
      --name "${EDGE_NAME}" \
      --label kiac.io/lab=k8gb \
      --progress none \
      --cpus 1 \
      --memory 512M \
      --mount "type=bind,source=${STATE_DIR}/edge,target=/kiac-k8gb" \
      "${EDGE_IMAGE}" -g -c /kiac-k8gb/named.conf >/dev/null
  fi

  for _ in $(seq 1 60); do
    edge_ip="$(container_ipv4 "${EDGE_NAME}")"
    if [[ -n "${edge_ip}" ]] && dig @"${edge_ip}" -p 1053 example.com SOA +short +time=1 +tries=1 >/dev/null 2>&1; then
      info "edge DNS is ready at ${edge_ip}:1053"
      return
    fi
    sleep 2
  done
  container logs "${EDGE_NAME}" >&2 || true
  fail "edge DNS did not become ready"
}

ensure_cluster() {
  local name="$1"
  local context="kiac-${name}"
  local node="kiac-${name}-control-plane"
  local marker="${STATE_DIR}/${name}.created"

  if context_ready "${context}"; then
    [[ -f "${marker}" ]] || fail "context ${context} is live and is not owned by this lab"
    info "using existing lab cluster ${name}"
    return
  fi

  if container_exists "${node}"; then
    [[ -f "${marker}" ]] || fail "cluster ${name} exists and is not owned by this lab"
    step "Resuming lab cluster ${name}"
    kiac resume cluster --name "${name}"
    return
  fi

  if context_exists "${context}"; then
    fail "stale context ${context} exists without a cluster; remove it or run cleanup"
  fi

  step "Creating single-node k3s cluster ${name}"
  touch "${marker}"
  kiac create cluster \
    --name "${name}" \
    --distro k3s \
    --workers 0 \
    --cpus 3 \
    --cp-memory 3G \
    --no-metrics \
    --no-storage \
    --wait 8m
}

write_values() {
  local path="$1"
  local geo="$2"
  local peers="$3"
  local edge_ip="$4"

  cat > "${path}" <<EOF
k8gb:
  installLegacyCrds: false
  clusterGeoTag: "${geo}"
  extGslbClustersGeoTags: "${peers}"
  edgeDNSServers:
    - "${edge_ip}:1053"
  reconcileRequeueSeconds: 5
  nsRecordTTL: 10
  dnsZones:
    - parentZone: example.com
      loadBalancedZone: cloud.example.com
      dnsZoneNegTTL: 10
  log:
    format: simple
    level: info

coredns:
  serviceType: LoadBalancer

extdns:
  enabled: true
  fullnameOverride: k8gb-external-dns
  provider:
    name: rfc2136
  domainFilters:
    - example.com
  dnsPolicy: ClusterFirst
  txtOwnerId: "k8gb-${geo}"
  txtPrefix: "k8gb-${geo}-"
  env:
    - name: EXTERNAL_DNS_RFC2136_TSIG_SECRET
      valueFrom:
        secretKeyRef:
          name: rfc2136
          key: secret
  extraArgs:
    rfc2136-tsig-axfr:
    rfc2136-tsig-secret-alg: hmac-sha256
    rfc2136-tsig-keyname: externaldns-key
    rfc2136-host: "${edge_ip}"
    rfc2136-port: 1053
    rfc2136-zone: example.com
EOF
}

wait_for_lb_ip() {
  local context="$1"
  local namespace="$2"
  local service="$3"
  local ip=""

  for _ in $(seq 1 90); do
    ip="$(kubectl --context "${context}" -n "${namespace}" get service "${service}" \
      -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${ip}" ]]; then
      printf '%s\n' "${ip}"
      return
    fi
    sleep 2
  done
  fail "${context}: ${namespace}/${service} has no LoadBalancer IP"
}

install_k8gb() {
  local context="$1"
  local geo="$2"
  local peers="$3"
  local edge_ip="$4"
  local values="${STATE_DIR}/values-${geo}.yaml"

  step "Installing k8gb ${K8GB_VERSION} in ${geo}"
  write_values "${values}" "${geo}" "${peers}" "${edge_ip}"
  kubectl --context "${context}" create namespace k8gb --dry-run=client -o yaml \
    | kubectl --context "${context}" apply -f - >/dev/null
  kubectl --context "${context}" -n k8gb create secret generic rfc2136 \
    --from-literal="secret=${TSIG_SECRET}" --dry-run=client -o yaml \
    | kubectl --context "${context}" apply -f - >/dev/null
  helm upgrade --install k8gb k8gb/k8gb \
    --kube-context "${context}" \
    --namespace k8gb \
    --version "${K8GB_VERSION}" \
    --values "${values}" \
    --wait \
    --timeout 8m

  local dns_ip
  dns_ip="$(wait_for_lb_ip "${context}" k8gb k8gb-coredns)"
  info "${geo} authoritative DNS: ${dns_ip}:53"
}

deploy_demo() {
  local context="$1"
  local region="$2"

  step "Deploying the ${region} service"
  kubectl --context "${context}" apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: k8gb-demo
data:
  index.html: |
    region=${region}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: k8gb-demo
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: k8gb-demo
  template:
    metadata:
      labels:
        app.kubernetes.io/name: k8gb-demo
    spec:
      containers:
        - name: web
          image: ${APP_IMAGE}
          ports:
            - name: http
              containerPort: 80
          readinessProbe:
            httpGet:
              path: /
              port: http
          volumeMounts:
            - name: content
              mountPath: /usr/share/nginx/html/index.html
              subPath: index.html
      volumes:
        - name: content
          configMap:
            name: k8gb-demo
---
apiVersion: v1
kind: Service
metadata:
  name: k8gb-demo
  labels:
    app.kubernetes.io/name: k8gb-demo
spec:
  type: LoadBalancer
  selector:
    app.kubernetes.io/name: k8gb-demo
  ports:
    - name: http
      port: 8080
      targetPort: http
---
apiVersion: k8gb.io/v1beta1
kind: Gslb
metadata:
  name: k8gb-demo
  annotations:
    k8gb.io/hostname: "${HOSTNAME}"
spec:
  resourceRef:
    apiVersion: v1
    kind: Service
    matchLabels:
      app.kubernetes.io/name: k8gb-demo
  strategy:
    type: failover
    primaryGeoTag: eu
    dnsTtlSeconds: 10
EOF
  kubectl --context "${context}" rollout status deployment/k8gb-demo --timeout=180s
  wait_for_lb_ip "${context}" default k8gb-demo >/dev/null
}

edge_ip_or_fail() {
  container_exists "${EDGE_NAME}" || fail "edge DNS is absent; run: $0 up"
  local ip
  ip="$(container_ipv4 "${EDGE_NAME}")"
  [[ -n "${ip}" ]] || fail "edge DNS has no IPv4 address"
  printf '%s\n' "${ip}"
}

wait_for_region() {
  local expected="$1"
  local edge_ip="$2"
  local deadline=$((SECONDS + 180))
  local answers=""
  local ip=""
  local body=""

  while (( SECONDS < deadline )); do
    answers="$(dig @"${edge_ip}" -p 1053 "${HOSTNAME}" A +short +time=2 +tries=1 \
      | awk '/^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/')"
    for ip in ${answers}; do
      body="$(curl --fail --silent --show-error --max-time 5 "http://${ip}:8080/" 2>/dev/null || true)"
      if grep -Fq "region=${expected}" <<<"${body}"; then
        info "${HOSTNAME} -> ${ip}:8080 -> ${body}"
        return
      fi
    done
    sleep 3
  done

  printf 'Last DNS answers: %s\n' "${answers:-none}" >&2
  kubectl --context "${EU_CONTEXT}" get gslb k8gb-demo -o wide >&2 || true
  kubectl --context "${US_CONTEXT}" get gslb k8gb-demo -o wide >&2 || true
  fail "DNS did not converge on region=${expected} within 180 seconds"
}

verify_lab() {
  local edge_ip
  edge_ip="$(edge_ip_or_fail)"
  context_ready "${EU_CONTEXT}" || fail "${EU_CONTEXT} is not ready"
  context_ready "${US_CONTEXT}" || fail "${US_CONTEXT} is not ready"

  step "Checking DNS delegation"
  info "edge DNS: ${edge_ip}:1053"
  info "delegated nameservers:"
  dig +nocmd @"${edge_ip}" -p 1053 cloud.example.com NS +norecurse +noall +authority +time=2 +tries=1
  info "active failover address (TCP DNS):"
  dig @"${edge_ip}" -p 1053 "${HOSTNAME}" A +short +tcp +time=2 +tries=1

  step "Sending traffic through the failover record"
  wait_for_region eu "${edge_ip}"
}

run_failover() {
  local edge_ip
  edge_ip="$(edge_ip_or_fail)"
  verify_lab

  restore_primary() {
    kubectl --context "${EU_CONTEXT}" scale deployment/k8gb-demo --replicas=1 >/dev/null 2>&1 || true
  }
  trap restore_primary EXIT

  step "Stopping the primary application"
  kubectl --context "${EU_CONTEXT}" scale deployment/k8gb-demo --replicas=0
  wait_for_region us "${edge_ip}"

  step "Restoring the primary application"
  restore_primary
  kubectl --context "${EU_CONTEXT}" rollout status deployment/k8gb-demo --timeout=180s
  wait_for_region eu "${edge_ip}"
  trap - EXIT
}

up() {
  require_tools
  mkdir -p "${STATE_DIR}"

  local edge_ip
  ensure_edge_dns
  edge_ip="$(edge_ip_or_fail)"
  ensure_cluster "${EU_CLUSTER}"
  ensure_cluster "${US_CLUSTER}"

  helm repo add k8gb https://www.k8gb.io --force-update >/dev/null
  install_k8gb "${EU_CONTEXT}" eu us "${edge_ip}"
  install_k8gb "${US_CONTEXT}" us eu "${edge_ip}"
  deploy_demo "${EU_CONTEXT}" eu
  deploy_demo "${US_CONTEXT}" us
  verify_lab

  printf '\nLab is ready. Run: %s failover\n' "$0"
}

cleanup() {
  local name
  for name in "${EU_CLUSTER}" "${US_CLUSTER}"; do
    if [[ -f "${STATE_DIR}/${name}.created" ]]; then
      step "Deleting lab cluster ${name}"
      kiac delete cluster --name "${name}" || true
      rm -f "${STATE_DIR}/${name}.created"
    else
      info "leaving ${name} alone (no ownership marker)"
    fi
  done

  if [[ -f "${STATE_DIR}/edge.created" ]]; then
    step "Deleting the edge DNS VM"
    container delete --force "${EDGE_NAME}" >/dev/null 2>&1 || true
    rm -f "${STATE_DIR}/edge.created"
  else
    info "leaving ${EDGE_NAME} alone (no ownership marker)"
  fi

  rm -f "${STATE_DIR}/values-eu.yaml" "${STATE_DIR}/values-us.yaml"
  info "BIND zone data remains in ${STATE_DIR}/edge for inspection"
}

case "${1:-up}" in
  up) up ;;
  verify) require_tools; verify_lab ;;
  failover) require_tools; run_failover ;;
  cleanup) cleanup ;;
  -h|--help|help) usage ;;
  *) usage >&2; exit 1 ;;
esac

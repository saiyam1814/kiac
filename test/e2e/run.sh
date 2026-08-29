#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
PROFILE=${1:-quick}
RUN_ID=${KIAC_E2E_RUN_ID:-$(date +%s)}
RUN_ID=$(printf '%s' "${RUN_ID}" | tr -cd 'a-zA-Z0-9-' | cut -c1-20)
export KIAC_E2E_PREFIX=${KIAC_E2E_PREFIX:-"ci-${RUN_ID}"}
export KIAC_E2E_STATE_FILE=${KIAC_E2E_STATE_FILE:-"${TMPDIR:-/tmp}/kiac-e2e-${RUN_ID}.clusters"}
export KIAC_E2E_KUBECONFIG=${KIAC_E2E_KUBECONFIG:-"${TMPDIR:-/tmp}/kiac-e2e-${RUN_ID}.kubeconfig"}
export KUBECONFIG=${KIAC_E2E_KUBECONFIG}

KIAC_BIN=${KIAC_BIN:-"${ROOT}/bin/kiac"}
TRAFFIC_BIN="${ROOT}/dist/e2e/kiac-e2e-traffic-linux-arm64"
CURRENT_CLUSTER=""
CURRENT_CONTEXT=""
CURRENT_NODES=""
TRAFFIC_POD=""
CURRENT_MOUNT_DIR=""
UPLOAD_NODE_PORT=""

umask 077
mkdir -p "$(dirname "${TRAFFIC_BIN}")" "$(dirname "${KIAC_E2E_STATE_FILE}")" "$(dirname "${KUBECONFIG}")"
touch "${KIAC_E2E_STATE_FILE}" "${KUBECONFIG}"

k() {
  kubectl --context "${CURRENT_CONTEXT}" --request-timeout=15s "$@"
}

node_ipv4() {
  container exec "$1" sh -c "ip -4 route get 1.1.1.1 | awk '{for(i=1;i<NF;i++) if (\$i==\"src\") print \$(i+1)}'" | tr -d '\r\n'
}

node_ipv6() {
  container exec "$1" sh -c "ip -6 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1 | grep -v '^fe80' | head -n1" | tr -d '\r\n'
}

url_host() {
  case "$1" in
    *:*) printf '[%s]' "$1" ;;
    *) printf '%s' "$1" ;;
  esac
}

retry() {
  local attempts=$1
  local delay=$2
  shift 2
  local i
  for ((i = 1; i <= attempts; i++)); do
    if "$@"; then
      return 0
    fi
    sleep "${delay}"
  done
  return 1
}

diagnostics() {
  [[ -n "${CURRENT_CLUSTER}" ]] || return 0
  printf '\nRuntime diagnostics for %s\n' "${CURRENT_CLUSTER}" >&2
  "${KIAC_BIN}" get nodes --name "${CURRENT_CLUSTER}" >&2 || true
  if [[ -n "${CURRENT_CONTEXT}" ]]; then
    k get nodes -o wide >&2 || true
    k get pods -A -o wide >&2 || true
    k get svc -A >&2 || true
    k get gateway,httproute -A >&2 || true
  fi
  local node
  for node in ${CURRENT_NODES}; do
    printf '\nEdge proxy on %s\n' "${node}" >&2
    container exec "${node}" sh -c '
PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/bin/aux:$PATH"
IPT="$(command -v iptables-legacy || command -v iptables)"
"$IPT" -w -t nat -nvL KIAC-EDGE 2>/dev/null || true
ss -tnp 2>/dev/null | grep -E "15080|32080" || true
if command -v journalctl >/dev/null 2>&1; then
  journalctl -u kiac-edge-proxy --no-pager -n 40 2>/dev/null || true
else
  tail -40 /var/log/kiac-edge-proxy.log 2>/dev/null || true
fi
' >&2 || true
  done
}

finish() {
  local status=$?
  if [[ ${status} -ne 0 ]]; then
    diagnostics
  fi
  if [[ ${status} -ne 0 && "${KIAC_E2E_KEEP_ON_FAILURE:-false}" == true ]]; then
    printf '\nRetained %s for debugging. Clean it with:\n  %s delete cluster --name %s\n' \
      "${CURRENT_CLUSTER}" "${KIAC_BIN}" "${CURRENT_CLUSTER}" >&2
  else
    "${ROOT}/test/e2e/cleanup.sh"
    if [[ -n "${CURRENT_MOUNT_DIR}" ]]; then
      rm -rf -- "${CURRENT_MOUNT_DIR}"
    fi
  fi
  exit "${status}"
}

trap 'exit 130' INT
trap 'exit 143' TERM
trap finish EXIT

wait_for_metrics() {
  k top nodes >/dev/null 2>&1
}

wait_for_server() {
  local pod=$1
  k -n kiac-e2e exec "${pod}" -- wget -q -O - http://127.0.0.1:8080/healthz >/dev/null
}

start_traffic_server() {
  k -n kiac-e2e rollout status deployment/upload-server --timeout=180s
  TRAFFIC_POD=$(k -n kiac-e2e get pod -l app=upload-server -o jsonpath='{.items[0].metadata.name}')
  k -n kiac-e2e cp "${TRAFFIC_BIN}" "${TRAFFIC_POD}:/tmp/kiac-e2e-traffic"
  k -n kiac-e2e exec "${TRAFFIC_POD}" -- chmod 0755 /tmp/kiac-e2e-traffic
  k -n kiac-e2e exec "${TRAFFIC_POD}" -- sh -c \
    'nohup /tmp/kiac-e2e-traffic server --listen :8080 >/tmp/kiac-e2e-server.log 2>&1 &'
  retry 30 1 wait_for_server "${TRAFFIC_POD}"
}

wait_for_lb_ip() {
  local namespace=$1
  local service=$2
  local ip
  ip=$(k -n "${namespace}" get svc "${service}" -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
  [[ -n "${ip}" ]]
}

edge_proxy_running() {
  container exec "$1" sh -c '
for process in /proc/[0-9]*; do
  [ "$(readlink "$process/exe" 2>/dev/null)" = /usr/local/bin/kiac-edge-proxy ] && exit 0
done
exit 1
'
}

expect_can_i() {
  local node=$1
  local want=$2
  shift 2
  local got
  got=$(container exec "${node}" kubectl --kubeconfig /etc/kiac/kubeconfig auth can-i "$@" || true)
  got=$(printf '%s\n' "${got}" | tr -d '\r' | tail -n1)
  if [[ "${got}" != "${want}" ]]; then
    printf 'edge proxy RBAC: can-i %s returned %q, want %q\n' "$*" "${got}" "${want}" >&2
    return 1
  fi
}

install_traffic_binary() {
  local node=$1
  container exec -i "${node}" sh -c 'cat > /tmp/kiac-e2e-traffic && chmod 0755 /tmp/kiac-e2e-traffic' < "${TRAFFIC_BIN}"
}

forget_cluster() {
  local name=$1
  local next="${KIAC_E2E_STATE_FILE}.next"
  awk -v name="${name}" '$0 != name' "${KIAC_E2E_STATE_FILE}" > "${next}"
  mv "${next}" "${KIAC_E2E_STATE_FILE}"
}

test_upload() {
  local sender=$1
  local ingress=$2
  local family=$3
  local ip
  if [[ "${family}" == v6 ]]; then
    ip=$(node_ipv6 "${ingress}")
  else
    ip=$(node_ipv4 "${ingress}")
  fi
  [[ -n "${ip}" ]] || {
    printf 'node %s has no %s address\n' "${ingress}" "${family}" >&2
    return 1
  }
  container exec "${sender}" /tmp/kiac-e2e-traffic upload \
    --url "http://$(url_host "${ip}"):${UPLOAD_NODE_PORT}/upload" --bytes 1048576
}

test_gateway() {
  k apply -f "${ROOT}/test/e2e/gateway-route.yaml"
  k -n kiac-gateway wait --for=condition=Programmed gateway/kiac --timeout=180s
  local gateway_ip
  retry 30 2 wait_for_lb_ip kiac-gateway traefik
  gateway_ip=$(k -n kiac-gateway get svc traefik -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  retry 20 2 curl --noproxy '*' -gfsS --max-time 10 \
    -H 'Host: runtime.kiac.test' "http://$(url_host "${gateway_ip}")/healthz"
}

test_observability() {
  k -n kiac-observability rollout status deployment/prometheus --timeout=180s
  k -n kiac-observability rollout status deployment/grafana --timeout=180s
  retry 30 2 wait_for_lb_ip kiac-observability grafana
  local grafana_ip
  grafana_ip=$(k -n kiac-observability get svc grafana -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  retry 20 2 curl --noproxy '*' -gfsS --max-time 10 \
    "http://$(url_host "${grafana_ip}"):3000/api/health" >/dev/null
}

assert_k3s_node_addresses() {
  local node vm_ip internal_ips kindnet_ip node_exporter_ip saved_url live_url
  local cp="kiac-${CURRENT_CLUSTER}-control-plane"
  local server_url="https://$(node_ipv4 "${cp}"):6443"
  for node in ${CURRENT_NODES}; do
    vm_ip=$(node_ipv4 "${node}")
    internal_ips=$(k get node "${node}" -o \
      'jsonpath={range .status.addresses[?(@.type=="InternalIP")]}{.address}{" "}{end}')
    [[ " ${internal_ips} " == *" ${vm_ip} "* ]] || {
      printf '%s InternalIPs %q do not include current VM address %s\n' \
        "${node}" "${internal_ips}" "${vm_ip}" >&2
      return 1
    }
    kindnet_ip=$(k -n kube-system get pod -l k8s-app=kindnet \
      --field-selector "spec.nodeName=${node}" -o jsonpath='{.items[0].status.podIP}')
    [[ "${kindnet_ip}" == "${vm_ip}" ]] || {
      printf '%s kindnet PodIP %q does not match current VM address %s\n' \
        "${node}" "${kindnet_ip}" "${vm_ip}" >&2
      return 1
    }
    node_exporter_ip=$(k -n kiac-observability get pod -l app=node-exporter \
      --field-selector "spec.nodeName=${node}" -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)
    if [[ -n "${node_exporter_ip}" && "${node_exporter_ip}" != "${vm_ip}" ]]; then
      printf '%s node-exporter PodIP %q does not match current VM address %s\n' \
        "${node}" "${node_exporter_ip}" "${vm_ip}" >&2
      return 1
    fi
    if [[ "${node}" != *-control-plane ]]; then
      saved_url=$(container exec "${node}" cat /etc/kiac/k3s-server-url | tr -d '\r\n')
      live_url=$(container exec "${node}" sh -c \
        "tr '\\000' '\\n' < /proc/1/environ | sed -n 's/^K3S_URL=//p' | head -n1" | tr -d '\r\n')
      [[ "${saved_url}" == "${server_url}" && "${live_url}" == "${server_url}" ]] || {
        printf '%s k3s server URLs saved=%q live=%q, want %q\n' \
          "${node}" "${saved_url}" "${live_url}" "${server_url}" >&2
        return 1
      }
    fi
  done
}

run_cluster() {
  local label=$1
  local distro=$2
  local family=$3
  local workers=$4
  local features=$5
  local restart=$6
  local name="${KIAC_E2E_PREFIX}-${label}"
  local cp="kiac-${name}-control-plane"
  local sender="kiac-${name}-worker-1"
  local target="kiac-${name}-worker-2"
  local ingress="kiac-${name}-worker-3"
  local expected=$((workers + 1))
  local mount_dir="${TMPDIR:-/tmp}/kiac-e2e-${RUN_ID}/${name} host data"
  mkdir -p "${mount_dir}"
  printf 'host-sentinel\n' > "${mount_dir}/sentinel"
  CURRENT_MOUNT_DIR=${mount_dir}

  CURRENT_CLUSTER=${name}
  CURRENT_CONTEXT="kiac-${name}"
  CURRENT_NODES=${cp}
  local node_index
  for ((node_index = 1; node_index <= workers; node_index++)); do
    CURRENT_NODES+=" kiac-${name}-worker-${node_index}"
  done
  printf '%s\n' "${name}" >> "${KIAC_E2E_STATE_FILE}"

  local create=(create cluster --name "${name}" --distro "${distro}" --workers "${workers}" --ip-family "${family}" --wait 8m
    --mount "type=bind,source=${mount_dir},target=/kiac-e2e-host")
  if [[ "${features}" == true ]]; then
    create+=(--gateway --observability)
  fi

  printf '\n=== %s: %s %s, %s workers ===\n' "${label}" "${distro}" "${family}" "${workers}"
  "${KIAC_BIN}" "${create[@]}"

  k wait --for=condition=Ready nodes --all --timeout=300s
  local actual
  actual=$(k get nodes --no-headers | awk 'NF {count++} END {print count+0}')
  [[ "${actual}" -eq "${expected}" ]] || {
    printf 'got %s Kubernetes nodes, want %s\n' "${actual}" "${expected}" >&2
    return 1
  }

  local default_sc
  default_sc=$(k get storageclass -o jsonpath='{.items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")].metadata.name}')
  [[ -n "${default_sc}" ]] || {
    printf 'no default StorageClass found\n' >&2
    return 1
  }
  retry 18 10 wait_for_metrics

  for node in ${CURRENT_NODES}; do
    [[ "$(container exec "${node}" cat /kiac-e2e-host/sentinel | tr -d '\r\n')" == host-sentinel ]]
  done

  local i node
  for ((i = 0; i <= workers; i++)); do
    if [[ ${i} -eq 0 ]]; then
      node=${cp}
    else
      node="kiac-${name}-worker-${i}"
    fi
    retry 20 1 edge_proxy_running "${node}"
  done

  k apply -f "${ROOT}/test/e2e/workload.yaml"
  UPLOAD_NODE_PORT=$(k -n kiac-e2e get service upload-server -o jsonpath='{.spec.ports[0].nodePort}')
  k apply -f "${ROOT}/test/e2e/hostpath.yaml"
  k -n kiac-e2e rollout status deployment/hostpath --timeout=180s
  [[ "$(k -n kiac-e2e exec deployment/hostpath -- cat /host/sentinel | tr -d '\r\n')" == host-sentinel ]]
  k -n kiac-e2e exec deployment/hostpath -- sh -c 'printf pod-write > /host/from-pod'
  [[ "$(cat "${mount_dir}/from-pod")" == pod-write ]]
  k -n kiac-e2e patch deployment upload-server --type=merge \
    -p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"${target}\"}}}}}"
  k -n kiac-e2e rollout status deployment/upload-server --timeout=180s
  start_traffic_server
  k -n kiac-e2e wait --for=jsonpath='{.endpoints[0].conditions.ready}'=true \
    endpointslice -l kubernetes.io/service-name=upload-server --timeout=120s

  # The edge proxy reconciles Services and EndpointSlices every three
  # seconds. Let one complete pass observe the newly ready endpoint so
  # this test measures the converged upload path, not API propagation.
  sleep 5

  install_traffic_binary "${sender}"
  retry 5 2 test_upload "${sender}" "${ingress}" v4
  if [[ "${family}" == dual ]]; then
    retry 5 2 test_upload "${sender}" "${ingress}" v6
  fi

  local ingress_ip
  ingress_ip=$(node_ipv4 "${ingress}")
  container exec "${sender}" /tmp/kiac-e2e-traffic reject --addr "${ingress_ip}:15080"

  expect_can_i "${ingress}" yes list services --all-namespaces
  expect_can_i "${ingress}" yes list endpointslices.discovery.k8s.io --all-namespaces
  expect_can_i "${ingress}" yes list nodes
  expect_can_i "${ingress}" no get secrets --all-namespaces
  expect_can_i "${ingress}" no create pods --all-namespaces

  retry 30 2 wait_for_lb_ip kiac-e2e upload-server
  local lb_ip
  lb_ip=$(k -n kiac-e2e get svc upload-server -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  retry 20 2 curl --noproxy '*' -gfsS --max-time 10 \
    "http://$(url_host "${lb_ip}"):8080/healthz" >/dev/null

  if [[ "${features}" == true ]]; then
    test_gateway
    test_observability
  fi

  if [[ "${restart}" == true ]]; then
    if [[ "${distro}" == k3s ]]; then
      # The single-node command delegates to the k3s reconciler so a new
      # node address cannot leave stale hostNetwork metadata behind.
      "${KIAC_BIN}" stop node worker-1 --name "${name}"
      "${KIAC_BIN}" start node worker-1 --name "${name}"
      [[ "$(container exec "${sender}" cat /kiac-e2e-host/sentinel | tr -d '\r\n')" == host-sentinel ]]
      assert_k3s_node_addresses
      retry 30 1 edge_proxy_running "${sender}"
      install_traffic_binary "${sender}"
      retry 5 2 test_upload "${sender}" "${ingress}" v4

      # Reproduce a host reboot without resetting vmnet for unrelated
      # clusters on the runner: halt every VM, then exercise the k3s-aware
      # resume path against the resulting fresh addresses.
      for ((i = 1; i <= workers; i++)); do
        "${KIAC_BIN}" stop node "worker-${i}" --name "${name}"
      done
      "${KIAC_BIN}" stop node control-plane --name "${name}"
      "${KIAC_BIN}" get clusters -o json | grep -F "\"name\": \"${name}\"" >/dev/null
      "${KIAC_BIN}" resume cluster --name "${name}" --wait 8m
      k wait --for=condition=Ready nodes --all --timeout=300s
      assert_k3s_node_addresses
      for node in ${CURRENT_NODES}; do
        [[ "$(container exec "${node}" cat /kiac-e2e-host/sentinel | tr -d '\r\n')" == host-sentinel ]]
      done
      retry 30 2 k -n kiac-e2e exec deployment/hostpath -- test -f /host/from-pod
      [[ "$(k -n kiac-e2e exec deployment/hostpath -- cat /host/from-pod | tr -d '\r\n')" == pod-write ]]
      for node in ${CURRENT_NODES}; do
        retry 30 1 edge_proxy_running "${node}"
      done

      # Every workload container and each VM's /tmp restarted. Restore only
      # the test probes, then repeat the original cross-node data path.
      start_traffic_server
      sleep 5
      install_traffic_binary "${sender}"
      retry 5 2 test_upload "${sender}" "${ingress}" v4
      if [[ "${features}" == true ]]; then
        test_gateway
        test_observability
      fi
      "${KIAC_BIN}" verify cluster --name "${name}"
    else
      "${KIAC_BIN}" stop node worker-1 --name "${name}"
      "${KIAC_BIN}" get clusters -o json | grep -F "${name}" >/dev/null
      "${KIAC_BIN}" start node worker-1 --name "${name}"
      k wait --for=condition=Ready "node/${sender}" --timeout=300s
      retry 30 1 edge_proxy_running "${sender}"
      [[ "$(container exec "${sender}" cat /kiac-e2e-host/sentinel | tr -d '\r\n')" == host-sentinel ]]
      # The node VM keeps its root disk but receives a fresh /tmp on boot.
      # Reinstall only the test client; the product proxy in /usr/local/bin
      # must have persisted and is checked immediately above.
      install_traffic_binary "${sender}"
      retry 5 2 test_upload "${sender}" "${ingress}" v4
    fi
  fi

  "${KIAC_BIN}" delete cluster --name "${name}"
  [[ "$(cat "${mount_dir}/sentinel")" == host-sentinel ]]
  [[ "$(cat "${mount_dir}/from-pod")" == pod-write ]]
  rm -rf -- "${mount_dir}"
  CURRENT_MOUNT_DIR=""
  forget_cluster "${name}"
  CURRENT_CLUSTER=""
  CURRENT_CONTEXT=""
  CURRENT_NODES=""
  UPLOAD_NODE_PORT=""
}

printf 'Building kiac and the runtime traffic probe with %s\n' "$(go version)"
make -C "${ROOT}" build
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=false -trimpath \
  -o "${TRAFFIC_BIN}" "${ROOT}/test/e2e/traffic"
"${KIAC_BIN}" doctor --fix

case "${PROFILE}" in
  quick)
    run_cluster ka kubeadm ipv4 3 true true
    run_cluster k3 k3s ipv4 3 true true
    ;;
  kubeadm)
    run_cluster ka kubeadm ipv4 3 true true
    ;;
  k3s)
    run_cluster k3 k3s ipv4 3 true true
    ;;
  dual)
    run_cluster kad kubeadm dual 3 true false
    run_cluster k3d k3s dual 3 true false
    ;;
  full)
    run_cluster ka kubeadm ipv4 3 true true
    run_cluster k3 k3s ipv4 3 true true
    run_cluster kad kubeadm dual 3 true false
    run_cluster k3d k3s dual 3 true false
    ;;
  *)
    printf 'unknown runtime profile %q (quick, kubeadm, k3s, dual, full)\n' "${PROFILE}" >&2
    exit 2
    ;;
esac

printf '\nRuntime profile %s passed.\n' "${PROFILE}"

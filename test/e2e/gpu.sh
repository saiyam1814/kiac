#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
RUN_ID=${KIAC_E2E_RUN_ID:-$(date +%s)}
RUN_ID=$(printf '%s' "${RUN_ID}" | tr -cd 'a-zA-Z0-9-' | cut -c1-20)
export KIAC_E2E_PREFIX=${KIAC_E2E_PREFIX:-"gpu-${RUN_ID}"}
export KIAC_E2E_STATE_FILE=${KIAC_E2E_STATE_FILE:-"${TMPDIR:-/tmp}/kiac-gpu-e2e-${RUN_ID}.clusters"}
export KIAC_E2E_KUBECONFIG=${KIAC_E2E_KUBECONFIG:-"${TMPDIR:-/tmp}/kiac-gpu-e2e-${RUN_ID}.kubeconfig"}
export KUBECONFIG=${KIAC_E2E_KUBECONFIG}

KIAC_BIN=${KIAC_BIN:-"${ROOT}/bin/kiac"}
TRAFFIC_BIN="${ROOT}/dist/e2e/kiac-e2e-traffic-linux-arm64"
CURRENT_CLUSTER=""
CURRENT_CONTEXT=""
PORT_FORWARD_PID=""

umask 077
mkdir -p "$(dirname "${TRAFFIC_BIN}")" "$(dirname "${KIAC_E2E_STATE_FILE}")" "$(dirname "${KUBECONFIG}")"
touch "${KIAC_E2E_STATE_FILE}" "${KUBECONFIG}"

k() {
  kubectl --context "${CURRENT_CONTEXT}" --request-timeout=30s "$@"
}

retry() {
  local attempts=$1
  local delay=$2
  shift 2
  local attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if "$@"; then
      return 0
    fi
    sleep "${delay}"
  done
  return 1
}

wait_for_lb() {
  local namespace=$1
  local service=$2
  [[ -n "$(k -n "${namespace}" get svc "${service}" -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)" ]]
}

grafana_is_healthy() {
  local port=$1
  local response
  response=$(curl --noproxy '*' -fsS --max-time 5 "http://127.0.0.1:${port}/api/health" 2>/dev/null) || return 1
  grep -Eq '"database"[[:space:]]*:[[:space:]]*"ok"' <<<"${response}"
}

diagnostics() {
  [[ -n "${CURRENT_CLUSTER}" ]] || return 0
  printf '\nGPU runtime diagnostics for %s\n' "${CURRENT_CLUSTER}" >&2
  "${KIAC_BIN}" get nodes --name "${CURRENT_CLUSTER}" >&2 || true
  "${KIAC_BIN}" gpu status --name "${CURRENT_CLUSTER}" -o json >&2 || true
  k get nodes -o wide >&2 || true
  k get pods -A -o wide >&2 || true
  k get events -A --sort-by=.lastTimestamp >&2 || true
  k get deviceclass,resourceslices.resource.k8s.io -o wide >&2 || true
}

finish() {
  local status=$?
  if [[ -n "${PORT_FORWARD_PID}" ]]; then
    kill "${PORT_FORWARD_PID}" 2>/dev/null || true
    wait "${PORT_FORWARD_PID}" 2>/dev/null || true
  fi
  if [[ ${status} -ne 0 ]]; then
    diagnostics
  fi
  if [[ ${status} -ne 0 && "${KIAC_E2E_KEEP_ON_FAILURE:-false}" == true ]]; then
    printf 'Retained %s for debugging.\n' "${CURRENT_CLUSTER}" >&2
  else
    "${ROOT}/test/e2e/cleanup.sh"
  fi
  exit "${status}"
}

trap 'exit 130' INT
trap 'exit 143' TERM
trap finish EXIT

forget_cluster() {
  local name=$1
  local next="${KIAC_E2E_STATE_FILE}.next"
  awk -v name="${name}" '$0 != name' "${KIAC_E2E_STATE_FILE}" > "${next}"
  mv "${next}" "${KIAC_E2E_STATE_FILE}"
}

assert_gpu_inventory() {
  local driver=$1
  local gpu_workers=$2
  local count status
  count=$(k get nodes -l kiac.dev/gpu.present=true --no-headers | awk 'NF {n++} END {print n+0}')
  [[ "${count}" -eq "${gpu_workers}" ]]
  [[ "$(k get nodes -l kiac.dev/gpu.present=true -o 'go-template={{range .items}}{{index .metadata.labels "kiac.dev/gpu.api"}}{{"\n"}}{{end}}' | sort -u)" == venus ]]
  if k get nodes -o json | grep -q 'nvidia.com/gpu'; then
    printf 'a node incorrectly advertises nvidia.com/gpu\n' >&2
    return 1
  fi
  status=$("${KIAC_BIN}" gpu status --name "${CURRENT_CLUSTER}" -o json)
  grep -F "\"driver\": \"${driver}\"" <<<"${status}" >/dev/null
  [[ "$(grep -c '"schedulable": true' <<<"${status}")" -eq "${gpu_workers}" ]]
  if [[ "${driver}" == dra ]]; then
    [[ "$(k get resourceslices.resource.k8s.io -o 'go-template={{range .items}}{{if eq .spec.driver "gpu.kiac.dev"}}{{.spec.nodeName}}{{"\n"}}{{end}}{{end}}' | sort -u | awk 'NF {n++} END {print n+0}')" -eq "${gpu_workers}" ]]
  else
    [[ "$(k get nodes -l kiac.dev/gpu.present=true -o 'go-template={{range .items}}{{index .status.capacity "kiac.dev/gpu"}}{{"\n"}}{{end}}' | grep -c '^1$')" -eq "${gpu_workers}" ]]
  fi
}

test_vulkan_pair() {
  k apply -f "${ROOT}/test/e2e/gpu-vulkan-pair.yaml"
  k wait --for=jsonpath='{.status.phase}'=Succeeded pod/kiac-gpu-vulkan-a pod/kiac-gpu-vulkan-b --timeout=8m
  k logs kiac-gpu-vulkan-a | grep -F 'Virtio-GPU Venus' >/dev/null
  k logs kiac-gpu-vulkan-b | grep -F 'Virtio-GPU Venus' >/dev/null
  local node_a node_b
  node_a=$(k get pod kiac-gpu-vulkan-a -o jsonpath='{.spec.nodeName}')
  node_b=$(k get pod kiac-gpu-vulkan-b -o jsonpath='{.spec.nodeName}')
  [[ "${node_a}" != "${node_b}" ]]
  k delete -f "${ROOT}/test/e2e/gpu-vulkan-pair.yaml" --wait=true
}

test_vulkan_single() {
  k apply -f "${ROOT}/examples/gpu-vulkan.yaml"
  k wait --for=jsonpath='{.status.phase}'=Succeeded pod/kiac-gpu-vulkan --timeout=8m
  k logs kiac-gpu-vulkan | grep -F 'KIAC_REAL_APPLE_GPU_OK' >/dev/null
  k logs kiac-gpu-vulkan | grep -F 'Virtio-GPU Venus' >/dev/null
  k delete -f "${ROOT}/examples/gpu-vulkan.yaml" --wait=true
}

test_dra_memory() {
  k apply -f "${ROOT}/examples/gpu-dra-memory.yaml"
  k wait --for=condition=Ready pod/kiac-gpu-dra-memory --timeout=5m
  [[ "$(k get resourceclaim kiac-gpu-memory-8gi -o jsonpath='{.status.allocation.devices.results[0].driver}')" == gpu.kiac.dev ]]
  k exec kiac-gpu-dra-memory -- test -c /dev/dri/renderD128
  k delete -f "${ROOT}/examples/gpu-dra-memory.yaml" --wait=true
}

dra_overflow_pending() {
  [[ "$(k get pod kiac-gpu-capacity-overflow -o jsonpath='{.status.phase}')" == Pending ]]
  [[ -z "$(k get resourceclaim kiac-gpu-capacity-overflow -o jsonpath='{.status.allocation}')" ]]
}

test_dra_capacity() {
  local node memory_mib capacity_gib remainder manifest overflow events
  node="kiac-${CURRENT_CLUSTER}-gpu-1"
  memory_mib=$(k get node "${node}" -o 'go-template={{index .metadata.labels "kiac.dev/gpu.memory"}}')
  capacity_gib=$((memory_mib / 1024))
  if [[ "${capacity_gib}" -le 8 ]]; then
    printf 'GPU window on %s is only %s GiB; need more than 8 GiB for capacity test\n' "${node}" "${capacity_gib}" >&2
    return 1
  fi
  remainder=$((capacity_gib - 8))
  manifest="${TMPDIR:-/tmp}/kiac-gpu-dra-capacity-${RUN_ID}.yaml"
  overflow="${TMPDIR:-/tmp}/kiac-gpu-dra-capacity-overflow-${RUN_ID}.yaml"
  sed -e "s/__NODE__/${node}/g" -e "s/__REMAINDER__/${remainder}/g" \
    "${ROOT}/test/e2e/gpu-dra-capacity.yaml.tmpl" > "${manifest}"
  sed -e "s/__NODE__/${node}/g" \
    "${ROOT}/test/e2e/gpu-dra-capacity-overflow.yaml.tmpl" > "${overflow}"
  k apply -f "${manifest}"
  k wait --for=condition=Ready pod/kiac-gpu-capacity-a pod/kiac-gpu-capacity-b --timeout=5m
  k apply -f "${overflow}"
  retry 30 2 dra_overflow_pending
  events=$(k get events --field-selector involvedObject.kind=Pod,involvedObject.name=kiac-gpu-capacity-overflow -o jsonpath='{range .items[*]}{.reason}{" "}{.message}{"\n"}{end}')
  grep -Eiq 'allocat|capacity|resource' <<<"${events}"
  k delete -f "${overflow}" --wait=true
  k delete -f "${manifest}" --wait=true
  rm -f "${manifest}" "${overflow}"
}

test_gpu_compatibility() {
  k create namespace kiac-gpu-compat-e2e
  "${KIAC_BIN}" gpu compat enable --name "${CURRENT_CLUSTER}" --namespace kiac-gpu-compat-e2e
  local before after status
  before=$(k -n kube-system get deployment kiac-gpu-compat -o 'go-template={{index .spec.template.metadata.annotations "kiac.dev/tls-id"}}')
  status=$("${KIAC_BIN}" gpu status --name "${CURRENT_CLUSTER}" -o json)
  grep -F '"ready": true' <<<"${status}" >/dev/null
  "${KIAC_BIN}" gpu compat enable --name "${CURRENT_CLUSTER}" --namespace kiac-gpu-compat-e2e --rotate-certificate
  after=$(k -n kube-system get deployment kiac-gpu-compat -o 'go-template={{index .spec.template.metadata.annotations "kiac.dev/tls-id"}}')
  [[ -n "${before}" && -n "${after}" && "${before}" != "${after}" ]]
  k apply -f "${ROOT}/test/e2e/gpu-compat.yaml"
  k wait --for=jsonpath='{.status.phase}'=Succeeded pod/kiac-gpu-compat -n kiac-gpu-compat-e2e --timeout=8m
  [[ "$(k get pod kiac-gpu-compat -n kiac-gpu-compat-e2e -o 'go-template={{index .metadata.annotations "kiac.dev/rewrote-gpu-resource"}}')" == nvidia.com/gpu ]]
  k logs pod/kiac-gpu-compat -n kiac-gpu-compat-e2e | grep -F 'Virtio-GPU Venus' >/dev/null
  if k get nodes -o json | grep -q 'nvidia.com/gpu'; then
    printf 'compatibility webhook leaked nvidia.com/gpu into node capacity\n' >&2
    return 1
  fi
  k delete -f "${ROOT}/test/e2e/gpu-compat.yaml" --wait=true
  "${KIAC_BIN}" gpu compat disable --name "${CURRENT_CLUSTER}" --namespace kiac-gpu-compat-e2e
  k delete namespace kiac-gpu-compat-e2e --wait=true
}

start_traffic_server() {
  local pod
  k apply -f "${ROOT}/test/e2e/gpu-traffic.yaml"
  k -n kiac-e2e rollout status deployment/upload-server --timeout=5m
  k -n kiac-e2e wait --for=condition=Ready pod/upload-sender --timeout=5m
  pod=$(k -n kiac-e2e get pod -l app=upload-server -o jsonpath='{.items[0].metadata.name}')
  k -n kiac-e2e cp "${TRAFFIC_BIN}" "${pod}:/tmp/kiac-e2e-traffic"
  k -n kiac-e2e cp "${TRAFFIC_BIN}" upload-sender:/tmp/kiac-e2e-traffic
  k -n kiac-e2e exec "${pod}" -- chmod 0755 /tmp/kiac-e2e-traffic
  k -n kiac-e2e exec upload-sender -- chmod 0755 /tmp/kiac-e2e-traffic
  k -n kiac-e2e exec "${pod}" -- sh -c 'nohup /tmp/kiac-e2e-traffic server --listen :8080 >/tmp/server.log 2>&1 &'
  retry 30 1 k -n kiac-e2e exec "${pod}" -- wget -q -O - http://127.0.0.1:8080/healthz
}

network_policy_denies_upload_server() {
  ! k -n kiac-e2e exec upload-sender -- wget -q -T 2 -O - http://upload-server:8080/healthz
}

test_k3s_network_policy() {
  retry 20 1 k -n kiac-e2e exec upload-sender -- wget -q -T 5 -O - http://upload-server:8080/healthz
  k apply -f "${ROOT}/test/e2e/gpu-network-policy.yaml"
  retry 30 1 network_policy_denies_upload_server
  k delete -f "${ROOT}/test/e2e/gpu-network-policy.yaml" --wait=true
  retry 30 1 k -n kiac-e2e exec upload-sender -- wget -q -T 5 -O - http://upload-server:8080/healthz
}

test_data_paths() {
  start_traffic_server
  test_k3s_network_policy
  retry 30 2 wait_for_lb kiac-e2e upload-server
  local lb_ip
  lb_ip=$(k -n kiac-e2e get svc upload-server -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  k -n kiac-e2e exec upload-sender -- /tmp/kiac-e2e-traffic upload \
    --url "http://${lb_ip}:8080/upload" --bytes 33554432
  retry 20 2 curl --noproxy '*' -fsS --max-time 15 "http://${lb_ip}:8080/healthz"

  k apply -f "${ROOT}/test/e2e/gateway-route.yaml"
  k -n kiac-gateway wait --for=condition=Programmed gateway/kiac --timeout=5m
  retry 30 2 wait_for_lb kiac-gateway traefik
  local gateway_ip
  gateway_ip=$(k -n kiac-gateway get svc traefik -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  retry 20 2 curl --noproxy '*' -fsS --max-time 15 -H 'Host: runtime.kiac.test' "http://${gateway_ip}/healthz"

  k apply -f "${ROOT}/examples/statefulset.yaml"
  k rollout status statefulset/notes --timeout=5m
  k get pvc -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -q '^Bound$'

  k -n kiac-observability rollout status deployment/prometheus --timeout=5m
  k -n kiac-observability rollout status deployment/grafana --timeout=5m
  retry 30 2 wait_for_lb kiac-observability grafana

  # The sender is deliberately restartPolicy: Never. Remove the transient
  # traffic namespace before reboot testing so verify does not mistake the
  # completed probe for an unhealthy application workload.
  k delete namespace kiac-e2e --wait=true
}

test_inference() {
  k apply -f "${ROOT}/examples/gpu-inference.yaml"
  k -n kiac-gpu-demo rollout status deployment/tinyllama --timeout=10m
  retry 30 2 wait_for_lb kiac-gpu-demo tinyllama
  local ip response
  ip=$(k -n kiac-gpu-demo get svc tinyllama -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  response=$(curl --noproxy '*' -fsS --max-time 90 "http://${ip}:8080/completion" \
    -H 'Content-Type: application/json' \
    -d '{"prompt":"Kubernetes on Apple GPUs is","n_predict":16}')
  grep -F '"tokens_predicted":16' <<<"${response}" >/dev/null
  k -n kiac-gpu-demo logs deployment/tinyllama | grep -F 'Virtio-GPU Venus' >/dev/null
  k -n kiac-gpu-demo logs deployment/tinyllama | grep -F 'offloaded 23/23 layers to GPU' >/dev/null
}

test_internal_observability() {
  k -n kiac-observability rollout status deployment/prometheus --timeout=5m
  k -n kiac-observability rollout status deployment/grafana --timeout=5m
  k -n kiac-observability rollout status deployment/kube-state-metrics --timeout=5m
  k -n kiac-observability rollout status daemonset/node-exporter --timeout=5m
  [[ "$(k -n kiac-observability get service grafana -o jsonpath='{.spec.type}')" == ClusterIP ]]
  [[ -z "$(k -n kiac-observability get service grafana -o jsonpath='{.status.loadBalancer.ingress[0].ip}')" ]]

  local log port
  log="${TMPDIR:-/tmp}/kiac-gpu-port-forward-${RUN_ID}.log"
  kubectl --context "${CURRENT_CONTEXT}" -n kiac-observability port-forward service/grafana :3000 >"${log}" 2>&1 &
  PORT_FORWARD_PID=$!
  retry 30 1 grep -qE 'Forwarding from 127\.0\.0\.1:[0-9]+' "${log}"
  port=$(sed -nE 's/.*Forwarding from 127\.0\.0\.1:([0-9]+).*/\1/p' "${log}" | head -1)
  retry 30 1 grafana_is_healthy "${port}"
  kill "${PORT_FORWARD_PID}" 2>/dev/null || true
  wait "${PORT_FORWARD_PID}" 2>/dev/null || true
  PORT_FORWARD_PID=""
  rm -f "${log}"
}

test_support_bundle() {
  local bundle root listing
  bundle="${TMPDIR:-/tmp}/kiac-gpu-support-${RUN_ID}-${CURRENT_CLUSTER}.tar.gz"
  root="kiac-support-${CURRENT_CLUSTER}"
  listing="${TMPDIR:-/tmp}/kiac-gpu-support-${RUN_ID}-${CURRENT_CLUSTER}.list"
  "${KIAC_BIN}" support bundle --name "${CURRENT_CLUSTER}" --output "${bundle}" --timeout 20s
  [[ "$(stat -f '%Lp' "${bundle}")" == 600 ]]
  tar -tzf "${bundle}" > "${listing}"
  grep -Fx "${root}/metadata.json" "${listing}" >/dev/null
  grep -Fx "${root}/cluster/verify.json" "${listing}" >/dev/null
  grep -Fx "${root}/kubernetes/gpu-driver.txt" "${listing}" >/dev/null
  grep -Fx "${root}/nodes/kiac-${CURRENT_CLUSTER}-gpu-1/gpu.txt" "${listing}" >/dev/null
  tar -xOzf "${bundle}" "${root}/metadata.json" | grep -F '"gpu"' >/dev/null
  rm -f "${bundle}" "${listing}"
}

stop_all_and_resume() {
  local gpu_workers=$1
  local workers=$2
  local index
  for ((index = 1; index <= gpu_workers; index++)); do
    "${KIAC_BIN}" stop node "gpu-${index}" --name "${CURRENT_CLUSTER}"
  done
  for ((index = 1; index <= workers; index++)); do
    "${KIAC_BIN}" stop node "worker-${index}" --name "${CURRENT_CLUSTER}"
  done
  "${KIAC_BIN}" stop node control-plane --name "${CURRENT_CLUSTER}"
  "${KIAC_BIN}" resume cluster --name "${CURRENT_CLUSTER}" --wait 10m
  k wait --for=condition=Ready nodes --all --timeout=8m
}

run_gpu_cluster() {
  local label=$1
  local distro=$2
  local version=$3
  local driver=$4
  local gpu_workers=$5
  local workers=$6
  local profile=$7
  local name="${KIAC_E2E_PREFIX}-${label}"
  CURRENT_CLUSTER=${name}
  CURRENT_CONTEXT="kiac-${name}"
  printf '%s\n' "${name}" >> "${KIAC_E2E_STATE_FILE}"

  local create=(create cluster --name "${name}" --distro "${distro}" --k8s-version "${version}"
    --workers "${workers}" --gpu-workers "${gpu_workers}" --gpu-resource-driver "${driver}"
    --cpus 4 --memory 3G --cp-memory 6G --wait 10m)
  if [[ "${profile}" == full ]]; then
    create+=(--gateway --observability)
  elif [[ "${profile}" == no-lb-observability ]]; then
    create+=(--no-lb --observability)
  fi
  printf '\n=== Real Apple GPU: %s %s, %s, %s GPU worker(s) ===\n' "${distro}" "${version}" "${driver}" "${gpu_workers}"
  "${KIAC_BIN}" "${create[@]}"
  k wait --for=condition=Ready nodes --all --timeout=8m
  [[ "$(k get nodes --no-headers | awk 'NF {n++} END {print n+0}')" -eq "$((gpu_workers + workers + 1))" ]]
  assert_gpu_inventory "${driver}" "${gpu_workers}"

  if [[ "${gpu_workers}" -ge 2 ]]; then
    test_vulkan_pair
  else
    test_vulkan_single
  fi
  if [[ "${driver}" == dra ]]; then
    test_dra_memory
    test_dra_capacity
    test_gpu_compatibility
  fi
  if [[ "${profile}" == full ]]; then
    test_data_paths
    test_inference
    "${KIAC_BIN}" stop node gpu-1 --name "${name}"
    "${KIAC_BIN}" start node gpu-1 --name "${name}"
    k wait --for=condition=Ready nodes --all --timeout=8m
    assert_gpu_inventory "${driver}" "${gpu_workers}"
    stop_all_and_resume "${gpu_workers}" "${workers}"
    assert_gpu_inventory "${driver}" "${gpu_workers}"
    k -n kiac-gpu-demo rollout status deployment/tinyllama --timeout=10m
  elif [[ "${profile}" == no-lb-observability ]]; then
    [[ "$(k get node "kiac-${name}-control-plane" -o 'go-template={{index .metadata.labels "kiac.io/lb-primary"}}')" == true ]]
    test_internal_observability
    "${KIAC_BIN}" stop node gpu-1 --name "${name}"
    "${KIAC_BIN}" start node gpu-1 --name "${name}"
    k wait --for=condition=Ready nodes --all --timeout=8m
    assert_gpu_inventory "${driver}" "${gpu_workers}"
    stop_all_and_resume "${gpu_workers}" "${workers}"
    assert_gpu_inventory "${driver}" "${gpu_workers}"
    test_internal_observability
  fi
  "${KIAC_BIN}" verify cluster --name "${name}"
  test_support_bundle
  "${KIAC_BIN}" delete cluster --name "${name}"
  forget_cluster "${name}"
  CURRENT_CLUSTER=""
  CURRENT_CONTEXT=""
}

printf 'Building Kiac and GPU runtime probes with %s\n' "$(go version)"
make -C "${ROOT}" build
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=false -trimpath \
  -o "${TRAFFIC_BIN}" "${ROOT}/test/e2e/traffic"
"${KIAC_BIN}" gpu doctor

case "${KIAC_GPU_E2E_PROFILE:-all}" in
  all)
    run_gpu_cluster k3s k3s 1.36 dra 2 1 full
    run_gpu_cluster kubeadm kubeadm 1.37 device-plugin 1 0 no-lb-observability
    ;;
  k3s)
    run_gpu_cluster k3s k3s 1.36 dra 2 1 full
    ;;
  kubeadm)
    run_gpu_cluster kubeadm kubeadm 1.37 device-plugin 1 0 no-lb-observability
    ;;
  *)
    printf 'Unknown KIAC_GPU_E2E_PROFILE=%s (use all, k3s, or kubeadm).\n' "${KIAC_GPU_E2E_PROFILE}" >&2
    exit 2
    ;;
esac

printf '\nReal Apple GPU runtime profile passed.\n'

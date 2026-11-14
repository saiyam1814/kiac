#!/usr/bin/env bash
# Focused CPU runtime compatibility: two VMs at a time, no host upgrades.
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
EXPECTED_VERSION=${KIAC_EXPECTED_CONTAINER_VERSION:-}
DISTRO=${KIAC_COMPAT_DISTRO:-both}
RUN_ID=${KIAC_E2E_RUN_ID:-$(date +%s)-$$}
RUN_ID=$(printf '%s' "${RUN_ID}" | tr -cd 'a-zA-Z0-9-' | cut -c1-24 | tr '[:upper:]' '[:lower:]')
[[ -n "${RUN_ID}" ]] || { printf 'empty compatibility run ID\n' >&2; exit 2; }
case "${DISTRO}" in both|kubeadm|k3s) ;; *) printf 'invalid KIAC_COMPAT_DISTRO\n' >&2; exit 2 ;; esac
case "${EXPECTED_VERSION}" in 1.6.1|1.7.1) ;; *) printf 'set KIAC_EXPECTED_CONTAINER_VERSION to 1.6.1 or 1.7.1\n' >&2; exit 2 ;; esac

KIAC_BIN=${KIAC_BIN:-"${ROOT}/bin/kiac"}
export KIAC_E2E_PREFIX="ci-compat-${RUN_ID}"
export KIAC_E2E_STATE_FILE=${KIAC_E2E_STATE_FILE:-"${TMPDIR:-/tmp}/kiac-e2e-${RUN_ID}.clusters"}
export KIAC_E2E_KUBECONFIG=${KIAC_E2E_KUBECONFIG:-"${TMPDIR:-/tmp}/kiac-e2e-${RUN_ID}.kubeconfig"}
export KUBECONFIG=${KIAC_E2E_KUBECONFIG}
CURRENT_CLUSTER=""
CURRENT_CONTEXT=""
IMAGE="kiac-e2e-compat:${RUN_ID}"
IMAGE_LOADED=false
STATE_OWNED=false
KUBECONFIG_OWNED=false
WORK=""
umask 077

k() { kubectl --context "${CURRENT_CONTEXT}" --request-timeout=15s "$@"; }

retry() {
  local i
  for ((i = 0; i < 40; i++)); do
    if "$@"; then return 0; fi
    sleep 3
  done
  return 1
}

owned_vms_absent() {
  # Container IDs are exact names; a substring of another cluster is not
  # evidence of a leak. Listing failure is a test failure, not an empty list.
  local names
  names=$(container ls --all --quiet) || return 1
  ! printf '%s\n' "${names}" | grep -Ex "kiac-${CURRENT_CLUSTER}-(control-plane|worker-1)"
}

delete_current() {
  [[ -n "${CURRENT_CLUSTER}" ]] || return 0
  "${KIAC_BIN}" delete cluster --name "${CURRENT_CLUSTER}" || return 1
  owned_vms_absent || return 1
  : > "${KIAC_E2E_STATE_FILE}"
  CURRENT_CLUSTER=""
  CURRENT_CONTEXT=""
}

finish() {
  local status=$?
  trap - EXIT
  if [[ ${status} -ne 0 && -n "${CURRENT_CLUSTER}" ]]; then
    printf '\nCompatibility failure in %s\n' "${CURRENT_CLUSTER}" >&2
    k get nodes -o wide >&2 || true
    k get pods -A -o wide >&2 || true
  fi
  if [[ ${status} -ne 0 && "${KIAC_E2E_KEEP_ON_FAILURE:-false}" == true ]]; then
    printf 'Retained cluster %s; delete with: %s delete cluster --name %s\n' \
      "${CURRENT_CLUSTER}" "${KIAC_BIN}" "${CURRENT_CLUSTER}" >&2
    printf 'Private compatibility files: %s\n' "${WORK}" >&2
  else
    if ! delete_current; then
      printf 'Compatibility cleanup failed; ownership remains recorded in %s\n' "${KIAC_E2E_STATE_FILE}" >&2
      status=1
    fi
    if [[ "${IMAGE_LOADED}" == true ]] && ! container image delete "${IMAGE}"; then
      printf 'Could not remove test image %s\n' "${IMAGE}" >&2
      status=1
    fi
    if [[ -z "${CURRENT_CLUSTER}" ]]; then
      if [[ "${STATE_OWNED}" == true ]]; then rm -f -- "${KIAC_E2E_STATE_FILE}"; fi
      if [[ "${KUBECONFIG_OWNED}" == true ]]; then rm -f -- "${KUBECONFIG}"; fi
    fi
    if [[ -z "${CURRENT_CLUSTER}" && -n "${WORK}" ]]; then rm -rf -- "${WORK}"; fi
  fi
  exit "${status}"
}

trap 'exit 130' INT
trap 'exit 143' TERM
trap finish EXIT

# This is deliberately before mkdir, make, doctor or any runtime mutation.
# status is the actual CLI talking to the actual running daemon; checking
# `container --version` alone cannot detect an upgraded CLI with an old server.
go run "${ROOT}/test/e2e/runtime-preflight" --expected-container-version "${EXPECTED_VERSION}"
command -v kubectl >/dev/null

WORK=$(mktemp -d "${TMPDIR:-/tmp}/kiac-compat-${RUN_ID}.XXXXXX")
mkdir -p "$(dirname "${KIAC_E2E_STATE_FILE}")" "$(dirname "${KUBECONFIG}")"
if [[ -e "${KIAC_E2E_STATE_FILE}" || -L "${KIAC_E2E_STATE_FILE}" || -e "${KUBECONFIG}" || -L "${KUBECONFIG}" ]]; then
  printf 'refusing to reuse existing compatibility state or kubeconfig\n' >&2
  exit 2
fi
# Noclobber uses exclusive creation; a concurrent file/symlink appearing
# after the checks above cannot be followed or overwritten. Track each
# successful creation independently in case the second one fails.
set -o noclobber
if : > "${KIAC_E2E_STATE_FILE}"; then STATE_OWNED=true; else exit 2; fi
if : > "${KUBECONFIG}"; then KUBECONFIG_OWNED=true; else exit 2; fi
set +o noclobber
if [[ "${KIAC_E2E_SKIP_BUILD:-false}" != true ]]; then make -C "${ROOT}" build; fi
[[ -x "${KIAC_BIN}" ]]
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=false -trimpath \
  -o "${WORK}/traffic" "${ROOT}/test/e2e/traffic"
go run "${ROOT}/test/e2e/compat-image" --binary "${WORK}/traffic" --name "${IMAGE}" --output "${WORK}/image.tar"
go run "${ROOT}/test/e2e/runtime-preflight" --expected-container-version "${EXPECTED_VERSION}" --assert-image-absent "${IMAGE}"
IMAGE_LOADED=true
container image load --input "${WORK}/image.tar"

check_traffic() {
  local ip
  ip=$(k -n kiac-e2e get service traffic -o jsonpath='{.spec.clusterIP}')
  [[ -n "${ip}" && "${ip}" != None ]]
  # The probe runs inside the CP pod and reaches the worker pod. The DNS
  # URL proves CoreDNS; the numeric URL separately proves ClusterIP routing.
  k -n kiac-e2e exec traffic-control -- /traffic upload \
    --url http://traffic.kiac-e2e.svc.cluster.local:8080/upload --bytes 1048576 || return 1
  k -n kiac-e2e exec traffic-control -- /traffic upload \
    --url "http://${ip}:8080/upload" --bytes 1048576
}

boot_id() {
  local id
  id=$(container exec "$1" cat /proc/sys/kernel/random/boot_id | tr -d '\r\n')
  [[ "${id}" =~ ^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$ ]] || return 1
  printf '%s' "${id}"
}

node_reports_boot_id() {
  [[ "$(k get node "$1" -o jsonpath='{.status.nodeInfo.bootID}')" == "$2" ]]
}

run_distro() {
  local distro=$1 cp worker before_cp before_worker after_cp after_worker
  local candidate="${KIAC_E2E_PREFIX}-${distro}"
  # Check both Kiac inventory (including other backends) and orphaned Apple
  # node names before acquiring cleanup ownership. A rejected create must
  # never lead the EXIT trap to delete a preexisting cluster.
  go run "${ROOT}/test/e2e/runtime-preflight" --expected-container-version "${EXPECTED_VERSION}" \
    --kiac-bin "${KIAC_BIN}" --assert-cluster-absent "${candidate}"
  CURRENT_CLUSTER="${candidate}"
  CURRENT_CONTEXT="kiac-${CURRENT_CLUSTER}"
  cp="kiac-${CURRENT_CLUSTER}-control-plane"
  worker="kiac-${CURRENT_CLUSTER}-worker-1"
  printf '%s\n' "${CURRENT_CLUSTER}" > "${KIAC_E2E_STATE_FILE}"
  printf '\n=== container %s: %s compatibility ===\n' "${EXPECTED_VERSION}" "${distro}"
  "${KIAC_BIN}" create cluster --name "${CURRENT_CLUSTER}" --distro "${distro}" \
    --workers 1 --cpus 2 --memory 2G --cp-memory 4G --ip-family ipv4 \
    --no-lb --no-storage --no-metrics --wait 8m
  k wait --for=condition=Ready nodes --all --timeout=300s
  [[ "$(k get nodes --no-headers | awk 'NF {n++} END {print n+0}')" == 2 ]]
  "${KIAC_BIN}" load image "${IMAGE}" --name "${CURRENT_CLUSTER}"
  k create namespace kiac-e2e
  local node role
  for role in control worker; do
    if [[ "${role}" == control ]]; then node=${cp}; else node=${worker}; fi
    k apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: traffic-${role}
  namespace: kiac-e2e
  labels:
    app: traffic-${role}
spec:
  nodeSelector:
    kubernetes.io/hostname: ${node}
  tolerations:
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
      effect: NoSchedule
  automountServiceAccountToken: false
  containers:
    - name: traffic
      image: ${IMAGE}
      imagePullPolicy: Never
      args: [server, --listen, ':8080']
      ports:
        - containerPort: 8080
      readinessProbe:
        httpGet:
          path: /healthz
          port: 8080
      resources:
        requests: {cpu: 5m, memory: 16Mi}
        limits: {memory: 64Mi}
      securityContext:
        runAsUser: 65532
        runAsNonRoot: true
        readOnlyRootFilesystem: true
        allowPrivilegeEscalation: false
        capabilities: {drop: [ALL]}
EOF
  done
  k apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: traffic
  namespace: kiac-e2e
spec:
  selector: {app: traffic-worker}
  ports:
    - port: 8080
      targetPort: 8080
EOF
  k -n kiac-e2e wait --for=condition=Ready pod/traffic-control pod/traffic-worker --timeout=180s
  retry check_traffic
  before_cp=$(boot_id "${cp}")
  before_worker=$(boot_id "${worker}")
  "${KIAC_BIN}" stop node worker-1 --name "${CURRENT_CLUSTER}"
  "${KIAC_BIN}" stop node control-plane --name "${CURRENT_CLUSTER}"
  go run "${ROOT}/test/e2e/runtime-preflight" --expected-container-version "${EXPECTED_VERSION}" \
    --assert-stopped-nodes "${cp},${worker}"
  "${KIAC_BIN}" resume cluster --name "${CURRENT_CLUSTER}" --wait 8m
  after_cp=$(boot_id "${cp}")
  after_worker=$(boot_id "${worker}")
  [[ "${before_cp}" != "${after_cp}" && "${before_worker}" != "${after_worker}" ]]
  retry node_reports_boot_id "${cp}" "${after_cp}"
  retry node_reports_boot_id "${worker}" "${after_worker}"
  k wait --for=condition=Ready nodes --all --timeout=300s
  k -n kiac-e2e wait --for=condition=Ready pod/traffic-control pod/traffic-worker --timeout=180s
  retry check_traffic
  printf 'PASS %s: exact local image on 2 nodes, DNS/ClusterIP before+after full VM reboot, both boot IDs changed\n' "${distro}"
  delete_current
}

if [[ "${DISTRO}" == both || "${DISTRO}" == kubeadm ]]; then run_distro kubeadm; fi
if [[ "${DISTRO}" == both || "${DISTRO}" == k3s ]]; then run_distro k3s; fi
printf '\nContainer %s CPU compatibility passed (%s).\n' "${EXPECTED_VERSION}" "${DISTRO}"

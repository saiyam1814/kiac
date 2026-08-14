#!/usr/bin/env bash
# Run Rancher Manager on a dedicated kiac cluster and verify its UI and API.
set -euo pipefail

RANCHER_VERSION="2.14.3"
RANCHER_CHART_VERSION="2.14.3"
KUBERNETES_VERSION="1.34"

CLUSTER_NAME="${KIAC_RANCHER_CLUSTER:-rancher-lab}"
CONTEXT="kiac-${CLUSTER_NAME}"
NAMESPACE="cattle-system"
RELEASE="rancher"
STATE_DIR="${KIAC_RANCHER_STATE_DIR:-${HOME}/.kiac/labs/rancher/${CLUSTER_NAME}}"
PASSWORD_FILE="${STATE_DIR}/bootstrap-password"
HOSTNAME_FILE="${STATE_DIR}/hostname"
TLS_CERT_FILE="${STATE_DIR}/tls.crt"
TLS_KEY_FILE="${STATE_DIR}/tls.key"
VALUES_FILE="${STATE_DIR}/values.yaml"

info() { printf '    %s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<EOF
Usage: $0 [up|verify|cleanup]

  up        Create a dedicated Kubernetes 1.34 cluster and install Rancher
  verify    Check Gateway routing, the Rancher UI, login, version, and local cluster
  cleanup   Delete only the Rancher cluster and state created by this script

Environment:
  KIAC_RANCHER_CLUSTER=<name>             cluster name (default: rancher-lab)
  KIAC_RANCHER_STATE_DIR=<path>           ownership and credential state
  KIAC_RANCHER_HOSTNAME=<dns-name>        override the generated sslip.io hostname
  RANCHER_BOOTSTRAP_PASSWORD_FILE=<path>  seed the admin password from a file
  KIAC_RANCHER_CPUS=<count>               control-plane CPUs (default: 4)
  KIAC_RANCHER_CP_MEMORY=<size>            control-plane memory (default: 6G)

State: ${STATE_DIR}

Rancher is intentionally installed on a dedicated cluster. It creates cluster-wide
resources, so this script never adopts an unrelated existing cluster.
EOF
}

require_tools() {
  local tool
  for tool in kiac container kubectl helm curl jq openssl; do
    command -v "${tool}" >/dev/null 2>&1 || fail "missing tool: ${tool}"
  done
}

check_helm_version() {
  local version=""
  local major=""
  local minor=""

  version="$(helm version --template '{{.Version}}' 2>/dev/null)"
  version="${version#v}"
  version="${version%%+*}"
  IFS=. read -r major minor _ <<<"${version}"
  [[ "${major}" =~ ^[0-9]+$ && "${minor}" =~ ^[0-9]+$ ]] \
    || fail "could not parse Helm version: ${version}"
  if (( major < 3 || (major == 3 && minor < 18) )); then
    fail "Rancher ${RANCHER_VERSION} requires Helm 3.18 or newer; found ${version}"
  fi
}

prepare_state() {
  umask 077
  mkdir -p "${STATE_DIR}"
  chmod 0700 "${STATE_DIR}"
}

container_exists() {
  container inspect "$1" >/dev/null 2>&1
}

context_exists() {
  [[ "$(kubectl config get-contexts "$1" -o name 2>/dev/null || true)" == "$1" ]]
}

context_ready() {
  kubectl --context "$1" get --raw=/readyz >/dev/null 2>&1
}

ensure_cluster() {
  local node="kiac-${CLUSTER_NAME}-control-plane"
  local marker="${STATE_DIR}/cluster.created"

  if context_ready "${CONTEXT}"; then
    [[ -f "${marker}" ]] \
      || fail "context ${CONTEXT} is live but was not created by this script; choose another name"
    info "using ready cluster ${CLUSTER_NAME}"
    return
  fi

  if container_exists "${node}"; then
    [[ -f "${marker}" ]] \
      || fail "cluster ${CLUSTER_NAME} exists and was not created by this script"
    step "Resuming Rancher cluster ${CLUSTER_NAME}"
    kiac resume cluster --name "${CLUSTER_NAME}"
    context_ready "${CONTEXT}" || fail "context ${CONTEXT} is not ready after resume"
    return
  fi

  if context_exists "${CONTEXT}"; then
    fail "stale context ${CONTEXT} exists without a running cluster; remove it or choose another name"
  fi

  step "Creating dedicated Kubernetes ${KUBERNETES_VERSION} cluster with Gateway API"
  touch "${marker}"
  kiac create cluster \
    --name "${CLUSTER_NAME}" \
    --workers 0 \
    --k8s-version "${KUBERNETES_VERSION}" \
    --gateway \
    --cpus "${KIAC_RANCHER_CPUS:-4}" \
    --cp-memory "${KIAC_RANCHER_CP_MEMORY:-6G}" \
    --no-metrics \
    --wait 10m
}

ensure_gateway() {
  kubectl --context "${CONTEXT}" get gatewayclass traefik >/dev/null 2>&1 \
    || fail "GatewayClass traefik is absent; the cluster must be created with --gateway"
  kubectl --context "${CONTEXT}" wait --for=condition=Accepted gatewayclass/traefik --timeout=180s
  kubectl --context "${CONTEXT}" -n kiac-gateway rollout status deployment/traefik --timeout=240s
}

gateway_ip() {
  local ip=""
  local deadline=$((SECONDS + 180))

  while (( SECONDS < deadline )); do
    ip="$(kubectl --context "${CONTEXT}" -n kiac-gateway get service traefik \
      -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${ip}" ]]; then
      printf '%s\n' "${ip}"
      return
    fi
    sleep 2
  done
  fail "kiac Gateway Service did not receive a LoadBalancer IP"
}

ensure_namespace() {
  if kubectl --context "${CONTEXT}" get namespace "${NAMESPACE}" >/dev/null 2>&1; then
    return
  fi
  kubectl --context "${CONTEXT}" create namespace "${NAMESPACE}" >/dev/null
}

ensure_password() {
  local source="${RANCHER_BOOTSTRAP_PASSWORD_FILE:-}"
  local password=""

  if [[ ! -f "${PASSWORD_FILE}" ]]; then
    if [[ -n "${source}" ]]; then
      [[ -f "${source}" ]] || fail "RANCHER_BOOTSTRAP_PASSWORD_FILE does not exist: ${source}"
      install -m 0600 "${source}" "${PASSWORD_FILE}"
    else
      printf 'Kiac-%s\n' "$(openssl rand -hex 16)" > "${PASSWORD_FILE}"
      chmod 0600 "${PASSWORD_FILE}"
    fi
  fi

  password="$(tr -d '\r\n' < "${PASSWORD_FILE}")"
  [[ ${#password} -ge 12 ]] || fail "Rancher bootstrap password must contain at least 12 characters"
  printf '%s\n' "${password}" > "${PASSWORD_FILE}"
  chmod 0600 "${PASSWORD_FILE}"
}

validate_hostname() {
  local hostname="$1"
  [[ ${#hostname} -le 253 && "${hostname}" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] \
    || fail "invalid Rancher hostname: ${hostname}"
}

desired_hostname() {
  local ip="$1"
  local hostname="${KIAC_RANCHER_HOSTNAME:-}"

  if [[ -z "${hostname}" ]]; then
    [[ "${ip}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] \
      || fail "cannot derive an sslip.io hostname from non-IPv4 address ${ip}; set KIAC_RANCHER_HOSTNAME"
    hostname="rancher.${ip}.sslip.io"
  fi
  validate_hostname "${hostname}"
  printf '%s\n' "${hostname}"
}

ensure_tls() {
  local hostname="$1"
  local current_hostname=""
  local config_file="${STATE_DIR}/openssl.cnf"

  [[ -f "${HOSTNAME_FILE}" ]] && current_hostname="$(tr -d '\r\n' < "${HOSTNAME_FILE}")"
  if [[ "${current_hostname}" == "${hostname}" \
      && -s "${TLS_CERT_FILE}" \
      && -s "${TLS_KEY_FILE}" ]] \
      && openssl x509 -in "${TLS_CERT_FILE}" -noout -checkend 86400 >/dev/null 2>&1; then
    return
  fi

  cat > "${config_file}" <<EOF
[req]
distinguished_name = subject
x509_extensions = server_cert
prompt = no

[subject]
commonName = ${hostname}

[server_cert]
subjectAltName = DNS:${hostname}
basicConstraints = critical, CA:false
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
EOF

  step "Generating a local TLS certificate for ${hostname}"
  openssl req -x509 -nodes -newkey rsa:2048 -sha256 -days 365 \
    -keyout "${TLS_KEY_FILE}" \
    -out "${TLS_CERT_FILE}" \
    -config "${config_file}" \
    -extensions server_cert >/dev/null 2>&1
  rm -f "${config_file}"
  printf '%s\n' "${hostname}" > "${HOSTNAME_FILE}"
  chmod 0600 "${TLS_CERT_FILE}" "${TLS_KEY_FILE}" "${HOSTNAME_FILE}"
}

write_values() {
  local hostname="$1"
  local password=""
  local quoted_hostname=""
  local quoted_password=""

  password="$(tr -d '\r\n' < "${PASSWORD_FILE}")"
  quoted_hostname="$(jq -Rn --arg value "${hostname}" '$value')"
  quoted_password="$(jq -Rn --arg value "${password}" '$value')"

  cat > "${VALUES_FILE}" <<EOF
hostname: ${quoted_hostname}
replicas: 1
bootstrapPassword: ${quoted_password}

networkExposure:
  type: gateway

gateway:
  gatewayClass:
    name: traefik
    ports:
      http: 80
      https: 443
    tls:
      source: secret
      secretName: tls-rancher-ingress

ingress:
  tls:
    source: secret

resources:
  requests:
    cpu: 250m
    memory: 512Mi
EOF
  chmod 0600 "${VALUES_FILE}"
}

ensure_release() {
  local marker="${STATE_DIR}/release.created"

  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" create secret tls tls-rancher-ingress \
    --cert="${TLS_CERT_FILE}" \
    --key="${TLS_KEY_FILE}" \
    --dry-run=client -o yaml \
    | kubectl --context "${CONTEXT}" apply -f - >/dev/null

  if helm status "${RELEASE}" --kube-context "${CONTEXT}" --namespace "${NAMESPACE}" >/dev/null 2>&1; then
    [[ -f "${marker}" ]] \
      || fail "Helm release ${NAMESPACE}/${RELEASE} exists and was not created by this script"
  else
    touch "${marker}"
  fi

  step "Installing Rancher Manager ${RANCHER_VERSION}"
  helm repo add rancher-stable https://releases.rancher.com/server-charts/stable --force-update >/dev/null
  helm upgrade --install "${RELEASE}" rancher-stable/rancher \
    --kube-context "${CONTEXT}" \
    --namespace "${NAMESPACE}" \
    --version "${RANCHER_CHART_VERSION}" \
    --values "${VALUES_FILE}" \
    --history-max 2 \
    --wait \
    --timeout 12m
}

route_ready() {
  local route="$1"
  local status=""

  status="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get httproute "${route}" -o json 2>/dev/null || true)"
  jq -e '
    ([.status.parents[]?.conditions[]? | select(.type == "Accepted" and .status == "True")] | length) > 0
    and
    ([.status.parents[]?.conditions[]? | select(.type == "ResolvedRefs" and .status == "True")] | length) > 0
  ' >/dev/null 2>&1 <<<"${status}"
}

wait_for_route() {
  local route="$1"
  local deadline=$((SECONDS + 180))

  while (( SECONDS < deadline )); do
    route_ready "${route}" && return
    sleep 2
  done
  fail "HTTPRoute ${NAMESPACE}/${route} was not accepted with resolved references"
}

rancher_curl() {
  local ip="$1"
  local hostname="$2"
  shift 2
  curl --insecure --fail --silent --show-error --max-time 15 \
    --resolve "${hostname}:443:${ip}" "$@"
}

rancher_token() {
  local ip="$1"
  local hostname="$2"
  local password=""
  local payload=""
  local response=""
  local token=""

  password="$(tr -d '\r\n' < "${PASSWORD_FILE}")"
  payload="$(jq -n --arg username admin --arg password "${password}" \
    '{username: $username, password: $password}')"

  for _ in $(seq 1 120); do
    response="$(printf '%s' "${payload}" \
      | rancher_curl "${ip}" "${hostname}" \
        -H 'Content-Type: application/json' \
        --data-binary @- \
        "https://${hostname}/v3-public/localProviders/local?action=login" 2>/dev/null || true)"
    token="$(jq -r '.token // empty' <<<"${response}" 2>/dev/null || true)"
    if [[ -n "${token}" ]]; then
      printf '%s\n' "${token}"
      return
    fi
    sleep 5
  done
  fail "Rancher login API did not return a token"
}

verify_rancher() {
  local ip=""
  local hostname=""
  local ping=""
  local ui=""
  local token=""
  local version_response=""
  local version=""
  local clusters=""
  local cluster_state=""
  local nodes=""
  local node_count="0"

  context_ready "${CONTEXT}" || fail "context ${CONTEXT} is not ready; run: $0 up"
  [[ -f "${PASSWORD_FILE}" && -f "${HOSTNAME_FILE}" ]] \
    || fail "Rancher credential or hostname state is absent; run: $0 up"

  hostname="$(tr -d '\r\n' < "${HOSTNAME_FILE}")"
  ip="$(gateway_ip)"

  step "Checking the Rancher workload and Gateway API routes"
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status deployment/rancher --timeout=600s
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" wait \
    --for=condition=Programmed gateway/rancher-gateway --timeout=180s
  wait_for_route rancher
  wait_for_route rancher-http-redirect

  step "Checking the Rancher UI and authenticated API"
  for _ in $(seq 1 120); do
    ping="$(rancher_curl "${ip}" "${hostname}" "https://${hostname}/ping" 2>/dev/null || true)"
    [[ "${ping}" == "pong" ]] && break
    sleep 5
  done
  [[ "${ping}" == "pong" ]] || fail "Rancher /ping did not return pong"

  for _ in $(seq 1 60); do
    ui="$(rancher_curl "${ip}" "${hostname}" --location "https://${hostname}/dashboard/" 2>/dev/null || true)"
    [[ "${ui}" == *"<title>Rancher</title>"* || "${ui}" == *"Rancher Dashboard"* ]] && break
    sleep 2
  done
  [[ "${ui}" == *"<title>Rancher</title>"* || "${ui}" == *"Rancher Dashboard"* ]] \
    || fail "Rancher dashboard HTML was not returned"

  token="$(rancher_token "${ip}" "${hostname}")"
  version_response="$(rancher_curl "${ip}" "${hostname}" \
    -H "Authorization: Bearer ${token}" \
    "https://${hostname}/v3/settings/server-version")"
  version="$(jq -r '.value // empty' <<<"${version_response}")"
  [[ "${version}" == "v${RANCHER_VERSION}" || "${version}" == "${RANCHER_VERSION}" ]] \
    || fail "Rancher API reported version ${version:-unknown}, expected ${RANCHER_VERSION}"

  for _ in $(seq 1 120); do
    clusters="$(rancher_curl "${ip}" "${hostname}" \
      -H "Authorization: Bearer ${token}" \
      "https://${hostname}/v3/clusters?name=local" 2>/dev/null || true)"
    cluster_state="$(jq -r '.data[]? | select(.id == "local") | .state // empty' \
      <<<"${clusters}" 2>/dev/null || true)"
    [[ "${cluster_state}" == "active" ]] && break
    sleep 5
  done
  [[ "${cluster_state}" == "active" ]] \
    || fail "Rancher did not report its local cluster active"

  nodes="$(rancher_curl "${ip}" "${hostname}" \
    -H "Authorization: Bearer ${token}" \
    "https://${hostname}/v3/nodes?clusterId=local")"
  node_count="$(jq 'if .data then (.data | length) else 0 end' <<<"${nodes}")"
  (( node_count >= 1 )) || fail "Rancher API did not return a node for the local cluster"

  info "Rancher API version: ${version}"
  info "local cluster state: ${cluster_state}"
  info "Kubernetes nodes through Rancher: ${node_count}"
  info "Gateway IP: ${ip}"
  info "UI: https://${hostname}"
  info "username: admin"
  info "password file: ${PASSWORD_FILE}"
}

up() {
  local ip=""
  local hostname=""

  require_tools
  check_helm_version
  kiac doctor >/dev/null 2>&1 || fail "kiac preflight failed; run: kiac doctor"
  prepare_state
  ensure_cluster
  ensure_gateway
  ip="$(gateway_ip)"
  hostname="$(desired_hostname "${ip}")"
  ensure_namespace
  ensure_password
  ensure_tls "${hostname}"
  write_values "${hostname}"
  ensure_release
  verify_rancher
}

cleanup() {
  command -v kiac >/dev/null 2>&1 || fail "missing tool: kiac"
  command -v container >/dev/null 2>&1 || fail "missing tool: container"

  if [[ ! -f "${STATE_DIR}/cluster.created" ]]; then
    info "leaving cluster ${CLUSTER_NAME} alone (no ownership marker)"
    return
  fi

  step "Deleting Rancher cluster ${CLUSTER_NAME}"
  if container_exists "kiac-${CLUSTER_NAME}-control-plane" || context_exists "${CONTEXT}"; then
    kiac delete cluster --name "${CLUSTER_NAME}"
  else
    info "cluster is already absent"
  fi
  rm -rf "${STATE_DIR}"
}

case "${1:-up}" in
  up) up ;;
  verify) require_tools; verify_rancher ;;
  cleanup) cleanup ;;
  -h|--help|help) usage ;;
  *) usage >&2; exit 1 ;;
esac

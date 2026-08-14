#!/usr/bin/env bash
# Run Portainer CE on a small kiac cluster and verify the real UI/API path.
set -euo pipefail

PORTAINER_VERSION="2.39.5"
PORTAINER_CHART_VERSION="239.5.0"
KUBERNETES_VERSION="1.34"

CLUSTER_NAME="${KIAC_PORTAINER_CLUSTER:-portainer-lab}"
CONTEXT="kiac-${CLUSTER_NAME}"
NAMESPACE="portainer"
RELEASE="portainer"
STATE_DIR="${KIAC_PORTAINER_STATE_DIR:-${HOME}/.kiac/labs/portainer/${CLUSTER_NAME}}"
PASSWORD_FILE="${STATE_DIR}/admin-password"
VALUES_FILE="${STATE_DIR}/values.yaml"
USE_EXISTING="${KIAC_PORTAINER_USE_EXISTING:-false}"

info() { printf '    %s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<EOF
Usage: $0 [up|verify|cleanup]

  up        Create a small Kubernetes 1.34 cluster and install Portainer CE
  verify    Check the Deployment, PVC, LoadBalancer, status API, and login API
  cleanup   Delete only Portainer resources and clusters created by this script

Environment:
  KIAC_PORTAINER_CLUSTER=<name>         cluster name (default: portainer-lab)
  KIAC_PORTAINER_USE_EXISTING=true      use an already-ready kiac context
  KIAC_PORTAINER_STATE_DIR=<path>       ownership and credential state
  PORTAINER_ADMIN_PASSWORD_FILE=<path>  seed the admin password from a file

State: ${STATE_DIR}
EOF
}

require_tools() {
  local tool
  for tool in kiac container kubectl helm curl jq openssl; do
    command -v "${tool}" >/dev/null 2>&1 || fail "missing tool: ${tool}"
  done
}

prepare_state() {
  umask 077
  mkdir -p "${STATE_DIR}"
  chmod 0700 "${STATE_DIR}"
}

use_existing() {
  case "${USE_EXISTING}" in
    1|true|TRUE|yes|YES) return 0 ;;
    *) return 1 ;;
  esac
}

container_exists() {
  container inspect "$1" >/dev/null 2>&1
}

context_exists() {
  kubectl config get-contexts -o name 2>/dev/null | grep -Fqx "$1"
}

context_ready() {
  kubectl --context "$1" get --raw=/readyz >/dev/null 2>&1
}

ensure_cluster() {
  local node="kiac-${CLUSTER_NAME}-control-plane"
  local marker="${STATE_DIR}/cluster.created"

  if context_ready "${CONTEXT}"; then
    if [[ -f "${marker}" ]] || use_existing; then
      info "using ready cluster ${CLUSTER_NAME}"
      return
    fi
    fail "context ${CONTEXT} is live but was not created by this script; set KIAC_PORTAINER_USE_EXISTING=true to use it"
  fi

  use_existing && fail "KIAC_PORTAINER_USE_EXISTING=true, but context ${CONTEXT} is not ready"

  if container_exists "${node}"; then
    [[ -f "${marker}" ]] || fail "cluster ${CLUSTER_NAME} exists and was not created by this script"
    step "Resuming Portainer cluster ${CLUSTER_NAME}"
    kiac resume cluster --name "${CLUSTER_NAME}"
    context_ready "${CONTEXT}" || fail "context ${CONTEXT} is not ready after resume"
    return
  fi

  if context_exists "${CONTEXT}"; then
    fail "stale context ${CONTEXT} exists without a running cluster; remove it or choose another name"
  fi

  step "Creating single-node Kubernetes ${KUBERNETES_VERSION} cluster"
  touch "${marker}"
  kiac create cluster \
    --name "${CLUSTER_NAME}" \
    --workers 0 \
    --k8s-version "${KUBERNETES_VERSION}" \
    --cpus "${KIAC_PORTAINER_CPUS:-4}" \
    --cp-memory "${KIAC_PORTAINER_CP_MEMORY:-4G}" \
    --no-metrics \
    --wait 8m
}

ensure_namespace() {
  local marker="${STATE_DIR}/namespace.created"

  if kubectl --context "${CONTEXT}" get namespace "${NAMESPACE}" >/dev/null 2>&1; then
    [[ -f "${marker}" ]] || fail "namespace ${NAMESPACE} already exists and was not created by this script"
    return
  fi

  touch "${marker}"
  kubectl --context "${CONTEXT}" create namespace "${NAMESPACE}" >/dev/null
}

ensure_password() {
  local source="${PORTAINER_ADMIN_PASSWORD_FILE:-}"
  local password=""

  if [[ ! -f "${PASSWORD_FILE}" ]]; then
    if [[ -n "${source}" ]]; then
      [[ -f "${source}" ]] || fail "PORTAINER_ADMIN_PASSWORD_FILE does not exist: ${source}"
      install -m 0600 "${source}" "${PASSWORD_FILE}"
    else
      printf 'Kiac-%s\n' "$(openssl rand -hex 16)" > "${PASSWORD_FILE}"
      chmod 0600 "${PASSWORD_FILE}"
    fi
  fi

  password="$(tr -d '\r\n' < "${PASSWORD_FILE}")"
  [[ ${#password} -ge 12 ]] || fail "Portainer admin password must contain at least 12 characters"
  printf '%s\n' "${password}" > "${PASSWORD_FILE}"
  chmod 0600 "${PASSWORD_FILE}"
}

write_values() {
  cat > "${VALUES_FILE}" <<EOF
replicaCount: 1

image:
  repository: portainer/portainer-ce
  tag: "${PORTAINER_VERSION}"
  pullPolicy: IfNotPresent

service:
  type: LoadBalancer

tls:
  force: true

adminPassword:
  existingSecret: portainer-admin-password

persistence:
  enabled: true
  size: 5Gi

resources:
  requests:
    cpu: 50m
    memory: 128Mi
EOF
  chmod 0600 "${VALUES_FILE}"
}

ensure_release() {
  local marker="${STATE_DIR}/release.created"

  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" create secret generic portainer-admin-password \
    --from-file="password=${PASSWORD_FILE}" \
    --dry-run=client -o yaml \
    | kubectl --context "${CONTEXT}" apply -f - >/dev/null

  if helm status "${RELEASE}" --kube-context "${CONTEXT}" --namespace "${NAMESPACE}" >/dev/null 2>&1; then
    [[ -f "${marker}" ]] || fail "Helm release ${NAMESPACE}/${RELEASE} exists and was not created by this script"
  else
    touch "${marker}"
  fi

  step "Installing Portainer CE ${PORTAINER_VERSION}"
  helm repo add portainer https://portainer.github.io/k8s/ --force-update >/dev/null
  helm upgrade --install "${RELEASE}" portainer/portainer \
    --kube-context "${CONTEXT}" \
    --namespace "${NAMESPACE}" \
    --version "${PORTAINER_CHART_VERSION}" \
    --values "${VALUES_FILE}" \
    --wait \
    --timeout 8m
}

load_balancer_ip() {
  local ip=""
  local deadline=$((SECONDS + 180))

  while (( SECONDS < deadline )); do
    ip="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get service "${RELEASE}" \
      -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${ip}" ]]; then
      printf '%s\n' "${ip}"
      return
    fi
    sleep 2
  done
  fail "Portainer Service did not receive a LoadBalancer IP"
}

portainer_jwt() {
  local ip="$1"
  local password=""
  local auth=""
  local jwt=""

  password="$(tr -d '\r\n' < "${PASSWORD_FILE}")"
  for _ in $(seq 1 60); do
    auth="$(jq -n --arg username admin --arg password "${password}" \
      '{Username: $username, Password: $password}' \
      | curl --insecure --fail --silent --show-error --max-time 10 \
        -H 'Content-Type: application/json' --data-binary @- \
        "https://${ip}:9443/api/auth" 2>/dev/null || true)"
    jwt="$(jq -r '.jwt // empty' <<<"${auth}" 2>/dev/null || true)"
    if [[ -n "${jwt}" ]]; then
      printf '%s\n' "${jwt}"
      return
    fi
    sleep 2
  done
  fail "Portainer login API did not return a token"
}

ensure_environment() {
  local ip=""
  local jwt=""
  local endpoints=""
  local existing_type=""
  local created=""

  ip="$(load_balancer_ip)"
  jwt="$(portainer_jwt "${ip}")"
  endpoints="$(curl --insecure --fail --silent --show-error --max-time 10 \
    -H "Authorization: Bearer ${jwt}" "https://${ip}:9443/api/endpoints")"
  existing_type="$(jq -r --arg name "${CONTEXT}" \
    'map(select(.Name == $name)) | first | .Type // empty' <<<"${endpoints}")"

  if [[ -n "${existing_type}" ]]; then
    [[ "${existing_type}" == "5" ]] \
      || fail "Portainer environment ${CONTEXT} exists with unexpected type ${existing_type}"
    info "using Portainer environment ${CONTEXT}"
    return
  fi

  step "Registering the local Kubernetes environment"
  created="$(curl --insecure --fail --silent --show-error --max-time 30 \
    -H "Authorization: Bearer ${jwt}" \
    --form-string "Name=${CONTEXT}" \
    --form-string 'EndpointCreationType=5' \
    "https://${ip}:9443/api/endpoints")"
  jq -e --arg name "${CONTEXT}" '.Name == $name and .Type == 5' >/dev/null <<<"${created}" \
    || fail "Portainer did not register the local Kubernetes environment"
}

verify_lab() {
  local ip=""
  local status=""
  local jwt=""
  local endpoints=""
  local endpoint_id=""
  local endpoint_count="0"
  local nodes=""
  local node_count="0"

  context_ready "${CONTEXT}" || fail "context ${CONTEXT} is not ready; run: $0 up"
  [[ -f "${PASSWORD_FILE}" ]] || fail "admin password is absent; run: $0 up"

  step "Checking the Portainer workload and storage"
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status deployment/portainer --timeout=240s
  [[ "$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pvc portainer -o jsonpath='{.status.phase}')" == "Bound" ]] \
    || fail "Portainer PVC is not Bound"

  ip="$(load_balancer_ip)"
  info "LoadBalancer IP: ${ip}"

  step "Checking the HTTPS status and authentication APIs"
  for _ in $(seq 1 60); do
    status="$(curl --insecure --fail --silent --show-error --max-time 5 \
      "https://${ip}:9443/api/system/status" 2>/dev/null || true)"
    if jq -e --arg version "${PORTAINER_VERSION}" '.Version == $version' \
      >/dev/null 2>&1 <<<"${status}"; then
      break
    fi
    sleep 2
  done
  jq -e --arg version "${PORTAINER_VERSION}" '.Version == $version' \
    >/dev/null 2>&1 <<<"${status}" \
    || fail "Portainer status API did not report version ${PORTAINER_VERSION}"

  jwt="$(portainer_jwt "${ip}")"

  for _ in $(seq 1 60); do
    endpoints="$(curl --insecure --fail --silent --show-error --max-time 10 \
      -H "Authorization: Bearer ${jwt}" "https://${ip}:9443/api/endpoints" 2>/dev/null || true)"
    endpoint_count="$(jq 'if type == "array" then length else 0 end' <<<"${endpoints}" 2>/dev/null || printf '0')"
    endpoint_id="$(jq -r --arg name "${CONTEXT}" \
      'map(select(.Name == $name and .Type == 5 and .Status == 1)) | first | .Id // empty' \
      <<<"${endpoints}" 2>/dev/null || true)"
    [[ -n "${endpoint_id}" ]] && break
    sleep 2
  done
  [[ -n "${endpoint_id}" ]] || fail "Portainer reports no healthy ${CONTEXT} Kubernetes environment"

  nodes="$(curl --insecure --fail --silent --show-error --max-time 10 \
    -H "Authorization: Bearer ${jwt}" \
    "https://${ip}:9443/api/endpoints/${endpoint_id}/kubernetes/api/v1/nodes")"
  node_count="$(jq 'if .kind == "NodeList" then (.items | length) else 0 end' <<<"${nodes}")"
  (( node_count >= 1 )) || fail "Portainer could not read Kubernetes nodes through its API proxy"

  info "Portainer API version: $(jq -r '.Version // .version // "unknown"' <<<"${status}")"
  info "managed environments: ${endpoint_count}"
  info "Kubernetes nodes through Portainer: ${node_count}"
  info "UI: https://${ip}:9443"
  info "username: admin"
  info "password file: ${PASSWORD_FILE}"
}

up() {
  require_tools
  kiac doctor >/dev/null 2>&1 || fail "kiac preflight failed; run: kiac doctor"
  prepare_state
  ensure_cluster
  ensure_namespace
  ensure_password
  write_values
  ensure_release
  ensure_environment
  verify_lab
}

cleanup() {
  require_tools

  if [[ -f "${STATE_DIR}/cluster.created" ]]; then
    step "Deleting Portainer cluster ${CLUSTER_NAME}"
    kiac delete cluster --name "${CLUSTER_NAME}"
    rm -rf "${STATE_DIR}"
    return
  fi

  if [[ -f "${STATE_DIR}/release.created" ]]; then
    context_ready "${CONTEXT}" || fail "cannot reach ${CONTEXT}; preserving ownership state"
    step "Removing the owned Portainer Helm release"
    helm uninstall "${RELEASE}" --kube-context "${CONTEXT}" --namespace "${NAMESPACE}" --wait
  else
    info "leaving ${NAMESPACE}/${RELEASE} alone (no ownership marker)"
  fi

  if [[ -f "${STATE_DIR}/namespace.created" ]]; then
    kubectl --context "${CONTEXT}" delete namespace "${NAMESPACE}" --wait=true
  else
    info "leaving namespace ${NAMESPACE} alone (no ownership marker)"
  fi
  rm -rf "${STATE_DIR}"
}

case "${1:-up}" in
  up) up ;;
  verify) require_tools; verify_lab ;;
  cleanup) cleanup ;;
  -h|--help|help) usage ;;
  *) usage >&2; exit 1 ;;
esac

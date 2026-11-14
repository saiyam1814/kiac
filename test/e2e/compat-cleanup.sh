#!/usr/bin/env bash
# Cancellation recovery for the focused profile. Keep failed ownership
# records so a later cleanup cannot mistake leaked VMs for a clean run.
set -Euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
KIAC_BIN=${KIAC_BIN:-"${ROOT}/bin/kiac"}
STATE=${KIAC_E2E_STATE_FILE:-}
[[ -n "${STATE}" && -e "${STATE}" ]] || exit 0
[[ -f "${STATE}" && ! -L "${STATE}" ]] || { printf 'invalid compatibility ownership file\n' >&2; exit 1; }
RUN_ID=$(printf '%s' "${KIAC_E2E_RUN_ID:-}" | tr -cd 'a-zA-Z0-9-' | cut -c1-24 | tr '[:upper:]' '[:lower:]')
[[ -n "${RUN_ID}" ]] || { printf 'compatibility cleanup requires its original run ID\n' >&2; exit 1; }
PREFIX="ci-compat-${RUN_ID}"
NEXT=$(mktemp "${STATE}.pending.XXXXXX") || exit 1
trap 'rm -f -- "${NEXT}"' EXIT
failed=0
while IFS= read -r name || [[ -n "${name}" ]]; do
  [[ -n "${name}" ]] || continue
  case "${name}" in
    "${PREFIX}-kubeadm"|"${PREFIX}-k3s") ;;
    *)
      printf 'refusing unexpected compatibility ownership record\n' >&2
      printf '%s\n' "${name}" >> "${NEXT}"
      failed=1
      continue
      ;;
  esac
  clean=true
  if ! "${KIAC_BIN}" delete cluster --name "${name}"; then clean=false; fi
  names=""
  if ! names=$(container ls --all --quiet); then clean=false; fi
  while IFS= read -r node; do
    case "${node}" in "kiac-${name}-"*) clean=false ;; esac
  done <<< "${names}"
  if [[ "${clean}" != true ]]; then
    printf '%s\n' "${name}" >> "${NEXT}"
    failed=1
  fi
done < "${STATE}"
if [[ ${failed} -ne 0 ]]; then
  mv -- "${NEXT}" "${STATE}" || exit 1
  printf 'compatibility cleanup incomplete; ownership records preserved\n' >&2
  exit 1
fi
rm -f -- "${STATE}"
if [[ -n "${KIAC_E2E_KUBECONFIG:-}" ]]; then
  case "$(basename "${KIAC_E2E_KUBECONFIG}")" in
    kiac-e2e-*.kubeconfig) rm -f -- "${KIAC_E2E_KUBECONFIG}" ;;
  esac
fi

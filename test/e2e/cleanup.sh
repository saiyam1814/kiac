#!/usr/bin/env bash
set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
KIAC_BIN=${KIAC_BIN:-"${ROOT}/bin/kiac"}
STATE_FILE=${KIAC_E2E_STATE_FILE:-"${TMPDIR:-/tmp}/kiac-e2e-clusters"}
PREFIX=${KIAC_E2E_PREFIX:-ci-}
if [[ -n "${KIAC_E2E_KUBECONFIG:-}" ]]; then
  export KUBECONFIG=${KIAC_E2E_KUBECONFIG}
fi

if [[ -f "${STATE_FILE}" ]]; then
  while IFS= read -r name; do
    [[ -n "${name}" ]] || continue
    case "${name}" in
      "${PREFIX}"*) ;;
      *)
        printf 'refusing to clean unexpected cluster name %q\n' "${name}" >&2
        continue
        ;;
    esac
    if [[ -x "${KIAC_BIN}" ]]; then
      "${KIAC_BIN}" delete cluster --name "${name}" || true
    fi
  done < <(sort -u "${STATE_FILE}")
  rm -f "${STATE_FILE}"
fi

# Never remove a caller's normal kubeconfig. Runtime files have a fixed,
# recognizable basename whether they came from local execution or Actions.
if [[ -n "${KUBECONFIG:-}" ]]; then
  case "$(basename "${KUBECONFIG}")" in
    kiac-e2e-*.kubeconfig) rm -f "${KUBECONFIG}" ;;
  esac
fi

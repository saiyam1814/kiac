# Runtime smoke tests

The unit suite uses fake `container` clients. This directory covers the
contract that only a real Apple silicon Mac can prove: node VM boot,
cross-node networking, the edge proxy, addons, and restart recovery.

Run one distro locally:

```sh
./test/e2e/run.sh kubeadm
./test/e2e/run.sh k3s
```

`quick` runs both IPv4 distros. `dual` runs both dual-stack distros and
`full` runs all four profiles. Every IPv4 profile creates one control
plane and three workers, enables Gateway API and observability, sends an
exactly verified 1 MiB upload from worker 1 through worker 3 to a pod on
worker 2, rejects an unauthenticated tunnel, checks the proxy's limited
RBAC, and stops and starts worker 1. Test clusters and their isolated
kubeconfig are removed on success, failure, interrupt, or CI cancellation.
For local failure analysis, set `KIAC_E2E_KEEP_ON_FAILURE=true`; the
failure report prints the exact delete command for the retained cluster.

## GitHub runner

The workflow deliberately has no `pull_request` trigger. A public fork
must never execute contributor-controlled code on a self-hosted Mac.
Attach a dedicated Apple silicon runner with the labels `macOS`, `ARM64`,
and `kiac-runtime`, install `apple/container` and `kubectl`, and keep
repository secrets off that machine. Then set the repository Actions
variable `KIAC_RUNTIME_CI_ENABLED` to `true`.

Once enabled, code merges run `quick`, the Monday schedule runs `full`,
and maintainers can dispatch any profile manually. The job uploads no
artifacts, uses only standard public-repository Actions plus the
self-hosted machine, and therefore consumes no paid runner minutes.

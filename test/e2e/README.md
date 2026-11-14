# Runtime smoke tests

The unit suite uses fake `container` clients. This directory covers the
contract that only a real Apple silicon Mac can prove: node VM boot,
cross-node networking, the edge proxy, addons, and restart recovery.

Run one distro locally:

```sh
./test/e2e/run.sh kubeadm
./test/e2e/run.sh k3s
```

For the focused Apple/container compatibility profile, start the selected
runtime separately and require its exact version:

```sh
KIAC_EXPECTED_CONTAINER_VERSION=1.7.1 ./test/e2e/run.sh compat
KIAC_EXPECTED_CONTAINER_VERSION=1.6.1 ./test/e2e/run.sh compat
```

`compat` runs kubeadm and K3s sequentially, with one control plane and one
worker at a time. Before creating any runtime resources it requires an
Apple silicon Mac on macOS 26 or newer and checks that both the actual CLI
and running API server report the expected version through
`container system status --format json`. A version-labelled runner alone
is insufficient: a newer CLI connected to an older daemon fails closed.
The preflight prints only versions and platform information, excluding
runtime paths and resource inventories. It is bounded to 20 seconds and
64 KiB of output and never starts services or installs software.

The profile generates its own scratch OCI image from the static traffic
probe, without BuildKit or a base image. It imports the exact short tag
through `kiac load image`, starts a `Never`-pull pod on each node, verifies
cross-node DNS and ClusterIP uploads, stops both VMs and resumes the
cluster. It requires both guest boot IDs to change, waits for kubelets to
report those new IDs, and repeats real pod traffic before deletion. It
checks that neither owned node VM remains. For a focused rerun, set
`KIAC_COMPAT_DISTRO=kubeadm` or `k3s`. To use an already built CLI, set
`KIAC_BIN=/absolute/path/to/kiac KIAC_E2E_SKIP_BUILD=true`.

`quick` runs both IPv4 distros. `dual` runs both dual-stack distros,
`flannel` runs a kubeadm IPv4 cluster with `--cni flannel --kernel full`,
and `full` runs all five clusters. Every IPv4 profile creates one control
plane and three workers, enables Gateway API and observability, sends an
exactly verified 1 MiB upload from worker 1 through worker 3 to a pod on
worker 2, rejects an unauthenticated tunnel, checks the proxy's limited
RBAC, and stops and starts worker 1. Test clusters and their isolated
kubeconfig are removed on success, failure, interrupt, or CI cancellation.
For local failure analysis, set `KIAC_E2E_KEEP_ON_FAILURE=true`; the
failure report prints the exact delete command for the retained cluster.

The separate `gpu` profile runs monthly or on manual dispatch. It creates a
K3s 1.36 cluster with one ordinary and two real Apple GPU workers using DRA,
then a kubeadm 1.37 cluster with no ordinary workers using the device-plugin
fallback, observability, and `--no-lb`. It proves two Venus VMs can execute
concurrently, explicit DRA memory allocation, the opt-in `nvidia.com/gpu`
admission rewrite, K3s NetworkPolicy enforcement, a 32 MiB cross-node upload,
LoadBalancer and Gateway traffic, PVCs, both external-IP and ClusterIP Grafana
access, TinyLlama inference, node restart, full-cluster resume, verification,
GPU support bundles, and leak-free deletion.

The script defaults to both distros. For a focused local rerun, set
`KIAC_GPU_E2E_PROFILE=k3s` or `KIAC_GPU_E2E_PROFILE=kubeadm` before invoking
`./test/e2e/gpu.sh`.

## GitHub runner

The workflow deliberately has no `pull_request` trigger, and every
self-hosted job requires `refs/heads/main`, including manual dispatch.
This YAML guard protects accidental dispatches; it is not a server-side
security boundary against a branch that changes its own workflow. Before
enabling the jobs, restrict the runner group to this workflow's trusted
`refs/heads/main` revision through GitHub's selected-workflow access policy,
or use isolated ephemeral runners with an equivalent trusted-ref boundary.
Contributor-controlled branches must never execute on a persistent Mac.

The CPU matrix needs two dedicated Apple silicon runners with `macOS` and
`ARM64` labels plus `kiac-runtime-1.6.1` or `kiac-runtime-1.7.1`, respectively.
Provision macOS 26+, `kubectl` and the exact runtime version on each runner
beforehand. The CPU job never upgrades a runtime or registers a personal
machine. Keep repository secrets off those machines. GPU and extended
manual profiles retain the separate `kiac-runtime` label and their existing
krunkit 1.3.2+/vmnet-helper 0.13.0+ prerequisites; the CPU matrix does not
require those GPU dependencies.

The repository variable `KIAC_RUNTIME_CI_ENABLED` must remain `false` until
the dedicated runners are available; this change does not enable it.
Once enabled, code merges and the Monday schedule run the CPU compatibility
matrix, the first day of each month runs `gpu`, and maintainers can dispatch
`compat` or the original extended profiles manually **from main**. The jobs
upload no artifacts. Their runner labels separate the two CPU runtime
versions from the GPU/extended queue.

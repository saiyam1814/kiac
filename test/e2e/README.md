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

The workflow deliberately has no `pull_request` trigger. A public fork
must never execute contributor-controlled code on a self-hosted Mac.
Attach a dedicated Apple silicon runner with the labels `macOS`, `ARM64`,
and `kiac-runtime`, install `apple/container`, `kubectl`, krunkit 1.3.2+, and
vmnet-helper 0.13.0+, and keep repository secrets off that machine. Then set
the repository Actions variable `KIAC_RUNTIME_CI_ENABLED` to `true`.

Once enabled, code merges run `quick`, the Monday schedule runs `full`, the
first day of each month runs `gpu`, and maintainers can dispatch any profile
manually. The job uploads no artifacts, uses only standard public-repository
Actions plus the self-hosted machine, and therefore consumes no paid runner
minutes.

# ⬢ kiac - Kubernetes in Apple Containers

**kiac** runs local Kubernetes clusters where **every node is its own lightweight virtual machine**, powered by [apple/container](https://github.com/apple/container) and the [Containerization](https://github.com/apple/containerization) framework on Apple silicon.

Think `kind`, but instead of sharing one Docker VM, each node's VM boots in about a second (a full cluster takes two to three minutes) - the same idea Weave Ignite pioneered with Firecracker, now native on your Mac with zero extra software between you and the hypervisor.

```
$ kiac create cluster --name dev --workers 2
⬢ kiac v0.1.0 · Kubernetes in Apple Containers
 ✓ Preflight checks (0.3s)
 ✓ Pulling node image kindest/node:v1.34.0 (4.1s)
 ✓ Booting 3 node VM(s) (9.8s)
 ✓ Initializing Kubernetes control plane (32.1s)
 ✓ Joining 2 worker(s) (18.4s)
 ✓ Installing CNI (kindnet) (1.2s)
 ✓ Installing storage (local-path-provisioner) (0.4s)
 ✓ Installing metrics-server (0.9s)
 ✓ Installing LoadBalancer (MetalLB) (1.1s)
 ✓ Waiting for nodes to be Ready (14.0s)
 ✓ Configuring LoadBalancer IP pool (3.2s)
 ✓ Writing kubeconfig (0.4s)

Cluster "dev" is ready in 81s. Every node is its own lightweight VM.
  kubectl get nodes
  kubectl top nodes        # native metrics, give it ~60s to scrape
```

## Why kiac

- **Real VM isolation per node.** apple/container maps each container to one lightweight VM on the Virtualization framework. Your "nodes" are not namespaces sharing a kernel - they are separate machines, like a real cluster.
- **No Docker Desktop, no Lima, no QEMU.** One Swift-native runtime that ships from Apple, one Go binary from us.
- **Metrics that just work.** kubelet and cAdvisor run inside a real Linux VM with real cgroups, and kiac installs metrics-server by default. `kubectl top nodes` works out of the box.
- **PVCs that just bind.** A default StorageClass backed by local-path-provisioner is installed on create, so StatefulSets and `volumeClaimTemplates` work immediately.
- **Direct networking.** On macOS 26+ every node gets its own IP that is reachable from your Mac. Hit NodePorts directly - no port-mapping gymnastics.
- **`type: LoadBalancer` works.** MetalLB ships by default with a pool of the cluster's node IPs, so Services get a real EXTERNAL-IP you can curl from your Mac. No `<pending>`, no tunnels.
- **A console when you want one.** `kiac ui` opens a local web console to create, watch, and delete clusters - same engine as the CLI.
- **kind-compatible workflow.** `create cluster`, `delete cluster`, `load image`, kubeconfig contexts. Your muscle memory transfers.

## Requirements

- Apple silicon Mac
- macOS 26+ for multi-node clusters (single-node works on macOS 15, with limitations)
- [apple/container](https://github.com/apple/container/releases) 1.0.0+
- `kubectl`

Run `kiac doctor` to verify everything.

## Install

```bash
go install github.com/saiyam1814/kiac@latest
```

Or build from source:

```bash
git clone https://github.com/saiyam1814/kiac && cd kiac && make build
```

## Usage

```bash
kiac doctor                                  # check your setup
kiac create cluster                          # single node, K8s 1.36, everything included
kiac create cluster --name dev --workers 2   # 1 control plane + 2 workers
kiac create cluster --k8s-version 1.34       # pick your Kubernetes (1.32-1.36 pinned)
kiac ui                                      # local web console to create/manage clusters
kiac get clusters
kiac get nodes --name dev
container build -t myapp:dev .               # build with apple/container
kiac load image myapp:dev --name dev         # push it into every node
kiac delete cluster --name dev
```

Each cluster gets a kubeconfig context named `kiac-<name>` merged into your `~/.kube/config` (a backup is written to `~/.kube/config.kiac.bak`).

### Flags for `create cluster`

| Flag | Default | Description |
|---|---|---|
| `--name` | `kiac` | cluster name |
| `--workers` | `0` | worker count; control plane is untainted when 0 |
| `--k8s-version` | `1.36` | Kubernetes minor, pinned digests for 1.32-1.36 |
| `--image` | resolved from `--k8s-version` | explicit node image override |
| `--cni` | `kindnet` | pod network: `kindnet` or `none`. Flannel/Calico/Cilium need kernel features (br_netfilter, VXLAN, eBPF) missing from Apple's stock node kernel; custom kernel support is on the roadmap |
| `--cpus` | `4` | vCPUs per node VM |
| `--memory` | `4G` | memory per node VM |
| `--no-metrics` | `false` | skip metrics-server |
| `--no-storage` | `false` | skip the local-path default StorageClass |
| `--no-lb` | `false` | skip MetalLB (`type: LoadBalancer` support) |
| `--wait` | `5m` | node readiness timeout |

## How it works

```
┌─ your Mac (Apple silicon) ──────────────────────────────┐
│  kiac CLI ──drives──▶ apple/container CLI               │
│                          │                              │
│   ┌─── node VM ─────┐ ┌─── node VM ────┐ ┌── node VM ─┐ │
│   │ control-plane   │ │ worker-1       │ │ worker-2   │ │
│   │ systemd         │ │ systemd        │ │ systemd    │ │
│   │ containerd      │ │ containerd     │ │ containerd │ │
│   │ kubelet + etcd  │ │ kubelet        │ │ kubelet    │ │
│   │ kube-apiserver  │ │ pods…          │ │ pods…      │ │
│   └─────────────────┘ └────────────────┘ └────────────┘ │
│        vmnet network: every node has a routable IP      │
└─────────────────────────────────────────────────────────┘
```

kiac boots each node from the standard `kindest/node` image (systemd, containerd, kubeadm preinstalled), initializes the control plane with `kubeadm`, joins workers, applies the kindnet CNI bundled in the node image, and installs metrics-server configured for kubeadm's self-signed kubelet certificates.

kiac coexists peacefully with Docker Desktop, Rancher Desktop, kind, k3d, and friends - it talks only to the apple/container runtime and never touches the Docker socket.

## Roadmap

- `container machine` backed nodes (WWDC26's persistent Linux environments)
- HA control planes
- Per-node `--kernel` and resource overrides via a config file
- Ingress helper
- Custom node kernels (`--kernel`) to unlock Flannel, Calico, and Cilium
- Built-in LoadBalancer controller to replace MetalLB (node-IP allocation needs no ARP speaker)

## License

MIT

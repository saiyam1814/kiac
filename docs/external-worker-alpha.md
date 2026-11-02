# External K3s worker (alpha)

`kiac join worker` creates one owned Fedora/krunkit VM and joins it to an existing IPv4 K3s cluster. This alpha supports the cluster's existing Flannel VXLAN network over one operator-managed WireGuard peer. It does not configure the remote control plane, other nodes, CNI, addons, or your host kubeconfig.

The VM advertises its stable WireGuard address to Kubernetes. Its separate private vmnet DHCP address remains available to Kiac for local management. No host routes, firewall settings, or host VPN are changed. Krunkit exposes Venus to its guests, but this worker installs no GPU resource driver and advertises no Kiac GPU resource.

## Requirements

- An Apple silicon Mac with the same krunkit 1.3.2+, vmnet-helper 0.13.0+, renderer dependencies and helper service as Kiac's GPU backend. Apple/container is not required. Kiac uses the verified Fedora 44 image and pinned K3s ARM64 binary.
- At least 7 GiB of available host disk space on the krunkit state filesystem before joining: 4 GiB reserve plus 3 GiB for the new VM. This thin-provisioning estimate does not reserve disk space or guarantee later guest-disk growth.
- An existing K3s cluster on one of Kiac's pinned releases, using IPv4 and Flannel VXLAN. The exact K3s server and worker releases must match. Kubeadm, other CNIs, dual-stack and remote GPU registration are outside this alpha.
- A reachable numeric IPv4 WireGuard endpoint with a peer already authorized for the new worker. The API TLS certificate must cover the VPN API address. Every remote node must already have bidirectional VPN routes to the new worker; Flannel must use those reachable node addresses/interfaces. Nodes exchange Flannel UDP 8472 over the tunnel. K3s normally carries control-plane-to-kubelet requests through the agent's outbound websocket tunnel; direct inbound kubelet TCP 10250 requirements depend on the cluster's egress-selector mode. Opening only the API port does not provide pod-network reachability.
- The cluster's actual pod and service CIDRs. They must be distinct from the API, worker VPN and VM management ranges. Kiac checks the local Flannel pod network after joining; the service CIDR is an operator-declared route contract.
- A full CA-pinned K3s secure token obtained through your trusted cluster administration channel. Prefer the agent token from the server's `/var/lib/rancher/k3s/server/agent-token`. Short tokens are refused. Treat this token and the WireGuard private key as secrets.

Prepare the remote routing and WireGuard peer before joining. A suitable topology is a reachable Linux gateway/control plane as the one peer, forwarding the existing cluster's node, pod and service routes. The guest initiates the UDP tunnel through vmnet NAT and sends periodic keepalives. The remote peer must route return traffic to the worker's `/32`; it must preserve node addresses for Flannel traffic. The outer endpoint must remain outside all tunnel routes to avoid a routing loop. This command does not make an arbitrary remote cluster reachable automatically.

## Join

Store both files with owner-only permissions (`chmod 600`). Symlinks, special files, hooks, DNS endpoints, default routes, multiple peers and unsupported WireGuard fields are refused. Supported fields are `Address`, `PrivateKey`, optional `MTU` (1280–1420), and one peer's `PublicKey`, optional `PresharedKey`, `Endpoint`, `AllowedIPs`, optional `PersistentKeepalive` (1–120 seconds). Defaults are MTU 1420 and keepalive 25 seconds. The fixed guest interface is `kiacwg0`.

For example, a private `worker.conf` contains:

```ini
[Interface]
Address = 10.250.76.2/32
PrivateKey = <worker-private-key>

[Peer]
PublicKey = <gateway-public-key>
Endpoint = 192.0.2.10:51820
AllowedIPs = 10.250.76.0/24, 10.42.0.0/16, 10.43.0.0/16
PersistentKeepalive = 25
```

Replace the example endpoint and keys with the real peer's details. `AllowedIPs` must cover the VPN API and the complete declared pod/service CIDRs. All routes are inside the guest; they must not overlap its vmnet management `/24`.

```bash
kiac join worker --name remote-lab \
  --server https://10.250.76.1:6443 \
  --token-file ./agent-token --wireguard-config ./worker.conf \
  --node-ip 10.250.76.2 --k8s-version 1.37 \
  --pod-cidr 10.42.0.0/16 --service-cidr 10.43.0.0/16
```

`--k8s-version` accepts a pinned minor or exact release. The command defaults to 2 vCPUs, 2G memory, a 20G persistent disk and a ten-minute readiness wait. Resource options are `--cpus` (1–32), `--memory` (1–60G), `--disk` (8–1024G), and `--wait` (up to one hour). Package installation has a separate bounded ten-minute minimum budget.

The K3s agent verifies the token's CA hash before sending credentials. Kiac waits for the agent's own kubelet certificate to read its Node, verifies its owner label, VPN InternalIP, exact API/kubelet release, Flannel VXLAN pod CIDR, initial scheduling taint and Ready condition, then pins that Node's UID. A successful join proves registration and node readiness; validate workload traffic for your topology separately.

New workers carry `kiac.dev/external-worker=true:NoSchedule` so ordinary workloads do not land on a laptop merely because it joined. DaemonSets or other workloads that already tolerate all taints can still schedule. Opt an intended workload in with both a worker label selector and a toleration:

```yaml
spec:
  nodeSelector:
    kiac.dev/external-worker: "true"
  tolerations:
    - key: kiac.dev/external-worker
      operator: Equal
      value: "true"
      effect: NoSchedule
```

Keep this fragment under the pod spec (for a Deployment, `spec.template.spec`). After a successful join, an operator may deliberately remove the taint; later `start worker` respects that choice.

Credentials cross the management channel only through standard input and are stored in private files inside the VM. Host inventory under `~/.kiac/workers` contains public identity and a CA hash, never the token or WireGuard private key. Failures before agent startup remove only the just-created owned local VM. Once startup may have registered a remote Node, Kiac retains the VM and its node-password with phase `joining`; fix the external network or cluster issue and run `kiac start worker NAME` to finish the same join. This retry uses the saved guest files, even if the original host credential files are gone. An ownership mismatch or cleanup failure also retains inventory.

## Lifecycle

```bash
kiac get workers
kiac get workers -o json
kiac logs worker remote-lab
kiac stop worker remote-lab
kiac start worker remote-lab --wait 10m
```

Starting checks the original Node UID and advertised VPN address again, and waits for the Node's reported boot ID to match the running guest's current boot ID. A cached Ready condition from before a reboot does not satisfy readiness. The fixed tunnel and K3s units start on guest boot. Log output redacts the saved token, its credentials and private WireGuard keys inside the guest before returning it to the host. Command failures omit private remote output.

Workers have a separate inventory and `kiacx-worker-…` VM namespace. Cluster delete, resume, verify, support bundles and the web UI do not manage these workers. Deleting an external worker only removes its owned local VM and inventory; it never calls the external cluster's administrative API.

Drain and delete the remote Node with your own external-cluster credentials first. Use the VM/Node name from `kiac get workers -o json`:

```bash
kubectl --context YOUR_EXTERNAL_CONTEXT drain NODE_NAME --ignore-daemonsets
kubectl --context YOUR_EXTERNAL_CONTEXT delete node NODE_NAME
kiac delete worker remote-lab
```

Stopping also affects only the local VM and does not drain workloads. Removing the local VM before draining or deleting its Node leaves that responsibility with the operator.

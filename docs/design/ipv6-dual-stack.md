# IPv6 and dual-stack clusters

`kiac create cluster --ip-family {ipv4|dual|ipv6}` selects the address
families a cluster's pods, Services, and nodes use. This documents why
the feature is shaped the way it is and how each moving part was
verified. Background: issue #10, and the community PoC in
cpressland/kiac@ec431ca that this builds on.

## The problem it fixes

A user booted a dual-stack PoC and found pod-to-pod and node-to-node IPv6
worked, but **Services never connected**. On the node:

```
$ ip6tables -L
ip6tables v1.8.11 (legacy): can't initialize ip6tables table `filter':
Table does not exist (do you need to insmod?)
```

The cause is below Kubernetes. The default kernel `apple/container` boots
(Kata's generic VM kernel) is built without IPv6 netfilter and with
`CONFIG_MODULES` off, so there is nothing to `insmod` and no way to create
the IPv6 `filter`/`nat` tables. kube-proxy probes, logs "No iptables
support for family IPv6", and silently programs only IPv4 rules, so every
IPv6 ClusterIP/NodePort/LoadBalancer is a black hole. Pod and node IPv6
work because routing needs no netfilter.

kiac's **full** kernel (`kernel/config-full`, built on Apple's
`config-arm64`) already includes the complete `IP6_NF_*` block. So the fix
is not new kernel work: it is requiring the full kernel for any non-ipv4
family and wiring Kubernetes for dual-stack. `kernel/config-full` now
re-asserts the IPv6 netfilter options so the CI guard fails the build if
an upstream base-config change ever drops them.

## Modes

- **ipv4** (default): unchanged. Stock kernel, single-stack IPv4. A zero
  `Config` and every existing cluster behave exactly as before.
- **dual**: IPv4-primary, IPv6 secondary. `kubeadm --pod-network-cidr`
  and `--service-cidr` (and the k3s `--cluster-cidr`/`--service-cidr`)
  carry both families, v4 first. Services are v4 by default and opt into
  v6 with `ipFamilyPolicy`. The host still reaches the apiserver over v4.
- **ipv6**: IPv6-primary single-stack at the Kubernetes layer. The
  apiserver advertises the node's v6, the host kubeconfig targets a
  bracketed v6 endpoint, and node InternalIPs are v6. Nodes keep their
  vmnet IPv4 for image pulls and other egress; this is not a link-level
  v6-only node. kubeadm only for now (see limitations).

Non-ipv4 families auto-select `--kernel full` (a small, sha-pinned
download) and fail preflight early if the container network advertises no
IPv6 subnet (`container network inspect default`), which needs macOS 26+.

## How the pieces fit

- **kubelet node IP.** kubelet's single-family auto-detection picks one
  address and, on dual-stack, often the wrong one, so kiac pins
  `--node-ip=v4,v6` (dual) or `=v6` (ipv6) in `/etc/default/kubelet`
  before kubelet first starts. The kindest/node systemd drop-in sources
  that file last, so it overrides kubeadm's own flags.
- **IPv6 comes from a router advertisement.** The vmnet global v6 address
  is assigned by SLAAC a beat after boot, so the code polls for it rather
  than reading once. Critically, `accept_ra=2` is set on the primary
  interface *before* enabling `net.ipv6 forwarding`: turning on forwarding
  otherwise disables the default `accept_ra=1`, and the node never
  acquires (or later loses, at RA-lifetime expiry) its v6 address. This
  bit only the k3s path at first, because k3s runs as PID 1 and its boot
  preamble executes before any SLAAC; kubeadm nodes acquire v6 during
  their own init.
- **kiac-lb** reads each Service's `spec.ipFamilies` and assigns a node
  address per requested family, so a dual-stack LoadBalancer gets both a
  v4 and a v6 ingress. IPv4-only clusters produce byte-identical status to
  before.
- **Edge proxy.** The node-local proxy that fixes large-upload TSO stalls
  was IPv4-only. Testing confirmed the same vmnet TSO forwarding bug hits
  IPv6 (a 150MB cross-node download stalled at 45MB over v6 while v4 ran
  at ~1GB/s), so the proxy now runs a second listener on `[::]:15080`,
  programs a parallel `KIAC-EDGE` chain in `ip6tables`, reads the IPv6
  `SO_ORIGINAL_DST`, and tunnels to the endpoint node over the matching
  family. After the fix the v6 transfer matched v4 (~1.8GB/s). On a stock
  (v4-only) kernel it detects the missing IPv6 nat table and runs
  IPv4-only rather than failing.
- **resume/heal.** A reboot changes both a node's v4 and v6. kubeadm's
  v4-primary heal (cert, kubeconfigs, kube-proxy ConfigMap) is unchanged;
  resume additionally re-pins each node's `--node-ip` from its current
  addresses when the node was created dual/ipv6 (detected from
  `/etc/default/kubelet`, so ipv4 clusters are untouched). k3s agents
  compute both node addresses at boot, while resume updates their server
  URL, waits for the current Node address set, and refreshes host-network
  pods. IPv6-only kubeadm resume still fails with a clear "recreate"
  message rather than a confusing error.

## Verified end to end

On macOS with `apple/container` 1.0.0 and the full kernel, per distro:

| Angle | dual (kubeadm) | dual (k3s) | ipv6 (kubeadm) |
|---|---|---|---|
| Node InternalIPs are dual / v6 | ✅ | ✅ | ✅ (v6) |
| ClusterIP over IPv6 (the issue) | ✅ | ✅ | ✅ |
| ClusterIP over IPv4 (no regression) | ✅ | ✅ | n/a |
| DNS returns A and AAAA | ✅ | ✅ | ✅ (AAAA) |
| NodePort over IPv6 from the Mac (cross-node) | ✅ | ✅ | ✅ |
| LoadBalancer v6 ingress reachable from the Mac | ✅ (dual) | ✅ (dual) | ✅ (v6) |
| Large upload over v6 (TSO), cross-node | ✅ ~1.8GB/s | — | ✅ ~1.5GB/s |
| Host kubectl over bracketed v6 endpoint | n/a | n/a | ✅ |
| Two-node (worker join) | ✅ | ✅ | ✅ |
| `kiac resume` heals both families | ✅ | supported (IPv4 resume E2E) | recreate (gated) |

Both dual columns were run with a worker. k3s resume is implemented for
dual-stack clusters and its IPv4 path has full reboot E2E coverage; a
separate dual-stack reboot run has not yet been benchmarked. The k3s TSO
cell was not measured separately. IPv6-only kubeadm resume is
intentionally gated to recreate.

## Limitations

- **arm64 / macOS 26+ only**, and the container network must be
  IPv6-enabled (the default vmnet network is on macOS 26+).
- **ipv6-only on k3s** is rejected (needs pre-boot apiserver cert SANs the
  kubeadm path handles); use `--distro kubeadm`, or `--ip-family dual`.
- **ipv6-only resume** is not yet supported; recreate the cluster.
- **Cilium** dual-stack is not wired (`--cni cilium` with a non-ipv4
  family is rejected); kindnet is the dual-stack CNI on both distros.
- The IPv6 CIDRs are kind's ULA defaults (`fd00:10:*`), not routable
  prefixes; this is a local development cluster.

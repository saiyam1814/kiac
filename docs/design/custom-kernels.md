# Custom node kernels

Ordinary KIAC clusters boot the kernel configured in apple/container unless
`--kernel` selects another image. Runtime upgrades can retain older kernels;
the CLI version alone does not identify the kernel a node will boot.

## Supported selection

```sh
kiac create cluster --kernel full
kiac create cluster --cni cilium --kernel full
kiac create cluster --cni flannel --kernel full
kiac create cluster --kernel /absolute/path/to/Image --cni none
```

`full` downloads KIAC's published ARM64 kernel into `~/.kiac/kernels/`.
`pkg/cluster/kernel.go` pins its SHA256 and verifies downloads and cache
hits. An explicit local path selects the user's kernel image instead.
Selection is stored with the cluster and passed to the runtime at node
creation. GPU clusters boot the kernel in their Fedora disk and reject
`--kernel`.

## CNI requirements

- Kindnet is the default and supports the older minimal 6.12.28 kernel.
- Ordinary Cilium clusters require an explicit kernel and the host Cilium
  CLI. KIAC runs the official installer.
- Ordinary Flannel clusters require an explicit kernel. KIAC installs the
  embedded upstream v0.28.9 VXLAN manifest and supplies its bridge delegate
  plugin from the verified CNI archive. This path supports IPv4 kubeadm
  clusters; it does not enforce NetworkPolicy.
- Calico is not bundled. `--cni none` leaves CNI installation to the user.

The 6.18.35 kernel recommended by apple/container 1.4.1 includes many
features absent from older retained kernels. KIAC still requires an
explicit kernel for ordinary Cilium and Flannel clusters. Non-IPv4
clusters auto-select the full kernel for IPv6 netfilter and use kindnet.

## Build and validation

[The kernel directory](../../kernel/README.md) describes the pinned build
inputs and configuration checks. The workflow verifies the source and
base-config checksums, merges `kernel/config-full`, rejects missing
required options, and publishes the ARM64 boot Image and checksum.

`test/e2e/run.sh flannel` exercises four nodes, cross-node traffic,
Gateway, observability, host mounts and worker restart. The dual-stack
profile checks IPv4 and IPv6 traffic. `kiac verify cluster` reports CNI
health, and `kiac support bundle` includes CNI diagnostics.

See the [networking guide](../docs/networking.html#kernel) and
[Cilium guide](../docs/cilium.html) for user-facing setup instructions.

# Custom node kernels (`--kernel`): groundwork

Status: groundwork landed, integration pending. No kernel is compiled or
wired into `kiac create` yet; this doc records the investigation, the
pipeline that now exists, and the exact wiring left to do.

Goal: one flag, `kiac create cluster --kernel full`, boots node VMs on a
kernel that has everything Cilium, Calico, and flannel need (VXLAN,
GENEVE, br_netfilter, eBPF JIT + BTF, WireGuard), lifting the current
`--cni` restriction to kindnet/none.

## Findings (verified 2026-07-06 on this machine)

### The `container` CLI already supports custom kernels

- `container` CLI 1.0.0 (build `ee848e3`, latest release 2026-06-09)
  exposes `container run -k, --kernel <path>  Set a custom kernel path`.
  No new runtime capability is needed; kiac only has to pass the flag.
- `container system property list` shows where the default kernel comes
  from: `[kernel] url = kata-static-3.28.0-arm64.tar.zst`, `binaryPath =
  opt/kata/share/kata-containers/vmlinux-6.18.15-186`. The default is
  Kata Containers' generic VM kernel, not one built from Apple's config.
- The kernel actually cached and booting nodes on this machine is
  `~/Library/Application Support/com.apple.container/kernels/vmlinux-6.12.28-153`
  (symlinked as `default.kernel-arm64`). `file` identifies it as
  "Linux kernel ARM64 boot executable Image": despite the `vmlinux-`
  name, the boot format on arm64 is the uncompressed
  `arch/arm64/boot/Image`.
- That Kata kernel is minimal and monolithic. It is why flannel, Calico,
  and Cilium fail today: no VXLAN, no br_netfilter, no BPF JIT, and
  `CONFIG_MODULES` off, so nothing can be loaded at runtime.

### apple/containerization ships kernel build tooling

Repo: github.com/apple/containerization, latest tag 0.35.0 (checked
2026-07-06). Its `kernel/` directory contains:

- `config-arm64` / `config-x86_64`: full kernel configs, currently
  targeting kernel.org `linux-6.18.5.tar.xz` (`KSOURCE` in
  `kernel/Makefile`; same pin at tags 0.30.0 through 0.35.0).
- `build.sh`: copies the config to `.config`, runs `make ARCH=arm64
  olddefconfig`, builds, and copies `arch/arm64/boot/Image` out as
  `vmlinux-arm64`. This confirms the arm64 artifact Containerization
  expects is the uncompressed Image (x86_64 uses compressed bzImage,
  `vmlinuz-x86_64`).
- `image/Dockerfile`: ubuntu:focal build container with cross
  toolchains. Note: it has no `dwarves`/pahole, so it cannot build a
  BTF-enabled kernel as-is; our workflow uses its own build container.
- Custom kernels are first-class in the framework API (per-container
  kernel selection) and in the CLI via `--kernel`; their own Makefile
  passes `--kernel kernel/vmlinux-<arch>` to `container run` for
  integration testing.

### Apple's base config vs Cilium's requirements

Apple's `config-arm64` at tag 0.35.0 (sha256
`cc6cc583...505de1cc`, identical on main) already enables most of what
CNIs need: `VXLAN`, `GENEVE`, `BRIDGE_NETFILTER`, `WIREGUARD`,
`MACVLAN`, `IPVLAN`, `VETH`, `BRIDGE`, `XDP_SOCKETS`, `NET_CLS_BPF`,
`NET_CLS_ACT`, `NET_SCH_INGRESS`, `NET_SCH_FQ`, `FIB_RULES`, `IP_SET`,
the `NETFILTER_XT_*` set, all `=y` with `# CONFIG_MODULES is not set`.

Still missing from it, per Cilium's System Requirements
(docs.cilium.io, `Documentation/operations/system_requirements.rst`,
Cilium minimum kernel 5.10):

| Option | State in Apple base | Consumer |
| --- | --- | --- |
| `CONFIG_BPF_JIT` | absent from the file | Cilium base requirement |
| `CONFIG_BPF_EVENTS` | absent from the file | Cilium base requirement |
| `CONFIG_DEBUG_INFO_BTF` | blocked (`DEBUG_INFO_NONE=y`) | Cilium base requirement |
| `CONFIG_CRYPTO_USER_API_HASH` | not set | Cilium base requirement |
| `CONFIG_SCHEDSTATS` | not set | Cilium base requirement |
| `CONFIG_BPF_STREAM_PARSER` | not set | Cilium sockmap datapath |
| `CONFIG_NETKIT` | absent (needs >= 6.8) | Cilium netkit device mode |

`kernel/config-full` supplies these and re-asserts the rest so base
drift cannot regress a requirement (each option's consumer is cited in
a comment in that file). Everything stays `=y`: the model remains
monolithic, zero loadable modules, matching Apple's and Kata's kernels.

## What landed tonight

- `kernel/config-full`: the merge fragment described above.
- `.github/workflows/kernel-build.yml`: manual + `kernel-v*` tag
  triggered job on `ubuntu-24.04-arm`. Pins kernel.org `linux-6.12.28`
  by sha256 (matching the 6.12.28 Kata kernel the CLI boots today, to
  minimize guest ABI drift), pins Apple's `config-arm64` at tag 0.35.0
  by sha256, merges the fragment with `scripts/kconfig/merge_config.sh`
  inside a `docker run ubuntu:24.04` container (hermetic toolchain,
  includes `dwarves` for BTF), asserts every fragment option survived
  `olddefconfig`, builds the uncompressed arm64 `Image`, and uploads
  `kiac-kernel-6.12.28-full` plus a `.sha256` (release assets on tag
  builds).
- `pkg/cluster/kernel.go`: `ResolveKernel(nameOrPath)`. Empty in, empty
  out (runtime default kernel). Existing file paths pass through. The
  name `full` downloads the release asset to `~/.kiac/kernels/` with
  sha256 verification against a pinned table, verified again on every
  cache hit. The pinned checksum is intentionally empty until the first
  release exists; `ResolveKernel("full")` fails with a clear message
  until then. Tests in `pkg/cluster/kernel_test.go` (httptest server).

## Wiring plan (the 5 lines, plus the CNI gate)

Integration touches files owned by another workstream; the exact
changes, in order:

1. `pkg/runtime/container.go`, `RunOpts`: add `Kernel string // local
   path to an arm64 boot Image; empty uses the runtime default`.
2. `pkg/runtime/container.go`, `RunDetached`: after the memory flag,
   `if o.Kernel != "" { args = append(args, "--kernel", o.Kernel) }`.
3. `pkg/cluster/cluster.go`, `Config`: add `Kernel string // resolved
   by ResolveKernel; empty keeps the runtime default kernel`.
4. `pkg/cluster/cluster.go`, `Create`, the `RunDetached` call: add
   `Kernel: cfg.Kernel` to the `runtime.RunOpts{...}` literal.
5. `cmd/create.go`: `f.StringVar(&kernelFlag, "kernel", "", "custom
   node kernel: 'full' or a path to an arm64 boot Image")`, then before
   `Create`: `createCfg.Kernel, err = cluster.ResolveKernel(kernelFlag)`
   (wrapped in a `ui.Step("Resolving kernel")` when it names a
   download).

CNI gate lifting, in `installCNI` (pkg/cluster/cluster.go): the
`"flannel", "calico", "cilium"` case keeps today's error when
`cfg.Kernel == ""` and otherwise proceeds: flannel applies an embedded
manifest with the vxlan backend; calico/cilium print the exact helm/
`cilium install` command (their installers probe kernel features
themselves) or apply pinned manifests once we commit to versions.
Follow-ups: `kernel` key in `FileConfig` (configfile.go) and surfacing
the kernel in `kiac get nodes -o wide`.

## Risks

- Kernel ABI vs vminitd expectations: vminitd (Containerization's guest
  init) needs virtio-blk/net/fs and vsock support in the guest kernel.
  Apple's base config provides these; building 6.12.28 from a config
  written for 6.18.5 relies on `olddefconfig` for reconciliation. The
  workflow's assert step catches dropped CNI options, but a dropped
  virtio option would only surface at boot, hence the boot test below.
- macOS version coupling: Virtualization.framework loads the arm64
  Image directly and is not expected to care about the kernel version,
  but each macOS/container CLI upgrade can move the default kernel
  (property list already points at 6.18.15 while the cache holds
  6.12.28). Re-run the boot test after CLI upgrades; bump
  `KERNEL_VERSION` when the default moves majors.
- Toolchain: `CONFIG_DEBUG_INFO_BTF` requires pahole; the workflow's
  ubuntu:24.04 container ships dwarves 1.25. Apple's focal builder
  image cannot be reused for this.
- kindest/node images assume kernel features via kubeadm preflight;
  `--ignore-preflight-errors=all` already masks that, and a richer
  kernel only removes failure modes there.

## Validation plan

1. Boot test (no Kubernetes): run a throwaway container with
   `--kernel` pointing at the built Image, check `uname -r` reports
   `6.12.28-kiac-full`, then probe features: create a vxlan and a
   geneve link, a bridge with `bridge-nf-call-iptables=1`, a wireguard
   link, and load a trivial BPF program (`bpftool feature` or
   `ip link set dev lo xdpgeneric obj ...`).
2. kiac boot test: `kiac create cluster --kernel full` with the default
   kindnet CNI; confirms kubeadm and the existing datapath still work
   on the new kernel before any CNI change.
3. Cilium e2e: `--kernel full --cni none`, install Cilium with kube-
   proxy replacement, run `cilium connectivity test`, then the same for
   flannel (vxlan backend) and Calico.
4. Only after 1-3 pass: flip the `--cni` gate and pin the checksum in
   `kernelAssets`.

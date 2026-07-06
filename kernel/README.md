# kiac node kernels

kiac node VMs boot whatever kernel the `container` runtime supplies. The
default (Kata Containers' generic VM kernel, monolithic, no modules)
lacks VXLAN, GENEVE, br_netfilter, the eBPF JIT, and BTF, which is why
`--cni` is limited to kindnet/none today.

This directory holds the config for the kiac **full** kernel, which
adds those features so Cilium, Calico, and flannel can run. Design and
wiring plan: `docs/design/custom-kernels.md`.

## Contents

- `config-full`: a kconfig merge fragment applied on top of
  apple/containerization's `kernel/config-arm64` (pinned by ref and
  sha256 in the workflow). Every option is `=y` and cites the CNI or
  feature that needs it; the kernel stays monolithic with zero
  loadable modules, matching Apple's model.

## Building

Builds run in CI, not on laptops: `.github/workflows/kernel-build.yml`
(manual `workflow_dispatch`, or push a `kernel-v*` tag to publish
release assets). The job:

1. Downloads kernel.org `linux-6.12.28.tar.xz` (sha256-pinned; 6.12.28
   matches the Kata kernel `container` 1.0.0 boots, minimizing ABI
   drift for vminitd).
2. Fetches Apple's `config-arm64` at containerization tag 0.35.0
   (sha256-pinned).
3. Inside `docker run ubuntu:24.04` (hermetic toolchain, includes
   `dwarves` for BTF): merges `config-full` with
   `scripts/kconfig/merge_config.sh`, runs `make olddefconfig`, fails
   if any fragment option was dropped, and builds the uncompressed
   `arch/arm64/boot/Image`. That Image format is what Containerization
   boots on arm64 (its own build.sh ships `arch/arm64/boot/Image` as
   `vmlinux-arm64`).
4. Publishes `kiac-kernel-<kver>-full` and a `.sha256` file.

## Using a built kernel

`ResolveKernel` in `pkg/cluster/kernel.go` maps the name `full` to the
release asset, downloads it once to `~/.kiac/kernels/`, and verifies
its sha256 against the checksum pinned in `kernelAssets` on download
and on every cache hit. After the first release, copy the digest from
the `.sha256` asset into that table.

Until `--kernel` is wired into `kiac create` (see the design doc), a
built Image can be tried directly:

```sh
container run --rm -it --kernel ~/.kiac/kernels/kiac-kernel-6.12.28-full alpine uname -r
```

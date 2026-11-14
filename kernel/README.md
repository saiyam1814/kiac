# kiac node kernels

Ordinary KIAC node VMs use the kernel configured in apple/container.
Older installations can retain a minimal kernel without the features
needed by overlay and eBPF CNIs. KIAC's explicit `--kernel full` path
provides a checksum-pinned kernel for Cilium and Flannel; the runtime's
newer default kernel does not change that selection requirement.

This directory holds the configuration for that full kernel. See the
[implemented kernel-selection behavior](../docs/design/custom-kernels.md).

## Contents

- `config-full`: a kconfig merge fragment applied on top of
  apple/containerization's `kernel/config-arm64` (pinned by ref and
  sha256 in the workflow). Required capabilities are `=y` and cite
  their consumers; module loading stays disabled for a monolithic kernel with zero
  loadable modules, matching Apple's model.

## Building

Builds run in CI, not on laptops: `.github/workflows/kernel-build.yml`
(manual `workflow_dispatch`, or push a `kernel-v*` tag to publish
release assets). The job:

1. Downloads kernel.org `linux-6.18.56.tar.xz` (sha256-pinned).
2. Fetches Apple's `config-arm64` at containerization tag 0.54.0
   (sha256-pinned).
3. Inside `docker run ubuntu:24.04` (hermetic toolchain, includes
   `dwarves` for BTF): merges `config-full` with
   `scripts/kconfig/merge_config.sh`, runs `make olddefconfig`, fails
   if any required enabled or disabled fragment option was dropped, and builds the uncompressed
   `arch/arm64/boot/Image`. That Image format is what Containerization
   boots on arm64 (its own build.sh ships `arch/arm64/boot/Image` as
   `vmlinux-arm64`).
4. Uploads the Image, resolved `.config`, and `.sha256` as a build
   artifact. Tag builds publish the Image and checksum as release assets.

## Using a built kernel

`ResolveKernel` in `pkg/cluster/kernel.go` maps the name `full` to the
release asset, downloads it once to `~/.kiac/kernels/`, and verifies
its sha256 against the checksum pinned in `kernelAssets` on download
and on every cache hit. New kernel releases must update that pin after
validating their published artifact.

Select the published kernel or an explicit local build:

```sh
kiac create cluster --kernel full
kiac create cluster --kernel /absolute/path/to/Image
```

# kiac GPU build plan (blueprint v3 → code)

Agent-ready engineering plan for "Apple GPU as a first-class Kubernetes device."
Self-contained: everything you need is in this file plus the referenced kiac code.
Derived from blueprint v3 (12-agent research synthesis, 2026-09-03) and a 4-agent
code recon of this repo (2026-09-04). When this plan and the code disagree, the
code wins — re-verify line numbers before editing; they drift.

## 0. Mission and non-goals

Ship, in order: a mock GPU resource on today's clusters (P1), krunkit-backed GPU
node VMs joined to the same cluster (P2), a schedulable `kiac.dev/gpu` device with
GFD-shaped labels (P3), an nvidia.com/gpu compat bridge (P4), the first DRA driver
for an Apple GPU (P5), then APIR pools / standalone stack / multi-Mac (P6-P8).

Hard non-goals (never build, never imply):
- No CUDA/NVML emulation. nvidia-smi, torch.cuda, DCGM never work. CUDA images
  never run; docs carry the Vulkan image swap table instead.
- No node ever advertises `nvidia.com/gpu` in capacity. The compat bridge is a
  per-pod admission rewrite with an audit annotation.
- No MLX or Metal API inside pods; no in-cluster tensor parallelism (TB5 RDMA is
  host-native); no macOS nodes (no macOS kubelet exists).
- No per-pod GPU compute throttling on private APIs (no public per-process Metal
  priority exists). Enforcement claims are limited to what the VM memory window
  actually enforces.
- Not competing on raw speed. Host-native Ollama/MLX is faster and we say so;
  `kiac gpu bench` prints the honest numbers.
- APIR (P6) is never multitenant: single-tenant taint, always.

## 1. Verified facts you must not re-litigate (Sept 2026)

- apple/container will never expose the GPU (containerization#46 wontfix). GPU
  nodes REQUIRE a second VM backend: krunkit (libkrun, Hypervisor.framework,
  virtio-gpu + Venus → virglrenderer → MoltenVK → Metal), ~75-80% of native.
- Prior art: minikube v1.37.0 ships an official krunkit driver + "AI playground"
  tutorial (K8s inside a krunkit VM, pods share /dev/dri via
  squat/generic-device-plugin, resource `devic.es/dri`). Single node, no DRA, no
  memory accounting. That is the bar to beat, and proof the pattern works.
- Modern guest userspace breaks on a 16KiB blob-alignment bug (krunkit#114), NOT
  ABI drift; fix must land in Mesa (VIRTIO_GPU_F_BLOB_ALIGNMENT). Pin workload
  images to Mesa 25.0.x-era userspace (Fedora 42 class; quay.io/ramalama works).
- Per-VM RAM+vRAM addressable is ~62GiB (36-bit IPA); krunkit auto-sizes the GPU
  shm window as (62GiB - guest RAM). libkrun PR #765 (40-bit) lifts this later.
  `krun_set_gpu_options2(shm_size)` is the per-VM hard vRAM cap primitive.
- APIR (near-native llama.cpp): guest side merged upstream (GGML_VIRTGPU,
  llama.cpp, 2026-01-28); virglrenderer !1590 + libkrun #508 unmerged — shipping
  APIR means carrying those two patches. APIR disables Venus in that VM and is
  not multitenant safe. Different container image (GGML_VIRTGPU build), never
  "same YAML".
- K8s 1.37: DRA extended-resource mapping is STABLE
  (`DeviceClass.spec.extendedResourceName`). DRA consumable capacity: on by
  default 1.36+, feature-gate flip on 1.34-1.35.
- Resource-name plurality is normal in production (nvidia.com/gpu, gpu.shared,
  mig-1g.5gb, HAMi gpumem/gpucores; autoscaler#8095). Charts parameterize the
  name (vLLM production-stack `requestGPUType`, KServe
  `serving.kserve.io/gpu-resource-types`, Ollama chart raw resources).

## 2. Contracts (fixed now; changing any of these later is a breaking change)

- Alpha resource name: `kiac.dev/gpu` — define once as `const gpuResourceDomain =
  "kiac.dev"` / `const gpuResourceName = "kiac.dev/gpu"` in pkg/cluster/gpu.go.
  `kiac.dev` did not resolve in DNS on 2026-09-04, so this is explicitly an
  unstable alpha contract. Confirm control of the final domain before P3; keep
  it a single const so that pre-stable rename remains one line.
- Node labels (GFD-shaped, on GPU + mock nodes):
  `kiac.dev/gpu.present=true`, `kiac.dev/gpu.product` ("Apple M-series (Venus)"
  or "Mock"), `kiac.dev/gpu.memory` (MiB, the VM window), `kiac.dev/gpu.count`,
  `kiac.dev/gpu.api` (venus|apir|mock).
- GPU node naming: `kiac-<cluster>-gpu-<n>` (new suffix class; see P2 grammar
  checklist). Taint on GPU nodes: `kiac.dev/gpu=true:NoSchedule`. APIR nodes add
  `kiac.dev/gpu-apir=true:NoSchedule` (single-tenant enforced by a
  one-workload-per-node admission rule in P6, not by count).
- RuntimeClass: install a pass-through `kiac-gpu` RuntimeClass with handler
  `runc` so GPU-shaped manifests can select the alpha explicitly. Do not create
  or replace `nvidia`: k3s already owns that immutable RuntimeClass with handler
  `nvidia`, and changing it breaks installation while impersonating a runtime
  that kiac does not provide. The Kubernetes API requires a non-empty handler;
  verify `runc` on both kindest/node and k3s.
- Compat rewrite annotation: `kiac.dev/rewrote-gpu-resource: "nvidia.com/gpu"`.
- Verify check IDs (SchemaVersion 1 stays 1; append-only):
  `gpu.runtime-class`, `gpu.device-plugin`, `gpu.nodes`, `gpu.node.<name>`. MUST
  report Skip (never Fail) on clusters without the feature — see
  pkg/cluster/verify.go:59 contract comment (verify.go:35).
- Version pins (record in code tables per repo convention, never inline):
  krunkit >= 1.3.2 (`brew tap slp/krun`, then `brew install krunkit`; the old
  `slp/krunkit` tap is deprecated),
  vmnet-helper (minikube-documented release), workload demo image
  quay.io/ramalama pinned to an exact tag+digest at implementation time,
  generic-device-plugin pinned by digest, K8s default 1.37 (already
  DefaultK8sVersion in pkg/cluster/versions.go).

## 3. Repo strategy

- kiac repo (this one): backend, CLI, embedded manifests, docs. Everything in
  P0-P2 and the kiac halves of P3-P5.
- NEW repo `macgpu` (create at P3 start): the standalone stack — Helm chart
  (device-plugin config, labeler, compat webhook, later DRA driver), component
  images, `macgpu-node` join binary (P8). kiac EMBEDS rendered, pinned manifests
  of macgpu components exactly the way it embeds Traefik/Gateway upstream
  manifests today (pkg/cluster/gateway_assets.go pattern: one //go:embed var per
  file, doc comment with upstream source + version + rationale).
- Until P3, no new repo: mock mode uses upstream squat/generic-device-plugin.

## 4. Working agreements (repo conventions — recon-verified; follow exactly)

- cobra RunE stays a thin shim; ALL orchestration in pkg/cluster (cmd/root.go:1).
  New command files self-register via init() + rootCmd.AddCommand (cmd/node.go
  pattern). Add cmd/gpu.go with `gpuCmd` parent (Use+Short only) and leaves.
- Every user-visible phase wraps in ui.Step(title, fn); details ui.Infof;
  copy-paste follow-ups ui.Hintf; doctor-style lines ui.Check (pkg/ui/ui.go).
- Errors: lowercase fmt.Errorf, %w for causes, exact remedial command inline
  ("install it with: brew install krunkit").
- Addons: optional ones degrade (ui.Infof + continue) after labelLBPrimary,
  gated on cfg.CNI != "none" — cluster.go:418-430 and the k3s twin
  k3s.go:376-386. EVERY addon ships kubeadm and k3s twins.
- Embedded assets: pkg/cluster/assets/gpu/*.yaml + pkg/cluster/gpu_assets.go;
  every image pinned to exact tag (+digest where the pattern does); validated in
  gpu_test.go by the decodeAll + pinned-image-regex pattern (gateway_test.go).
- Config plumbing triple: Config field (cluster.go:106-125) + flag in
  cmd/create.go init() + configfile.go Merge guarded by the EXACT flag-name
  string (KnownFields(true): missing YAML keys = parse failure for users).
- Tests hermetic, table-driven, no mock frameworks: fake backend binaries as
  shell scripts in t.TempDir injected via runtime.Client{Bin: script}
  (verify_test.go:95-136). CI = `make ci` on macos-26, Go 1.25.12; hosted
  runners have NO krunkit and NO GPU — real GPU coverage goes to the
  self-hosted runtime-smoke workflow as a new profile.
- Downloaded artifacts: URL+sha256 tables, ~/.kiac/ cache, digest re-verified on
  every cache hit (kernel.go kernelAssets + downloadVerified — reuse it).
- Comments explain WHY at mechanism level, citing issues (repo standard).

---

## P0 — Validation spike (1-2 weeks, NO kiac code, do first, publish results)

Nobody has published two concurrent krunkit GPU VMs on one Mac. This decides P2+.

Protocol (scripts + results land in test/spike-gpu/, a README with numbers):
1. Install krunkit + vmnet-helper (record exact versions; brew or source).
2. Boot ONE krunkit VM from a Fedora 42 cloud image (cloud-init NoCloud seed for
   SSH key). Verify /dev/dri exists; `vulkaninfo` inside a podman/docker
   container in the guest using a Mesa-25.0.x-era image shows
   "Virtio-GPU Venus (Apple ...)".
3. Baseline: host-native `llama-bench` (llama.cpp Metal) on a pinned model
   (qwen3-4b-q4_k_m.gguf or similar). Record tok/s pp+tg.
4. In-VM: same model, llama.cpp Vulkan build, `-ngl 999`. Record tok/s and the
   %-of-native. Expect 75-80%; if far below, STOP and investigate before P2.
5. TWO VMs concurrently: run llama-bench in both simultaneously. Record: does
   macOS time-slice (both make progress)? aggregate vs solo throughput? any
   crash/starvation?
6. vRAM window: verify the (62GiB - guest RAM) auto-sizing; if krunkit exposes
   no flag, confirm via guest /sys or allocation failure point. Attempt the
   ~10-line krunkit patch exposing shm_size; verify a 8GiB-window VM fails
   allocations past 8GiB while a 32GiB one succeeds.
7. Kubelet probe: install k3s agent OR kubeadm+containerd in the guest; join a
   throwaway kiac cluster manually (token from cp). Confirm Ready node +
   /dev/dri visible to a privileged pod.
Acceptance: README with the five numbers (native, venus solo, venus x2, window
cap behavior, join success), versions table, and every command reproducible.
Publish (blog/gist) — it's a first.

Current execution note (2026-09-04): the available host is an M1 Max MacBook
Pro with 32 GPU cores and 64 GiB unified memory, but had only 4.2 GiB free disk
and no krunkit installation. Do not begin the image-heavy spike until at least
30 GiB is available. The hardware is suitable; storage is the blocker.

## P1 — Mock GPU mode on today's kiac (independent of P0; parallelizable)

Goal: `kiac create cluster --gpu-mock` → charts requesting `kiac.dev/gpu`
schedule on stock apple/container nodes; labels + RuntimeClass appear; works on
kubeadm AND k3s; CI-testable on hosted runners.

Tasks:
1. Config: add `GPUMock bool` to cluster.Config (cluster.go:106-125). Flag
   `--gpu-mock` in cmd/create.go init() (pattern of --observability line ~141).
   configfile.go: extend FileAddons with `GPUMock *bool yaml:"gpuMock"`, Merge
   guarded by changed("gpu-mock"). Zero value = today's behavior.
2. Assets: pkg/cluster/assets/gpu/{namespace,device-plugin,runtimeclass}.yaml.
   device-plugin.yaml = squat/generic-device-plugin 0.2.0, pinned to multi-arch
   digest `sha256:66c8d5c270eb2b721f1064c549b9b7898152a6d2f0163380a5d37dc7636c20ff`
   (verified to contain linux/arm64), `--domain kiac.dev`, device `gpu`, count 1.
   The plugin cannot advertise a count without discovering a path. Use
   `/dev/null` as the harmless mock marker; allocation mounts it at
   `/dev/kiac-mock-gpu` and provides no acceleration.
   Never use an optional missing path: upstream discovery then advertises zero.
3. pkg/cluster/gpu.go: `installGPU(cp string, cfg Config) error` +
   `installGPUK3s` twin, in the installGateway shape (gateway.go:19): ui.Step
   per phase, m.rt.ExecStdin kubectl apply, label nodes (kubectl label via cp —
   labelLBPrimary pattern, lb.go:327) with the P2 contract labels
   (`gpu.api=mock`), then bounded polls for DaemonSet readiness and advertised
   capacity; end with ui.Infof capacity summary + ui.Hintf
   `kubectl get nodes -o custom-columns=...`.
4. Wire into both addon blocks (cluster.go:418-430, k3s.go:376-386), degrade on
   error like observability.
5. Verify: add `gpu.runtime-class`, `gpu.device-plugin`, and `gpu.nodes` checks,
   Skip when the addon is absent (verify.go; recon says append-only IDs, never
   bump SchemaVersion).
6. webui: add gpu fields to /api/meta + createReq (webui.go:138-147, :380-456);
   the CLI flag is the real API since webui shells out to kiac.
7. Tests: gpu_test.go (decodeAll + image-pin regex), configfile_test.go cases,
   verify test against the fake `container` script.
Acceptance: on a stock cluster, `kubectl run g --image=busybox --overrides` with
`kiac.dev/gpu: 1` limits schedules and runs; `kiac verify cluster` passes with
gpu.* = pass; a cluster created WITHOUT the flag shows gpu.* = skip; `make ci`
green.
Demo: helm chart requesting one GPU reconciles on a MacBook with no krunkit.

## P2 — krunkit GPU node backend (the big one; requires P0 pass)

Goal: `kiac create cluster --workers 2 --gpu-workers 1` → mixed cluster: cp +
workers on apple/container, GPU node(s) on krunkit, joined, labeled, tainted;
resume/delete/status/verify/stop/start all see them.

### 2a. Backend abstraction (do first, zero behavior change, own PR)
- Extract a narrow `NodeBackend` in pkg/runtime: RunDetached(RunOpts), Exec,
  ExecStdin, ExecTimeout, WaitReady, Logs, IP, IPv6, List(prefix) ([]Info,
  error), Stop, Start, and Remove. Keep apple/container-only host operations
  (ImagePull/ImageSave, Available, Version, System*, NetworkHasIPv6) on a
  separate `HostRuntime` interface that embeds NodeBackend. Existing Client
  implements both unchanged; krunkit must not grow meaningless System* stubs.
- Manager gains `host HostRuntime` plus a per-node router: `backendFor(node
  string) NodeBackend` keyed by name suffix (-gpu- → krunkit) with the host
  backend as default. Enumeration paths use one `listNodes` method that merges
  all backends. Info gains a `Backend string` field.
- Fix the leaks: webui.go:180 Runtime().IP → Manager method; cmd/doctor.go and
  cmd/version.go keep constructing the container client directly (they check
  the apple/container runtime specifically — fine, note it).
- Tests: every existing test still green with zero behavior change.

### 2b. krunkit backend implementation (pkg/runtime/krunkit.go)
- Process model: krunkit is a foreground VMM. RunDetached = spawn krunkit (+
  vmnet-helper per minikube's documented pairing) detached, write pidfile +
  state JSON under ~/.kiac/gpu-nodes/<node>.json (image path, disk path, args,
  MAC, created). List(prefix) reads that registry and synthesizes Info rows —
  THE fix for the "invisible to container ls" trap (delete would otherwise
  orphan processes; recon risk list).
- Exec transport: krunkit has no exec verb. Use SSH: generate a per-cluster
  ed25519 key under ~/.kiac/gpu-nodes/, inject via cloud-init NoCloud seed ISO
  at first boot; Exec/ExecStdin/ExecTimeout = ssh -o BatchMode against the node
  IP. WaitReady for krunkit = wait for SSH, then the distro probe.
- IP discovery: guest-exec `ip -4 route get 1.1.1.1 ... src` (recon: the ONLY
  portable path — container.go:268 primary path works once Exec works). For
  pre-SSH bootstrap, parse /var/db/dhcpd_leases by the VM's MAC.
- RunOpts translation: parse "2G"-style Memory into MiB, numeric CPUs; ignore
  Kernel (libkrun boots the disk image's kernel); honor DNS by writing
  /etc/resolv.conf via cloud-init (krunkit lacks --dns; contract: replace, max
  3 — dns.go).
- vRAM window: pass the GPU shm window per P0 findings (krunkit flag if our
  patch landed, else document the default and set node gpu.memory label from
  the effective value). NEVER claim a cap we don't set.
- Fake-ability: struct holds Bin fields (krunkit, vmnet-helper, ssh) so tests
  stub them with shell scripts, same as runtime.Client{Bin:...}.

### 2c. GPU node image
- Bootable raw/qcow2 EFI disk image: Fedora 42 base + containerd + kubelet +
  kubeadm (and k3s binary for the k3s path) + udev rule for /dev/dri
  permissions + cloud-init enabled. Built by a new GitHub workflow
  (gpu-image-build.yml; kernel-build.yml is the precedent), published as a
  release asset, pinned URL+sha256 in a new table in pkg/cluster/gpuimage.go
  (reuse kernel.go's downloadVerified + ~/.kiac/images/ cache).
- Note: workload-container Mesa matters (25.0.x era) — the node image only
  needs kernel virtio-gpu/DRM; document both pins in the image README.

### 2d. Cluster integration
- Naming grammar — extend ALL FOUR places together (recon risk): status.go:116
  clusterNameFromNode, cluster.go:763 Clusters(), persist.go:305 orderNodes,
  chaos.go:106 resolveNode (+ error hints :109-122); ALSO webui.go:192 nodeRole
  and :493 clusterNode; verify.go:107 layout counting.
- Create flow: new fields GPUWorkers int, GPUMemory string, GPUImage string in
  Config + flags + configfile (triple). In Create (cluster.go:170-472): after
  workers join, boot GPU VMs via the krunkit backend, WaitReady, apply per-node
  boot prep (ip_forward; senderOffloadFix derived-interface variant — derive
  NIC from default route like ipv6BootPrep does at cluster.go:77, since eth0 is
  vmnet-specific), pinKubeletNodeIP ALWAYS (cluster.go:505 pattern; recon:
  kubelet auto-detect unreliable across vmnet subnets), then kubeadm token
  join with --node-name (cluster.go:295-320 recipe). k3s path: agent env
  K3S_URL/K3S_TOKEN at boot (k3s.go:144); token is generated per create — pass
  it to GPU agents in the same create; for later add-node, read it from the
  server node.
- Edge proxy on GPU nodes: run the same three file installs + service start
  (edgeproxy.go:138-198) over SSH; read the tunnel token from an existing node
  (/etc/kiac/tunnel.token) — never regenerate (recon risk).
- Label + taint via cp kubectl after Ready: contract labels + kiac.dev/gpu
  taint. Device plugin (P1 asset, real mode: mounts /dev/dri, count N) rolls
  out via nodeSelector gpu.present=true.
- Reachability preflight: verify GPU-node-IP:6443→cp and host→GPU-node-IP
  before join (hostReachAPI pattern, reachability.go:23) — vmnet 'default' and
  vmnet-helper subnets differ; if the host can't reach the GPU node IP,
  EXCLUDE it from kiac-lb (label it out; lb.go eligible_nodes honors labels via
  kiac.io/lb-primary precedent) rather than handing out a dead EXTERNAL-IP.
- Resume: krunkit nodes re-launch from the state registry (process spawn, not
  `container start`), rediscover IP, then healWorkerScript + re-pin node-ip
  like any worker (persist.go). Dual-stack: GPU nodes are v4-only initially —
  skip the IPv6 wait paths for them (recon: nodeIPArg would block 45s and
  fail); document "GPU nodes join dual clusters v4-only".
- Delete/cleanupOnFailure: remove krunkit processes + disk images + registry
  entries; a failed create must not leak host processes.
- Doctor: extend cmd/doctor.go with krunkit/vmnet-helper presence+version
  ui.Check lines (gated: only when GPU flags/nodes present). New cmd/gpu.go
  group: `kiac gpu doctor|status` (status = per-node window, labels, device
  plugin health). `bench` and `values` come in P3/P4.
- Verify: `gpu.node.<name>` per-node probe (vulkaninfo-lite: /dev/dri exists,
  device plugin registered); note verify.go:119 services check must know the
  GPU node's init story (Fedora systemd — `systemctl is-active containerd
  kubelet` actually works; k3s GPU agents need the k3s probe).
- Support bundle: gpu-conditional kubectl outputs + per-node device-plugin log
  tail keyed on gpu.* != Skip (support.go:148 pattern); ADD redaction regexes
  for the SSH private key path/content BEFORE collecting anything near it
  (supportRedactions + TestRedactSupportText).
Acceptance: mixed 3-node cluster up in one command on kubeadm AND k3s; pod with
`kiac.dev/gpu: 1` + ramalama image streams tokens with host GPU active in
Activity Monitor (~75-80% of the P0 native baseline via `kiac gpu bench`
prototype script); `kiac resume`, `kiac delete`, `kiac get`, `kiac verify`,
stop/start all handle GPU nodes; failed create leaks no processes; make ci
green (krunkit faked); runtime-smoke gains a gpu profile (self-hosted).

## P3 — macgpu standalone stack + real scheduling polish

- Create macgpu repo: Helm chart wrapping device-plugin config, labeler
  (fold into one DaemonSet), RuntimeClass shim; publish pinned images. kiac
  vendors rendered manifests (gpu_assets.go swaps to macgpu-rendered content,
  same pins). Chart must install on minikube --driver=krunkit unmodified —
  that's the ecosystem acceptance test.
- `kiac gpu bench`: runs the pinned model on (a) host-native llama.cpp/Metal
  if present, (b) a venus pod, prints tok/s side by side. Honesty artifact.
- `kiac gpu values <chart>`: emits override files for vllm-production-stack
  (requestGPUType), kserve (gpu-resource-types annotation), ollama (resources
  block). Table-driven, tested.
Acceptance: chart installs on kiac AND minikube-krunkit; bench table prints.

## P4 — nvidia.com/gpu compat bridge

- macgpu repo: mutating webhook (controller-runtime): namespaces labeled
  `kiac.dev/gpu-compat=nvidia` get nvidia.com/gpu(+mig-*) requests rewritten to
  kiac.dev/gpu + audit annotation; emits Warning event on detectably-CUDA
  images ("needs a Vulkan build — see the swap table"). HAMi-DRA's webhook is
  the pattern precedent. kiac: `kiac gpu compat enable|disable` toggling the
  namespace label + embedded webhook manifests (cert bootstrap: use a
  self-signed CA generated at install like other local-first webhooks; keep it
  simple, document rotation).
Acceptance: unmodified vLLM production-stack chart with ONLY the documented
image swap schedules and runs; the rewrite annotation is present; node
capacity never shows nvidia.com/gpu.

## P5 — DRA driver (first Apple GPU DRA driver)

- macgpu repo: fork/skeleton from kubernetes-sigs/dra-example-driver (CDI
  plumbing + simulated-device mode = Linux-CI-on-kind for free). Publishes one
  ResourceSlice device per GPU node: attributes product/api/unified-memory,
  consumable capacity `memory` = that node's vRAM window. DeviceClass
  `gpu.kiac.dev` with extendedResourceName `kiac.dev/gpu` (1.37 stable) — the
  SAME pod YAML from P1 keeps working, now DRA-backed. requestPolicy: 1Gi steps.
- kiac: install DRA driver instead of device plugin when cluster >= 1.37
  (flag-gated rollout: `--gpu-resource-driver=dra|device-plugin`, default
  device-plugin until soak); wire vRAM window value → capacity.
Acceptance: 8Gi + 24Gi claims admit on a 32Gi-window node, a further 8Gi claim
is rejected at scheduling with a clear event; simulated mode runs in kind CI;
`kubectl get resourceslices` shows the device on a MacBook.

## P6 — APIR pool (gated on P0-P5 stable + patch review status re-check)

- Second GPU image flavor carrying virglrenderer !1590 + libkrun #508 builds
  (crc-org downstream precedent). `--gpu-mode apir` node pools: taint
  kiac.dev/gpu-apir single-tenant, DRA attribute api=apir, docs state: GGML
  _VIRTGPU images only, Venus disabled in these VMs, not multitenant safe.
  `kiac gpu bench` gains the apir column (~95-100% of native llama.cpp).
  Re-verify patch status upstream before building — they may have merged.

## P7 — Standalone release / P8 — multi-Mac

- P7: macgpu chart verified on minikube-krunkit + Colima k3s with no kiac;
  host-endpoint addon (`kiac gpu host-endpoint`): registers native
  llama-server/Ollama/MLX as Service+EndpointSlice, documented as EXTERNAL (no
  pod sandbox) — LLMKube/Model Runner pattern.
- P8: `macgpu-node --join <token>` host binary (build/release per the
  edge-proxy sub-binary precedent: Makefile asset target, reproducible-build
  check in ci, goreleaser second build id — NOTE release.yml:86 verify step
  assumes ONE archive; rewrite it when adding the second artifact). Enrolls
  any Mac's GPU worker into an existing cluster over LAN.

## Risk register (recon-sourced traps; check before each phase)

1. `container ls` is the single inventory: ANY krunkit path missing the
   registry merge silently orphans VMs (P2a fixes; test delete-after-kill).
2. Node-name grammar lives in 4+ places; a missed one silently drops GPU nodes
   from listings (grep for "-worker-" when touching).
3. distroFromNodes (status.go:95) misclassifies k3s clusters if GPU nodes run
   non-k3s images — keep at least the k3s-image check semantics; add a test.
4. WaitReady hardcodes `systemctl is-active containerd`; k3s GPU agents have
   no systemd — route readiness per distro+backend.
5. Dual-stack helpers block on IPv6 for v4-only nodes (nodeIPArg 45s wait) —
   skip for GPU nodes explicitly.
6. senderOffloadFix hardcodes eth0 — derive the NIC; and verify whether
   vmnet-helper even needs it (P0 measurement: large NodePort upload through a
   GPU node).
7. kiac-lb will hand out any Ready node's IP — gate on host reachability.
8. configfile KnownFields(true): ship YAML keys with the flags or user configs
   break.
9. Support bundle: new secrets (SSH keys, tokens) need redaction regexes FIRST.
10. Release pipeline assumes one build/one archive — rework release.yml verify
    + homebrew cask when macgpu-node ships (P8, not before).
11. Mock/device-plugin images must be linux/arm64 + minimal-kernel-safe (no
    eBPF) or they crash-loop on the default Apple kernel.
12. Upstream drift: before P2 and P6, re-check libkrun #765, Mesa blob
    alignment, virglrenderer !1590, libkrun #508 — any merge simplifies work.

Live dependency status (re-checked 2026-09-04): krunkit v1.3.2 is current;
libkrun #765 remains open and non-draft with a dirty merge state; libkrun #508
remains an open draft with a dirty merge state. Treat neither as available.
Re-check the GitLab virglrenderer MR and Mesa alignment work immediately before
P6 because their forge state cannot be inferred from the libkrun PRs.

## Suggested execution order for an agent team

- Agent A: P0 spike (scripts + numbers). Agent B in parallel: P1 mock mode.
- Then P2a (interface extraction, own PR, mergeable independently) → P2b-2d.
- P3 and P4 parallelize after P2. P5 after P3. P6-P8 sequential, gated.
- One PR per numbered phase minimum; P2 splits into 2a/2b/2c/2d PRs.
- Every PR: make ci green, conventions section 4 respected, docs updated
  (README Features + examples/ lab for each user-visible milestone — the
  repo's pattern is a lab .md per feature).

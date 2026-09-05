# Real Apple GPU and inference lab

This lab creates a multi-node Kubernetes cluster with a real Apple GPU worker,
proves the pod sees the Venus Vulkan device, runs a reproducible Metal-versus-
Venus LLM benchmark, and serves TinyLlama through a LoadBalancer.

This is an alpha feature for Apple silicon. It is not GPU emulation: krunkit
maps the Mac's Apple GPU into its Linux VMs as `/dev/dri` through
virtio-gpu/Venus. Only designated `-gpu-N` workers publish Kubernetes GPU
inventory. It also is not CUDA compatibility. Kiac never advertises
`nvidia.com/gpu`, and CUDA, NVML, Metal, and MLX are unavailable inside pods.
The same is true of `nvidia-smi`.

## 1. Install the GPU runtime

Install krunkit from its official tap. On macOS 26 or newer, install
vmnet-helper from Homebrew too:

```sh
brew tap libkrun/krun
brew trust libkrun/krun
brew install krunkit
brew tap nirs/vmnet-helper
brew trust nirs/vmnet-helper
brew install vmnet-helper
kiac gpu doctor
```

On macOS 14 or 15, use the
[vmnet-helper upstream installer and sudoers setup](https://github.com/nirs/vmnet-helper#installation)
instead of Homebrew. If krunkit came from the old `slp/krunkit` or `slp/krun`
tap, follow the
[current driver migration instructions](https://minikube.sigs.k8s.io/docs/drivers/krunkit/)
before reinstalling it.

`kiac gpu doctor` checks macOS/arm64, krunkit 1.3.2+, vmnet-helper 0.13.0+,
SSH, disk-image, and host-network tooling, plus an installed Venus-capable virglrenderer from the
official krunkit dependency stack.

## 2. Create the cluster

The checked-in config selects K3s 1.36, one ordinary worker, one GPU worker,
and the DRA resource driver:

```sh
kiac create cluster --config examples/gpu-cluster.yaml
kubectl get nodes -L kiac.dev/gpu.present,kiac.dev/gpu.api,kiac.dev/gpu.memory
kiac gpu status --name gpu-lab
```

GPU mode uses krunkit for the control plane and all workers so the cluster has
one reliable VM network. Only `kiac-gpu-lab-gpu-1` publishes GPU labels, the
`kiac.dev/gpu=true:NoSchedule` taint, and schedulable GPU inventory; Kiac mounts
the render device only into pods allocated to a GPU worker.
Ordinary Kiac clusters still use the faster apple/container backend.

Use kubeadm instead:

```sh
kiac create cluster --name gpu-ka --distro kubeadm --k8s-version 1.37 \
  --workers 1 --gpu-workers 1
```

The default `--gpu-resource-driver device-plugin` exposes one
`kiac.dev/gpu` extended resource per GPU worker. Experimental
`--gpu-resource-driver dra` on Kubernetes 1.36+ publishes a
`gpu.kiac.dev` DeviceClass and one memory-capacity ResourceSlice per worker.
Both modes accept the ordinary `kiac.dev/gpu: 1` pod resources below.

## 3. Prove Vulkan reaches the Apple GPU

```sh
kubectl apply -f examples/gpu-vulkan.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/kiac-gpu-vulkan --timeout=5m
kubectl logs kiac-gpu-vulkan
```

The test fails unless `vulkaninfo` contains a real Venus device. A successful
run ends with output similar to:

```text
deviceName = Virtio-GPU Venus (Apple M1 Max)
driverName = venus
driverInfo = Mesa 25.0.7
KIAC_REAL_APPLE_GPU_OK
```

The workload image is pinned because newer generic Mesa images can currently
enumerate no Venus device due to an upstream blob-alignment gap. Do not replace
it with a random Vulkan image and assume `/dev/dri` alone is sufficient.

## 4. Measure real model inference

```sh
kiac gpu bench --name gpu-lab
```

The first run downloads a SHA-pinned 638 MiB TinyLlama Q4_K_M model to
`~/.kiac/models`. It runs the same 128-token prompt and 64-token generation
benchmark three times with native Metal, when host `llama-bench` is installed,
and in a Venus pod. The command rejects the pod result unless llama.cpp names
`Virtio-GPU Venus`; use `-o json` for automation.

The two backends are not expected to have equal performance. The comparison is
an observable regression baseline, not a claim that Venus is faster than
native Metal.

## 5. Serve TinyLlama on Kubernetes

```sh
kubectl apply -f examples/gpu-inference.yaml
kubectl rollout status deployment/tinyllama -n kiac-gpu-demo --timeout=10m
IP=$(kubectl get svc tinyllama -n kiac-gpu-demo \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl -fsS "http://${IP}:8080/completion" \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Kubernetes on Apple GPUs is","n_predict":32}'
kubectl logs -n kiac-gpu-demo deploy/tinyllama | grep 'Virtio-GPU Venus'
```

The init container downloads the same revision-pinned model and verifies its
SHA-256 before the server starts. The server image is the exact arm64 digest
validated by this lab.

## 6. Request DRA memory explicitly

In DRA mode, a workload can request GPU-window capacity in 1 GiB steps:

```sh
kubectl apply -f examples/gpu-dra-memory.yaml
kubectl get deviceclass,resourceslices.resource.k8s.io
kubectl describe resourceclaim kiac-gpu-memory-8gi
```

The memory value is a schedulable share of the VM-level Venus window recorded
by krunkit. It is not dedicated physical VRAM or a compute-performance quota.
Capacity is accounted per GPU worker; multiple GPU VMs can collectively
request more than the Mac's unified memory, so size multi-node clusters with
the host limit in mind. The simpler extended-resource form requests the
driver's default available capacity.

## 7. Existing inference charts

Kiac can generate the Kubernetes scheduling portion for common integrations:

```sh
kiac gpu values vllm > values-kiac-vllm.yaml
kiac gpu values ollama > values-kiac-ollama.yaml
kiac gpu values kserve > kserve-kiac-fragment.yaml
```

Read the comments in each generated file. Stock vLLM and NVIDIA-flavored
Ollama/KServe images require CUDA and will not execute on Venus merely because
their resource name schedules. A Venus/Vulkan-capable application image is
still required. For legacy manifests that hard-code an NVIDIA resource name,
`kiac gpu compat enable --name gpu-lab --namespace <namespace>` provides an
opt-in admission rewrite; it does not emulate CUDA or NVML.

## Verify, recover, and clean up

```sh
kiac verify cluster --name gpu-lab
kiac support bundle --name gpu-lab
kiac stop node gpu-1 --name gpu-lab
kiac start node gpu-1 --name gpu-lab

kubectl delete -f examples/gpu-dra-memory.yaml --ignore-not-found
kubectl delete -f examples/gpu-vulkan.yaml --ignore-not-found
kubectl delete -f examples/gpu-inference.yaml --ignore-not-found
kiac delete cluster --name gpu-lab
```

`resume`, node stop/start, status, verify, support bundles, LoadBalancer,
Gateway API, observability, storage, and ordinary cross-node networking use the
same lifecycle paths in GPU clusters. GPU mode currently supports IPv4 only;
kubeadm GPU clusters support kindnet or Cilium (using the host Cilium CLI), but
custom kernel images remain an apple/container-backend feature.

# Mock GPU scheduling lab

This lab exercises kiac's first GPU alpha contract on an ordinary Apple
container cluster. It proves that Kubernetes charts can discover a GPU-labeled
node, request `kiac.dev/gpu`, and use `runtimeClassName: kiac-gpu`.

**This mode does not expose the Apple GPU and does not accelerate workloads.**
It is a scheduling and compatibility harness for the real krunkit/Venus backend
planned next.

## 1. Create an opt-in mock cluster

```bash
kiac create cluster --name gpu-lab --workers 2 --gpu-mock
kubectl config use-context kiac-gpu-lab
```

Without `--gpu-mock`, kiac creates exactly the same cluster as before and runs
no device-plugin Pods.

## 2. Inspect the contract

```bash
kubectl get runtimeclass kiac-gpu -o custom-columns=NAME:.metadata.name,HANDLER:.handler
kubectl get daemonset -n kiac-gpu-system kiac-gpu-device-plugin
kubectl get nodes -l kiac.dev/gpu.present=true \
  -o 'custom-columns=NAME:.metadata.name,GPU:.status.capacity.kiac\.dev/gpu,PRODUCT:.metadata.labels.kiac\.dev/gpu\.product,API:.metadata.labels.kiac\.dev/gpu\.api'
```

Every current node advertises one synthetic `kiac.dev/gpu`. The labels say
`product=Mock`, `count=1`, `memory=0`, and `api=mock`, so automation can reject
mock nodes when it needs real acceleration.

## 3. Schedule a GPU-shaped Pod

```bash
kubectl apply -f examples/gpu-mock.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/kiac-gpu-mock-check --timeout=2m
kubectl logs pod/kiac-gpu-mock-check
kubectl get pod kiac-gpu-mock-check -o wide
```

Expected log:

```text
mock GPU scheduling works; no hardware acceleration is enabled
```

The device plugin maps the node's harmless `/dev/null` character device to
`/dev/kiac-mock-gpu` in this Pod. This confirms allocation happened; it is not a
CUDA, Metal, or Vulkan device.

## 4. Verify and clean up

```bash
kiac verify cluster --name gpu-lab
kubectl delete -f examples/gpu-mock.yaml
kiac delete cluster --name gpu-lab
```

`verify` reports `gpu.runtime-class`, `gpu.device-plugin`, `gpu.nodes`, and one
`gpu.node.<node-name>` check per labeled node. On clusters created without the
flag, the three global GPU checks report `skip`.

## What comes next

The stable target keeps the workload-facing resource and labels while replacing
the marker device with a real `/dev/dri` path from krunkit/Venus GPU nodes. The
validation spike and backend work are tracked in
[`docs/design/gpu-build-plan.md`](../docs/design/gpu-build-plan.md).

# Cilium on kiac: eBPF networking in one command

The stock node kernel (Apple's generic VM kernel) has no VXLAN, no
br_netfilter, and no eBPF JIT, which is why `--cni` is limited to
kindnet out of the box. `--kernel full` swaps in the kiac full kernel,
and with it Cilium becomes a one-liner. This walkthrough creates a
Cilium cluster, checks it, routes traffic through the Gateway API
addon, and measures cross-node throughput with a 20MB fetch.

Time: a few minutes. Everything runs on your Mac.

## 0. Prerequisites

Two host tools on top of apple/container:

```sh
brew install --cask saiyam1814/tap/kiac
brew install cilium-cli
```

kiac drives the official `cilium` CLI to install Cilium, and it checks
for it (and for the `--kernel` flag) before booting any VM, so a
missing prerequisite fails in the first second, not minutes in.

## 1. Create the cluster

```sh
kiac create cluster --name cilium --workers 2 --cni cilium --kernel full --gateway
```

What happens, in order:

- `--kernel full` downloads the published kiac kernel once (a ~15MB
  arm64 boot Image from the `kernel-v6.12.28-full` release,
  sha256-verified on download and on every cache hit, cached under
  `~/.kiac/kernels`). It is Apple's base kernel config plus what CNIs
  need: VXLAN, Geneve, br_netfilter, nf_tables, the eBPF JIT with BTF,
  WireGuard, and kprobes. See `kernel/README.md` for how it is built.
- Every node VM boots on that kernel.
- kiac runs `cilium install --wait` against the new cluster using your
  host's cilium CLI and passes the cluster's `--wait` budget through as
  Cilium's `--wait-duration`, then installs the addons you asked for.

With `--observability` and `--gateway` both added, the whole stack
(Cilium, Prometheus, Grafana, Gateway API, Traefik) came up in 1m37s
on an M-series Mac.

## 2. Check Cilium

The create merged a `kiac-cilium` context into your kubeconfig:

```sh
cilium status --context kiac-cilium
```

Everything should report OK: the operator, one agent per node, and
cluster health. For a deeper (and slower) check, `cilium connectivity
test --context kiac-cilium` runs the upstream end-to-end suite.

## 3. Route traffic with the Gateway API

`--gateway` installed the Gateway API CRDs and Traefik with a default
Gateway that accepts routes from every namespace, and the [HTTPRoute
example](httproute.yaml) attaches to it as-is:

```sh
kubectl apply -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/httproute.yaml

GATEWAY_IP=$(kubectl get svc traefik -n kiac-gateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl -H 'Host: echo.local' http://$GATEWAY_IP/
```

The echo server answers with a JSON dump of the request. Same flags,
same manifest, same behavior as on a kindnet or k3s cluster; the addon
matrix does not care which CNI is underneath.

## 4. Measure cross-node throughput with a 20MB fetch

Cilium's vxlan datapath is not just about features. Tunneled pod
traffic rides packets addressed to node IPs, which take vmnet's fast
path; routed CNIs on the stock kernel push cross-node bulk traffic
through vmnet's slow forwarded path instead, roughly two orders of
magnitude slower for bulk transfers. Measured here: about 285MB/s
pod-to-pod across nodes, and about 1GB/s from the Mac into a pod.

Pin a server to worker-1 that publishes a 20MB file. Save as
`blob.yaml` and `kubectl apply -f blob.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: blob
  labels:
    app: blob
spec:
  replicas: 1
  selector:
    matchLabels:
      app: blob
  template:
    metadata:
      labels:
        app: blob
    spec:
      # Pin to worker-1 so the fetch below is guaranteed cross-node.
      nodeSelector:
        kubernetes.io/hostname: kiac-cilium-worker-1
      containers:
        - name: nginx
          image: docker.io/nginx:1.29-alpine
          # Write a 20MB file into the web root, then run nginx.
          command: ["/bin/sh", "-c"]
          args:
            - dd if=/dev/zero of=/usr/share/nginx/html/blob.bin bs=1M count=20 &&
              exec nginx -g 'daemon off;'
          ports:
            - name: http
              containerPort: 80
          resources:
            requests:
              cpu: 10m
              memory: 32Mi
---
apiVersion: v1
kind: Service
metadata:
  name: blob
  labels:
    app: blob
spec:
  selector:
    app: blob
  ports:
    - name: http
      port: 80
      targetPort: http
```

Fetch it from a pod pinned to worker-2 and let curl report the speed:

```sh
kubectl run fetch --rm -it --restart=Never \
  --image=docker.io/curlimages/curl:8.11.1 \
  --overrides='{"spec":{"nodeName":"kiac-cilium-worker-2"}}' \
  -- -sS -o /dev/null -w 'speed: %{speed_download} bytes/sec\n' http://blob/blob.bin
```

The printed speed is bytes per second; expect a few hundred MB/s, and
the 20MB transfer itself finishes in a fraction of a second. Run the
same fetch on a stock-kernel kindnet cluster and the transfer takes
several seconds, which is vmnet's forwarded path at work, not the CNI
doing anything wrong.

## 5. Clean up

```sh
kubectl delete -f blob.yaml
kiac delete cluster --name cilium
```

## What this shows

- `--cni cilium --kernel full` is the entire Cilium setup: no kernel
  builds on your laptop, no manual CNI install, prerequisites checked
  before a single VM boots.
- The full kernel is pinned and verified: exact release asset, exact
  sha256, cached once.
- Addons are CNI-agnostic: `--gateway` and `--observability` behave
  the same on Cilium, kindnet, and k3s clusters.
- The overlay is a real performance choice on vmnet, not overhead:
  cross-node bulk traffic is dramatically faster tunneled than routed.

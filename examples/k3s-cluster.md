# k3s clusters: the lighter distro

`--distro k3s` swaps the node payload: instead of kindest/node with
kubeadm and systemd, every VM runs the rancher/k3s image with k3s as
PID 1. Same VM-per-node layout, same node names, same kiac commands.
This quickstart creates one, shows what is bundled, and runs the
storage and LoadBalancer examples on it unchanged.

## 1. Create

```sh
kiac create cluster --distro k3s --workers 1
```

A 2-node k3s cluster is ready in roughly 22 to 54 seconds across runs,
and the whole thing costs about 3.7GB of host memory (the control-plane
VM gets 4G by default, workers 2G; tune with `--cp-memory` and
`--memory`).

`--k8s-version` works the same as on the kubeadm path: minors 1.32
through 1.36, each pinned to an exact rancher/k3s image digest, 1.36
by default. `kubectl` talks to it through the merged `kiac-dev`
context as usual.

## 2. What is bundled, and what kiac changes

k3s brings its own batteries, and kiac keeps most of them:

- **sqlite datastore**, not etcd: one file, less memory, fine for a
  laptop cluster.
- **local-path** is the default StorageClass, from k3s itself.
- **servicelb** (klipper) backs `type: LoadBalancer` with node IPs
  natively, so kiac-lb is not installed on this distro.
- **metrics-server** is bundled; `kubectl top nodes` works after the
  first scrape (~60s).

Two deliberate changes:

- The bundled **Traefik ingress is disabled**: it would fight the
  `--gateway` addon's Traefik for ports 80 and 443.
- The bundled **flannel is disabled** and kiac applies kindnet
  instead. Flannel wires pods onto a bridge, and on a kernel without
  br_netfilter, same-node pod-to-Service return traffic bypasses the
  iptables un-DNAT and times out. kindnet's routed veth topology
  avoids the bridge entirely. The upstream CNI plugin binaries kindnet
  needs (containernetworking v1.7.1) are downloaded once,
  sha256-verified, and cached under `~/.kiac`.

Because the distro carries its own addons, the `--no-metrics`,
`--no-storage`, and `--no-lb` flags map onto k3s `--disable` switches
rather than skipping kiac installs. `--cni` does not apply here; it is
a kubeadm-path flag and kiac says so if you pass it.

## 3. PVC demo: the StatefulSet example, unchanged

The [StatefulSet example](statefulset.yaml) targets the default
StorageClass, which on k3s is the bundled local-path:

```sh
kubectl apply -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/statefulset.yaml
kubectl get pvc            # Bound once each pod schedules
kubectl exec notes-0 -- sh -c 'echo hello > /data/note.txt'
kubectl delete pod notes-0
kubectl wait --for=condition=Ready pod/notes-0 --timeout=60s
kubectl exec notes-0 -- cat /data/note.txt   # still says hello
```

## 4. LoadBalancer demo: the nginx example, unchanged

```sh
kubectl apply -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/loadbalancer.yaml
kubectl get svc web        # wait for EXTERNAL-IP
curl http://$(kubectl get svc web -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
```

One k3s-specific detail: servicelb advertises every node's IP, so
`kubectl get svc web` may list more than one EXTERNAL-IP as the
per-node pods come up. Any of them answers from your Mac; for bulk
transfers the IP of the node actually running the pod is the fast one,
since traffic entering another node is NATed across vmnet's slower
forwarding path.

`--observability` and `--gateway` work on k3s with the same flags and
the same defaults as everywhere else (Grafana on :3000, GatewayClass
`traefik`, Gateway `kiac` in `kiac-gateway`), so the
[HTTPRoute](httproute.yaml) and
[observability](observability-scrape.yaml) examples apply unchanged
too.

## 5. Clean up

```sh
kubectl delete -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/loadbalancer.yaml
kubectl delete -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/statefulset.yaml
kubectl delete pvc -l app=notes
kiac delete cluster --name dev
```

## Choosing between the distros

kubeadm (the default) is the closest match to a production cluster:
real etcd, real kubeadm phases, systemd in the VM, and the `--cni`
flag including Cilium. k3s trades that fidelity for speed and
footprint, and is the right pick when the cluster is a means to an
end: testing a chart, a controller, or an operator that does not care
which distro is underneath.

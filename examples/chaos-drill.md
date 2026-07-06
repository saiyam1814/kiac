# Chaos drill: kill a node, watch Kubernetes recover

Every kiac node is its own lightweight VM, so `kiac stop node` is real
node failure: the kubelet goes silent, the node turns `NotReady`, pods
get evicted and rescheduled. Not a container pause, not a cordon. This
drill walks the full loop: spread an app across nodes, kill one, watch
the recovery, bring the node back.

Time: about 5 minutes. Everything runs on your Mac.

## 0. Create a cluster with room to fail

```sh
kiac create cluster --workers 3
```

Three workers so that when one dies there is somewhere for pods to go.
Node names in `kubectl get nodes` are `kiac-dev-control-plane` and
`kiac-dev-worker-1` through `kiac-dev-worker-3`; kiac commands accept
the short names (`worker-1`).

## 1. Deploy a 3-replica app spread across nodes

Save as `drill.yaml` and `kubectl apply -f drill.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: drill
  labels:
    app: drill
spec:
  replicas: 3
  selector:
    matchLabels:
      app: drill
  template:
    metadata:
      labels:
        app: drill
    spec:
      # Soft anti-affinity: prefer one replica per node, but still
      # schedule if fewer nodes are available. Hard anti-affinity
      # (requiredDuringScheduling...) would leave the evicted replica
      # Pending after the node dies, which is its own useful lesson.
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                topologyKey: kubernetes.io/hostname
                labelSelector:
                  matchLabels:
                    app: drill
      # Fail fast for the drill: by default pods tolerate a NotReady
      # node for 300s before eviction. 15s keeps the drill snappy.
      tolerations:
        - key: node.kubernetes.io/not-ready
          operator: Exists
          effect: NoExecute
          tolerationSeconds: 15
        - key: node.kubernetes.io/unreachable
          operator: Exists
          effect: NoExecute
          tolerationSeconds: 15
      containers:
        - name: nginx
          image: docker.io/nginx:1.29-alpine
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
```

Confirm the spread, one replica per worker:

```sh
kubectl get pods -l app=drill -o wide
```

## 2. Kill a node

Pick a worker that runs a replica (say worker-1) and stop its VM:

```sh
kiac stop node worker-1 --name dev
```

`--name dev` is the default cluster name; drop it if that is what you
used. Stopping the control plane works too, but then the API server
(and kubectl) is down until you start it again, so for this drill stick
to a worker.

## 3. Watch the failure play out

```sh
kubectl get nodes -w
```

After roughly 40 to 60 seconds the node controller stops hearing from
the kubelet and `kiac-dev-worker-1` flips to `NotReady`. In a second
terminal:

```sh
kubectl get pods -l app=drill -o wide -w
```

Once the node is `NotReady`, the 15s toleration above runs out, the
replica on worker-1 goes `Terminating`, and the Deployment schedules a
replacement on a surviving node. You are back to 3 running replicas
without touching anything.

Note what else happened: kiac-lb hands out node VM IPs to LoadBalancer
Services, so any Service that was assigned worker-1's address goes
dark with the VM. That is honest failure too, and it heals itself:
once the node flips to `NotReady`, its IP stops being eligible and
kiac-lb re-points the Service at a surviving node on its next pass (it
runs every few seconds). From the host side, `kiac get nodes --name
dev` shows the stopped VM, and `kiac get clusters` drops to
`3/4 running`.

## 4. Bring the node back

```sh
kiac start node worker-1 --name dev
kubectl get nodes -w
```

The VM boots, the kubelet reconnects, and the node returns to `Ready`.
VMs can come back with a different IP; the kubelet re-registers with
the new address, and kiac-lb notices that any LoadBalancer ingress IP
still pinned to the old address is no longer an eligible node IP and
re-points it on its next pass. Nothing to resync by hand. `kiac start
node` is idempotent: running it against a node that is already up is a
no-op.

One known limitation to be honest about: after a stop/start of a
single node, TCP from your Mac to that VM's new IP can be dropped by a
vmnet issue in apple/container 1.0. In-cluster traffic (what this
drill exercises) is unaffected, and a full host restart followed by
`kiac resume` does not hit it; see [resume-drill.md](resume-drill.md).

The rescheduled pods stay where they landed; Kubernetes does not
rebalance on its own. Delete one (`kubectl delete pod <name>`) and the
anti-affinity preference pulls the replacement onto the freshly
returned, empty worker.

## 5. Clean up

```sh
kubectl delete -f drill.yaml
```

or delete the whole cluster:

```sh
kiac delete cluster --name dev
```

## What this shows

- Node failure and recovery are first-class, scriptable operations:
  `kiac stop node` / `kiac start node`.
- Default eviction is slow on purpose (300s); tolerations put the
  trade-off in your hands per workload.
- Soft anti-affinity keeps replicas spread when capacity allows and
  keeps the app running when it does not.
- The same drill is one click per node in `kiac ui`, which has
  stop/start buttons on every node.

# Reboot drill: a cluster that survives your Mac restarting

Every kiac node is a VM under apple/container, and every VM dies with
the container system service: a host reboot, or `container system
stop`, halts them all. The disks survive, but the VMs come back on
fresh vmnet IPs, and a Kubernetes control plane pins its address in
certificates, kubeconfigs, and ConfigMaps. `kiac resume` boots the VMs
and heals all of that. This drill proves it end to end.

Time: about 5 minutes, most of it watching. No actual reboot required
(though a real one works exactly the same way).

## 0. Create a cluster and put something real on it

```sh
kiac create cluster --workers 2
kubectl apply -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/loadbalancer.yaml
kubectl get svc web        # wait for EXTERNAL-IP
curl http://$(kubectl get svc web -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
```

You have 3 VMs, a Deployment, and a LoadBalancer Service answering on
a real IP from your Mac. That is the state we want back.

## 1. Kill everything

```sh
container system stop
```

This is what a host reboot does to kiac: the container system service
goes down and takes every VM with it. If you prefer the full-fat
version of this drill, actually restart your Mac instead; the rest of
the steps are identical.

Bring the service itself back so `kiac get clusters` can list the
damage (`kiac resume` would start it on its own; plain listing does
not):

```sh
container system start
```

## 2. Look at the damage

```sh
kiac get clusters
```

```
NAME  STATUS
dev   0/3 stopped
```

(`-o wide` adds the Kubernetes version and creation time, `-o json`
gives scripts the per-node detail.)

`0/3 stopped` is the signal that the VMs are gone, not just idle.
kubectl hangs or refuses connections; the API server is simply not
running. The rootfs of every node is still on disk under
`~/Library/Application Support/com.apple.container/`.

## 3. Resume

```sh
kiac resume cluster --name dev
```

Watch the steps. resume boots the stopped VMs, then compares the
control plane's new vmnet IP against the one recorded in its
admin.conf. When they differ (they almost always do), it heals every
place the old address is pinned:

- the API server's serving certificate (regenerated with the new IP in
  its SANs),
- the kubeconfigs inside the control plane,
- each worker's kubelet, re-pointed at the new control-plane address,
- the kube-proxy and cluster-info ConfigMaps,
- your host kubeconfig's `kiac-dev` context.

Then it waits for the API server and for every node to come back
Ready. A 3-node cluster resumes in well under a minute; the drill
behind this doc measured 46 seconds, and that run survived vmnet
handing out an entirely different subnet after the restart.

resume is idempotent. Running it against a healthy cluster is a no-op,
and re-running it after a partial failure just picks up where it left
off. `--wait` (default 5m) bounds the whole thing.

## 4. Verify the workload came back

```sh
kubectl get nodes
kubectl get pods -l app=web
kubectl get svc web
```

Nodes Ready, pods Running. The Service is the interesting one: its old
EXTERNAL-IP pointed at a node address that no longer exists. kiac-lb,
the LoadBalancer controller running inside the control-plane VM,
notices the stale ingress IP on its next pass (it runs every few
seconds) and re-points the Service at a live node IP. Curl the fresh
address:

```sh
curl http://$(kubectl get svc web -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
```

The nginx welcome page, same as before the "reboot".

## 5. Clean up

```sh
kubectl delete -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/loadbalancer.yaml
```

or delete the whole cluster:

```sh
kiac delete cluster --name dev
```

## Notes

- The healing exists because vmnet assigns a fresh IP on every VM
  boot; there is no static-address option in apple/container 1.0 to
  avoid it. `docs/design/persistent-clusters.md` records the full
  investigation.
- This drill (whole system down, then `kiac resume`) is the clean
  restart path. Stopping and starting a single node VM with `kiac
  stop node` / `kiac start node` is a different scenario with a known
  limitation: after a single-node restart, TCP from the Mac to that
  VM's new IP can be dropped by a vmnet issue in apple/container 1.0,
  while in-cluster traffic works fine. A full reboot plus resume does
  not hit it. See [chaos-drill.md](chaos-drill.md).

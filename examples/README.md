# kiac examples

Small, self-contained examples you can apply straight from the repo.
Each file starts with a comment block saying what it demonstrates and
the exact commands to run it.

Prerequisite for all of them: a kiac cluster.

```sh
brew install --cask saiyam1814/tap/kiac
kiac create cluster --workers 2
```

The manifests apply directly from GitHub, no clone needed:

```sh
kubectl apply -f https://raw.githubusercontent.com/saiyam1814/kiac/main/examples/loadbalancer.yaml
```

## Manifests

| Example | What it shows |
| --- | --- |
| [loadbalancer.yaml](loadbalancer.yaml) | nginx behind a `type: LoadBalancer` Service; curl a real EXTERNAL-IP from your Mac, no tunnels. |
| [statefulset.yaml](statefulset.yaml) | StatefulSet with `volumeClaimTemplates` on the bundled local-path default StorageClass; PVCs bind, data survives pod restarts. |
| [httproute.yaml](httproute.yaml) | Hostname routing with Gateway API: an echo app plus an HTTPRoute attached to the default Gateway from `--gateway`; needs a cluster created with `--gateway`. |
| [observability-scrape.yaml](observability-scrape.yaml) | Two pod annotations get your app's metrics into the bundled Prometheus and Grafana; needs a cluster created with `--observability`. |

## Guides and config files

| Example | What it shows |
| --- | --- |
| [cilium-cluster.md](cilium-cluster.md) | Cilium as the CNI in one command with `--kernel full`: prereqs, `cilium status`, Gateway API routing, and a cross-node throughput demo with a 20MB fetch. |
| [k3s-cluster.md](k3s-cluster.md) | k3s as the node distro: what k3s bundles, what kiac changes, the PVC and LoadBalancer examples running on it unchanged, and the footprint. |
| [observability-lab.md](observability-lab.md) | Step-by-step Grafana/Prometheus lab: create `--observability`, scrape an annotated app, query Prometheus, and optionally pair it with Gateway API. |
| [gateway-api-lab.md](gateway-api-lab.md) | Step-by-step Gateway API lab: inspect GatewayClass and Gateway, attach HTTPRoutes, test hostname/path matching, and troubleshoot route status. |
| [chaos-drill.md](chaos-drill.md) | Step-by-step node-failure drill: spread an app across workers, `kiac stop node`, watch eviction and reschedule, `kiac start node`, watch the rejoin. |
| [resume-drill.md](resume-drill.md) | Reboot-survival drill: stop the container system, see `0/3 stopped` in `kiac get clusters`, bring it all back with `kiac resume`, curl the app again. |
| [k8gb-lab.md](k8gb-lab.md) | Real DNS-based failover across two kiac clusters: lightweight edge DNS, delegated regional CoreDNS, HTTP traffic validation, failover, and failback. |
| [cluster.yaml](cluster.yaml) | Minimal `--config` file for `kiac create cluster`. |
| [cluster-full.yaml](cluster-full.yaml) | `--config` file with every knob set and commented, all addons on including observability and gateway. |

## Notes

- All images are pinned tags that exist for linux/arm64 and are small
  enough for a laptop cluster.
- Flags set on the command line override values from a `--config` file.
- Everything here assumes the default addons (metrics, storage,
  LoadBalancer) are on; if you created the cluster with `--no-lb` or
  `--no-storage`, the LoadBalancer and StatefulSet examples will not
  have their dependencies.

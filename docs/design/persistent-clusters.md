# Persistent clusters (`kiac resume`)

Status: implemented and live-validated for kubeadm and k3s. This doc
records what exists in apple/container today, why a cluster does not
survive a host reboot, and exactly how each resume path heals it.

Problem: every kiac node VM dies with the `container` system service.
After a Mac reboot (or `container system stop`), `kiac get clusters`
shows `0/N stopped`. The VMs' disks survive; the cluster does not come
back on its own until `kiac resume cluster` starts and repairs it.

## Findings (verified 2026-07-06 on this machine)

### `container machine` exists today, and is not the answer yet

`container` CLI 1.0.0 (build `ee848e3`, released 2026-06-09, the WWDC26
release) ships the `machine` subcommand announced as "long-lived Linux
environments": `create/run/set/set-default/stop/delete/list/inspect/logs`,
with `--cpus/--memory/--home-mount/--no-boot` and a default-machine
concept (`container system property list` shows `[machine] cpus = 5,
memory = 32gb, homeMount = rw`). Verified live with a throwaway
`kiac-spike-m1` machine (alpine:3.22, deleted after):

- The rootfs IS persistent: a file written before `container machine
  stop` was still there after the next boot.
- PID 1 is the image distro's `/sbin/init` (openrc gettys on alpine),
  NOT the OCI entrypoint. kindest/node's `/usr/local/bin/entrypoint`,
  which does all the cgroup/containerd/IP fixups before exec'ing
  systemd, would never run inside a machine. Nodes cannot be machines
  without a boot-command hook Apple has not shipped.
- The IP changes on EVERY boot: one machine went `.82 -> .83 -> .84 ->
  .85` across create/run/stop cycles. Machines share the containers'
  192.168.64.0/24 vmnet allocator, so `container machine` does not
  solve the address problem either.
- Machines live outside `container ls` (own `machine-apiserver` plugin
  state); `container machine run` also mangled some multi-arg commands
  and dropped early stdout in testing, so it is not exec plumbing kiac
  could build on today.

Verdict: `machine` is a WSL-style dev box. Re-platforming nodes onto it
buys persistence kiac already has (see next section) and loses the
entrypoint. Revisit when Apple ships a boot-command/entrypoint option or
stable addressing; nothing in the 1.0.0 release notes or help output
offers `--restart`, autostart, or static IPs anywhere (`container run
--help` confirms: only `-d, --detach`).

### Stopped containers already persist everything except the IP

- Every container survives on disk at `~/Library/Application
  Support/com.apple.container/containers/<name>/` as `config.json` plus
  `rootfs.ext4`/`initfs.ext4` disk images. `container system
  stop`/host reboot leaves them in `stopped` state with data intact;
  this is the same persistence `kiac stop node`/`kiac start node`
  (chaos.go) already relies on, which shipped E2E-verified.
- `config.json` stores NO address, only `networks: [{network:
  "default", options: {hostname: <name>}}]`. vmnet hands out the next
  free IP at each boot. New boot means new IP, for containers and
  machines alike.

So resurrection today is: `container start` every node, then heal every
place the old IPs are baked in. That is `kiac resume`.

## The kubeadm control-plane IP problem, exactly

`kiac create` runs `kubeadm init` with flags and no
`--control-plane-endpoint`, so the control-plane VM's IP is embedded in:

1. the apiserver serving cert SANs (advertise address),
2. `/etc/kubernetes/{admin,super-admin,kubelet,scheduler,controller-manager}.conf`
   server URLs on the control plane,
3. the static pod manifests (`etcd.yaml` listen/advertise URLs,
   `kube-apiserver.yaml --advertise-address`),
4. every worker's `/etc/kubernetes/kubelet.conf` server URL,
5. the `kube-proxy` ConfigMap kubeconfig (its pods crash-loop against
   the old IP after a reboot) and `kube-public/cluster-info` (read by
   future `kubeadm join` discovery),
6. the host kubeconfig contexts kiac wrote.

Two things heal themselves and need no action:

- Node addresses: kiac's `kubeadm join` sets no `--node-ip`, so each
  kubelet re-detects its address every boot and re-registers. This is
  the mechanism the shipped `kiac start node` already banks on.
- etcd data: the member's stale peer URL in the data dir is ignored on
  restart of a single-member etcd (initial-* flags are only read on
  first boot), and the apiserver dials etcd at `https://127.0.0.1:2379`,
  which is in the etcd cert SANs regardless of node IP.

### What the kindest/node entrypoint fixes at boot, and the trap in it

apple/container runs the image entrypoint as the VM init process
(`config.json initProcess.executable`), so kind's
`/usr/local/bin/entrypoint` runs on every `container start`. Verified
against kind's `images/base/files/usr/local/bin/entrypoint` (main, and
unchanged in this area since kind v0.9): it records the node's IP in
`/kind/old-ipv4` and, when it differs at the next boot, seds the OLD own
IP to the new one across the four static pod manifests,
`kubelet/scheduler/controller-manager.conf`, `/kind/kubeadm.conf`, and
`/var/lib/kubelet/kubeadm-flags.env`, then calls `fix_certificate` to
regenerate the apiserver cert. It never touches `admin.conf` or
`super-admin.conf`, and a worker's copy of the CONTROL PLANE's IP is not
its own IP, so items 4-6 above are never fixed by the entrypoint.

The trap: the entrypoint runs under `set -o errexit`, and
`fix_certificate` is hard-coded as

    rm -f /etc/kubernetes/pki/apiserver.{crt,key}
    kubeadm init phase certs apiserver --config /kind/kubeadm.conf

`/kind/kubeadm.conf` is written by kind's own provisioning; kiac's
flag-driven `kubeadm init` never creates it. So the FIRST control-plane
boot after an IP change deletes the apiserver certs, fails the kubeadm
call, and kills the entrypoint before it can exec systemd: the VM dies.
The SECOND boot survives, because the now-missing `apiserver.crt` makes
`fix_certificate` return early, leaving a booted node whose apiserver
crash-loops on the missing cert. Resume is built around this exact
sequence.

## The kubeadm resume flow

1. Preflight, then `container start` every stopped node and wait for
   containerd in parallel. `bootAndWait` watches for the VM dying
   mid-boot and starts it once more; the death is expected exactly once
   on the control plane after an IP change (trap above). Re-apply
   `net.ipv4.ip_forward=1` on every node (runtime state, reset by the
   reboot).
2. Read the old control-plane IP out of the control plane's own
   `admin.conf`: it is the one file the entrypoint never rewrites, which
   makes it the durable record of the pre-reboot address.
3. If the IP changed, run `healControlPlaneScript` on the control plane:
   sed old->new across all confs and manifests (idempotent; no-op where
   the entrypoint already fixed things), including `admin.conf` and
   `super-admin.conf`; regenerate the apiserver cert iff the manifest
   was still stale or the cert file is gone, via `kubeadm init phase
   certs apiserver --apiserver-advertise-address <new>` (flag-driven,
   deliberately not `--config /kind/kubeadm.conf`; kubeadm's default
   SANs match the original init); restart the kubelet so it re-reads
   everything; `crictl rm -f` the apiserver container to skip
   accumulated crash-loop backoff; restart `kiac-lb.service` if
   installed to clear any failed-unit backoff (it reads `admin.conf`
   through kubectl on every pass, so the file heal is what it needed).
4. On every worker, re-point `/etc/kubernetes/kubelet.conf` at the new
   control-plane IP and restart the kubelet. The sed matches ANY
   previous address, not one known old IP, so a resume that died halfway
   through an earlier attempt stays repairable.
5. Wait for `/readyz` through the healed `admin.conf`.
6. Rewrite the `kube-proxy` and `cluster-info` ConfigMaps to the new IP
   and `rollout restart` the kube-proxy daemonset (grep-guarded no-op
   when current; retried while the apiserver settles).
7. Wait for nodes Ready (non-fatal: `--cni none` clusters never report
   Ready). LoadBalancer IPs need no step: once `admin.conf` is healed,
   kiac-lb's next pass re-points any ingress IP pinned to a dead
   address.
8. Merge the healed kubeconfig into the host `~/.kube/config` with the
   new server IP (reuses `mergeKubeconfig`).

Every step is idempotent, so `kiac resume` re-runs safely after partial
failures and also works when the IPs happen to come back unchanged (it
degenerates to start + wait + no-op heals).

## The k3s resume flow

k3s does not have kubeadm's static-pod and certificate rewrite problem,
but every agent VM persists its original server address in `K3S_URL`.
After vmnet changes the server IP, the agents keep retrying a dead
endpoint. Rebuilding the VMs would throw away exactly the state resume
is meant to preserve, and adding a proxy daemon would add steady-state
cost to every cluster.

The lightweight path in `pkg/cluster/k3s_resume.go` is:

1. Start the k3s server and wait for its API.
2. Start each agent and atomically install a root-owned
   `/usr/local/bin/k3s` launcher plus `/etc/kiac/k3s-server-url`. The
   launcher only overrides `K3S_URL` for the `agent` command, then execs
   the image's existing `/bin/k3s`. It never reads or rewrites
   `K3S_TOKEN`, and creates no resident process.
3. Compare the live PID 1 environment with the current server URL. Only
   an agent still running against an old URL is stopped and started
   once. This also migrates clusters created before the launcher existed.
4. Wait for every Kubernetes Node to be Ready **and** to report the
   current VM IPv4 address. A stale Node can remain Ready for about a
   minute after a fast reboot, so readiness alone is not a sufficient
   convergence check.
5. If any VM restarted, roll kindnet and the optional node-exporter
   DaemonSet. Their host-network Pod objects otherwise retain stale
   PodIP metadata even after the Node address changes.
6. Rewrite the restricted edge-proxy kubeconfig to the current server,
   restart only stale helpers, wait for their iptables hooks, and merge
   the current k3s admin kubeconfig on the host.

The launcher is installed during new cluster creation too, so normal
boots take one file read and one `exec`; there is no Envoy, sidecar,
additional image, or steady-state memory overhead.

## Validation

The kubeadm path has been exercised repeatedly on real multi-node
clusters, including a complete vmnet subnet change, workload and
LoadBalancer recovery, and idempotent second resumes. The k3s path has
been exercised across repeated full outages with new addresses, a
pre-launcher cluster migration, exact 1 MiB cross-node NodePort uploads,
LoadBalancer traffic, metrics recovery, edge-proxy RBAC and rules, and
matching Node and host-network Pod addresses.

`test/e2e/run.sh k3s` keeps the release-level contract executable on a
trusted Apple runtime host: one server plus three agents, Gateway API,
observability, a complete stop/resume cycle, address convergence, and
post-resume traffic and verification. The normal pull-request workflow
stays on free hosted CI and does not require Apple virtualization.

`container machine` remains worth revisiting only if Apple adds a stable
boot-command/entrypoint hook or stable addressing. Today it loses the
node image entrypoint and changes address on every boot, so it does not
remove either healing problem.

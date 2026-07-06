# Persistent clusters (`kiac resume`)

Status: healing engine landed (`pkg/cluster/persist.go`,
`Manager.Resume`), CLI wiring and on-cluster E2E validation pending.
This doc records what exists in apple/container today, why a cluster
does not survive a host reboot, and exactly how resume heals it.

Problem: every kiac node VM dies with the `container` system service.
After a Mac reboot (or `container system stop`), `kiac get clusters`
shows `0/N stopped`. The VMs' disks survive; the cluster does not come
back on its own, and until now kiac had no way to bring it back.

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

## The control-plane IP problem, exactly

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

## The resume flow (implemented in `Manager.Resume`)

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

## CLI wiring left to do (cmd/, not part of this change)

```go
// cmd/resume.go
var (
	resumeName string
	resumeWait time.Duration
)

var resumeCmd = &cobra.Command{
	Use:   "resume cluster",
	Short: "Boot a stopped cluster's VMs and heal it after a host reboot",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] != "cluster" {
			return fmt.Errorf("unknown resource %q (supported: cluster)", args[0])
		}
		ui.Banner(Version)
		return cluster.NewManager().Resume(resumeName, resumeWait)
	},
}

func init() {
	resumeCmd.Flags().StringVar(&resumeName, "name", "dev", "cluster name")
	resumeCmd.Flags().DurationVar(&resumeWait, "wait", 5*time.Minute, "how long to wait for boot, API server, and node readiness")
	rootCmd.AddCommand(resumeCmd)
}
```

`kiac get clusters` already prints `0/N stopped`; its hint text should
point at `kiac resume cluster --name <name>`.

## Validated vs. pending validation

Validated in this spike: machine semantics (live, versioned), on-disk
persistence and the absence of stored IPs, new-IP-per-boot, the
entrypoint fixup and its `/kind/kubeadm.conf` trap (source-verified),
and the worker half of the story indirectly through the shipped `kiac
stop/start node` chaos path. NOT yet validated end to end: a full
reboot-and-resume cycle on a real cluster, because this workspace could
not create or stop clusters. Run the E2E checklist below before wiring
the command into a release; the first item to confirm on a real node is
that `/kind/old-ipv4` exists and matches the node IP (it is written only
if `getent ahostsv4 $(hostname)` resolves in the guest; if it does not,
the entrypoint fixup never fires and resume's own seds and cert regen
carry the whole heal, which the script is written to do).

## E2E validation checklist

On a machine with container 1.0.0, from repo root, using a THROWAWAY
cluster name:

1. `go build -o dist/kiac .` after wiring cmd/resume.go.
2. `dist/kiac create cluster --name resume-e2e --workers 2`
3. `kubectl --context kiac-resume-e2e get nodes` -> 3 Ready.
4. Deploy a canary: `kubectl create deploy web --image=nginx --replicas=2 && kubectl expose deploy web --port 80 --type LoadBalancer`; note the EXTERNAL-IP and `curl` it.
5. `container exec kiac-resume-e2e-control-plane cat /kind/old-ipv4` -> must equal the control plane's current IP (entrypoint fixup armed).
6. Reboot the Mac (or `container system stop && container system start`, which kills all VMs the same way).
7. `dist/kiac get clusters` -> `resume-e2e 0/3 stopped`.
8. `dist/kiac resume cluster --name resume-e2e` -> expect "control-plane IP changed a.b.c.d -> w.x.y.z" and exactly one "died during boot ... booting it again" for the control plane.
9. `kubectl --context kiac-resume-e2e get nodes` -> 3 Ready within the wait timeout; `kubectl -n kube-system get pods` -> kube-proxy pods Running, not CrashLoopBackOff; apiserver TLS validates against the new IP (kubectl working is the proof).
10. `kubectl get svc web` -> EXTERNAL-IP re-pointed by kiac-lb to a live node IP; `curl` it.
11. Idempotency: run `dist/kiac resume cluster --name resume-e2e` again -> succeeds with every heal a no-op.
12. Double-reboot: repeat steps 6-10 once more (exercises the any-old-IP worker sed and the second cert regen).
13. `dist/kiac delete cluster --name resume-e2e`.

## Effort estimates

- cmd/resume.go wiring + `kiac get clusters` hint + README/website blurb: ~0.5 day including review.
- E2E checklist on a real cluster, both reboot variants: ~half a day.
- Optional create-time hardening (separate epic): write a minimal
  `/kind/kubeadm.conf` during create, or init with
  `--control-plane-endpoint` on a stable name once inter-VM DNS is
  reliable (`container system dns` domains). Either defuses the
  first-boot cert trap at the source and shrinks resume to
  start+sysctl+kubeconfig; ~1-2 days with regression E2E.
- `container machine`-based nodes: blocked on Apple shipping an
  entrypoint/boot-command hook and stable addressing; re-evaluate each
  apple/container release.

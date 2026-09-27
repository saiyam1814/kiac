package cluster

// Kube-proxy exits when a Node's IP changes in Kubernetes 1.37. K3s embeds
// kube-proxy, so that exits the entire K3s process. Restart inside the same
// VM: restarting the VM itself would acquire another address from vmnet.
// K3s arranges for its containerd child to die with it; pod shims survive
// and are reattached by the next containerd, as on a systemd K3s restart.
const k3sSupervisor = `mkdir -p /var/run
pid=
stopping=
trap 'stopping=1; [ -z "$pid" ] || kill -TERM "$pid" 2>/dev/null || true' TERM INT
trap 'rm -f /var/run/kiac-k3s.pid' EXIT
while [ -z "$stopping" ]; do
  k3s "$@" &
  pid=$!
  printf '%s\n' "$pid" > /var/run/kiac-k3s.pid
  [ -z "$stopping" ] || kill -TERM "$pid" 2>/dev/null || true
  wait "$pid"
  status=$?
  if [ -n "$stopping" ]; then
    wait "$pid" 2>/dev/null || true
    exit 0
  fi
  [ "$status" -eq 0 ] && exit 0
  printf 'kiac: K3s exited with status %s; restarting in the same VM\n' "$status" >&2
  pid=
  sleep 1
done
`

// Older clusters run K3s as PID 1. New clusters record the supervised K3s
// child so probes inspect its executable and effective agent endpoint,
// including an endpoint refreshed by the persistent resume launcher.
const k3sLivePIDScript = `pid=1
if [ -r /var/run/kiac-k3s.pid ]; then
  read -r pid < /var/run/kiac-k3s.pid
fi
case "$pid" in ''|*[!0-9]*) exit 1;; esac
`

// K3s normally evacuates the cgroup root only when it is PID 1. With a
// supervisor, do that before spawning any helpers so domain controllers
// (especially memory) can be enabled for the kubelet's pod cgroups.
const k3sCgroupPrep = `if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  mkdir -p /sys/fs/cgroup/init || exit 1
  while read -r p; do
    [ "$p" = 0 ] || { echo "$p" > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true; }
  done < /sys/fs/cgroup/cgroup.procs
  for c in $(cat /sys/fs/cgroup/cgroup.controllers); do
    echo "+$c" > /sys/fs/cgroup/cgroup.subtree_control || exit 1
  done
fi
`

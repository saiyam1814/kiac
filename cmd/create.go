package cmd

import (
	"fmt"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var createCfg = cluster.Config{}

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a resource",
}

var createClusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Create a Kubernetes cluster of lightweight-VM nodes",
	Example: `  kiac create cluster
  kiac create cluster --name dev --workers 2
  kiac create cluster --memory 8G --cpus 4
  kiac create cluster --distro k3s --workers 1
  kiac create cluster --distro k3s --workers 1 --gpu-workers 1
  kiac create cluster --mount type=bind,source="$PWD",target=/workspace,readonly`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		selectedDistro := createDistro
		selectedK8sVersion := k8sVersion
		// Seed the typed family from the string flag (default ipv4) before
		// Merge, so a config-file value can still override it when the flag
		// was not set on the command line.
		createCfg.IPFamily = cluster.IPFamily(createIPFamily)
		if createConfigFile != "" {
			fc, err := cluster.LoadConfigFile(createConfigFile)
			if err != nil {
				return err
			}
			if err := fc.Merge(&createCfg, &selectedDistro, &selectedK8sVersion, cmd.Flags().Changed); err != nil {
				return err
			}
		}
		if createCfg.Workers < 0 {
			return fmt.Errorf("--workers must be >= 0")
		}
		if createCfg.GPUWorkers < 0 {
			return fmt.Errorf("--gpu-workers must be >= 0")
		}
		if !createCfg.IPFamily.Valid() {
			return fmt.Errorf("invalid ip-family %q (supported: ipv4, dual, ipv6)", createCfg.IPFamily)
		}
		// A non-ipv4 family needs the full kernel (the stock kernel has no
		// IPv6 netfilter). Auto-select it when the user did not name a
		// kernel, so --ip-family dual "just works" without the user
		// knowing which kernel carries IPv6; an explicit --kernel wins.
		if createCfg.GPUWorkers > 0 && createCfg.IPFamily != cluster.IPv4 {
			return fmt.Errorf("real GPU clusters currently require --ip-family ipv4; vmnet-helper does not provide the IPv6 node network yet")
		}
		if createCfg.GPUWorkers > 0 && createKernel != "" {
			return fmt.Errorf("--kernel applies to apple/container nodes; real GPU clusters boot the kernel in --gpu-image")
		}
		if createKernel == "" && createCfg.IPFamily.WantsIPv6() {
			createKernel = "full"
			ui.Infof("--ip-family %s needs the full node kernel; using --kernel full", createCfg.IPFamily)
		}
		if createKernel != "" {
			kpath, err := cluster.ResolveKernel(createKernel)
			if err != nil {
				return err
			}
			createCfg.Kernel = kpath
		}
		if !cluster.ValidName(createCfg.Name) {
			return fmt.Errorf("invalid cluster name %q: use lowercase letters, digits, and dashes", createCfg.Name)
		}
		createCfg.Distro = selectedDistro
		switch selectedDistro {
		case "kubeadm":
			if createCfg.GPUWorkers > 0 && createCfg.Image != "" {
				return fmt.Errorf("--image is an OCI node image and cannot boot real GPU VMs; use --gpu-image for the Fedora disk")
			}
			if createCfg.GPUWorkers > 0 || createCfg.Image == "" {
				if selectedK8sVersion == "" {
					selectedK8sVersion = defaultK8sVersion(selectedDistro)
				}
				img, err := cluster.ResolveImage(selectedK8sVersion)
				if err != nil {
					return err
				}
				createCfg.K8sVersion = cluster.K8sVersionFromImage(img)
				if createCfg.GPUWorkers == 0 {
					createCfg.Image = img
				}
			} else {
				// --image owns the version contract for ordinary clusters. Do
				// not record an unrelated default Kubernetes version beside it.
				createCfg.K8sVersion = cluster.K8sVersionFromImage(createCfg.Image)
			}
			return cluster.NewManager().Create(createCfg)
		case "k3s":
			// Kiac selects K3s networking for the active VM backend: kindnet
			// on apple/container and bundled Flannel on Fedora GPU clusters.
			if cmd.Flags().Changed("cni") || (createConfigFile != "" && createCfg.CNI != "" && createCfg.CNI != "kindnet") {
				return fmt.Errorf("cni selection applies to --distro kubeadm only; ordinary K3s uses kindnet and GPU K3s uses bundled Flannel")
			}
			if createCfg.GPUWorkers > 0 && createCfg.Image != "" {
				return fmt.Errorf("--image is an OCI node image and cannot boot real GPU VMs; use --gpu-image for the Fedora disk")
			}
			if createCfg.GPUWorkers > 0 || createCfg.Image == "" {
				if selectedK8sVersion == "" {
					selectedK8sVersion = defaultK8sVersion(selectedDistro)
				}
				img, err := cluster.ResolveK3sImage(selectedK8sVersion)
				if err != nil {
					return err
				}
				createCfg.K8sVersion = cluster.K8sVersionFromImage(img)
				if createCfg.GPUWorkers == 0 {
					createCfg.Image = img
				}
			} else {
				createCfg.K8sVersion = cluster.K8sVersionFromImage(createCfg.Image)
			}
			return cluster.NewManager().CreateK3s(createCfg)
		default:
			return fmt.Errorf("unknown --distro %q (supported: kubeadm, k3s)", selectedDistro)
		}
	},
}

func defaultK8sVersion(distro string) string {
	if distro == "k3s" {
		return cluster.DefaultK3sVersion
	}
	return cluster.DefaultK8sVersion
}

var (
	k8sVersion       string
	createDistro     string
	createConfigFile string
	createKernel     string
	createIPFamily   string
)

func init() {
	f := createClusterCmd.Flags()
	f.StringVar(&createConfigFile, "config", "", "cluster config YAML (see examples/cluster.yaml); flags set explicitly on the command line override file values")
	f.StringVar(&createCfg.Name, "name", "dev", "cluster name")
	f.IntVar(&createCfg.Workers, "workers", 0, "number of worker nodes (control plane is untainted when 0)")
	f.IntVar(&createCfg.GPUWorkers, "gpu-workers", 0, "number of real Apple GPU worker VMs (alpha; moves the whole cluster to the krunkit backend)")
	f.StringVar(&createCfg.GPUImage, "gpu-image", cluster.DefaultGPUImage, "GPU VM boot disk: fedora-44 (verified download) or a raw ARM64 disk path")
	f.StringVar(&createCfg.GPUDiskSize, "gpu-disk-size", "20G", "writable disk size for each krunkit-backed node")
	f.StringVar(&createCfg.GPUDriver, "gpu-resource-driver", "device-plugin", "Kubernetes GPU resource driver: device-plugin or dra")
	f.StringVar(&k8sVersion, "k8s-version", "",
		"Kubernetes version (default: latest pinned for the selected distro; kubeadm "+cluster.DefaultK8sVersion+", k3s "+cluster.DefaultK3sVersion+")")
	f.StringVar(&createDistro, "distro", "kubeadm",
		"Kubernetes distribution: kubeadm or k3s (ordinary nodes use OCI images; GPU clusters provision Fedora VMs)")
	f.StringVar(&createCfg.Image, "image", "", "node image (overrides --k8s-version)")
	f.StringVar(&createCfg.CNI, "cni", "kindnet", "kubeadm pod network: kindnet, cilium (ordinary clusters need --kernel full), or none (bring your own)")
	f.StringVar(&createIPFamily, "ip-family", "ipv4", "IP family for pods, Services, and nodes: ipv4, dual (IPv4-primary + IPv6), or ipv6 (IPv6-primary; needs the full kernel, auto-selected)")
	f.StringVar(&createKernel, "kernel", "", "custom node kernel: 'full' (downloads the published kiac kernel with VXLAN/eBPF/br_netfilter) or a path to a kernel Image")
	f.StringSliceVar(&createCfg.DNS, "dns", nil, "nameserver IPs for the node VMs, repeatable up to 3 (resolv.conf's own limit); overrides the runtime's default resolv.conf entirely rather than adding to it")
	f.Var(&createCfg.Mounts, "mount", "bind a host directory into every node VM (type=bind,source=/host/path,target=/node/path[,readonly]); repeatable")
	f.StringVar(&createCfg.CPUs, "cpus", "4", "vCPUs per node VM")
	f.StringVar(&createCfg.Memory, "memory", "2G", "memory per worker VM (idle workers use a few hundred MB)")
	f.StringVar(&createCfg.CPMemory, "cp-memory", "4G", "memory for the control-plane VM (etcd, apiserver, and on single-node clusters every addon)")
	f.BoolVar(&createCfg.NoMetrics, "no-metrics", false, "skip installing metrics-server")
	f.BoolVar(&createCfg.NoStorage, "no-storage", false, "skip installing the local-path default StorageClass")
	f.BoolVar(&createCfg.NoLB, "no-lb", false, "skip installing kiac-lb (type: LoadBalancer support)")
	f.BoolVar(&createCfg.NoEdgeProxy, "no-edge-proxy", false, "skip the node-local edge proxy that fixes large TCP uploads through NodePorts and LoadBalancers")
	f.BoolVar(&createCfg.Observability, "observability", false, "install Prometheus + Grafana + node-exporter, Grafana on a LoadBalancer IP")
	f.BoolVar(&createCfg.Gateway, "gateway", false, "install Gateway API CRDs + Traefik with a ready-to-use GatewayClass and Gateway")
	f.DurationVar(&createCfg.WaitTimeout, "wait", 5*time.Minute, "timeout for each cluster readiness step, including CNI installation")
	createCmd.AddCommand(createClusterCmd)
}

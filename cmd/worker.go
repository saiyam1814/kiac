package cmd

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

type externalWorkerService interface {
	Join(cluster.ExternalWorkerConfig) (cluster.ExternalWorkerState, error)
	Workers() ([]cluster.ExternalWorkerInfo, error)
	Start(string, time.Duration) error
	Stop(string) error
	Delete(string) error
	Logs(string) (string, error)
}

var externalWorkerManager = func() externalWorkerService { return cluster.NewExternalWorkerManager() }
var externalWorkerCfg cluster.ExternalWorkerConfig
var externalWorkerOutput string
var externalWorkerStartWait time.Duration

var joinCmd = &cobra.Command{Use: "join", Short: "Join an owned worker VM to an external cluster"}

var joinWorkerCmd = &cobra.Command{
	Use: "worker", Short: "Join one external K3s worker (alpha, WireGuard and Flannel VXLAN)", Args: cobra.NoArgs,
	Long: `Boot a CPU worker in a verified Fedora/krunkit VM and join an existing
IPv4 K3s cluster using its existing Flannel VXLAN CNI. The external cluster's
control plane and nodes must already have bidirectional routes to the worker's
WireGuard IP. Kiac does not change their CNI, networking, credentials or addons.

Provide a private full CA-pinned K3s token file and a private WireGuard config
with one Interface and one Peer. Shell hooks, DNS endpoints, and default routes
are not supported. The VPN address is advertised to Kubernetes; the private
vmnet DHCP address is retained for local VM management. The K3s release and
declared pod/service CIDRs must match the external cluster.

New Nodes have kiac.dev/external-worker=true:NoSchedule. Opt intended workloads
in with a matching toleration and selector. An incomplete agent startup retains
its VM identity; start worker retries that same join.

Stopping or deleting this worker only affects its local VM. Drain workloads
and remove its remote Node separately with your external-cluster credentials.`,
	Example: `  kiac join worker --name lab --server https://10.250.76.1:6443 \
    --token-file ./agent-token --wireguard-config ./worker.conf \
    --node-ip 10.250.76.2 --k8s-version 1.37`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if externalWorkerCfg.WaitTimeout <= 0 {
			return fmt.Errorf("--wait must be > 0")
		}
		ui.Banner(Version)
		state, err := externalWorkerManager().Join(externalWorkerCfg)
		if err != nil {
			return err
		}
		ui.Successf("External K3s worker %q is Ready as Node %s (%s).", state.Name, state.VMName, state.NodeIP)
		return nil
	},
}

var getWorkersCmd = &cobra.Command{
	Use: "workers", Aliases: []string{"worker"}, Short: "List owned external worker VMs", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if externalWorkerOutput != "" && externalWorkerOutput != "json" {
			return fmt.Errorf("unknown output format (supported: json)")
		}
		workers, err := externalWorkerManager().Workers()
		if err != nil {
			return err
		}
		if externalWorkerOutput == "json" {
			if workers == nil {
				workers = []cluster.ExternalWorkerInfo{}
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(workers)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tPHASE\tVM\tREADY\tNODE\tNODE-IP\tMANAGEMENT-IP\tK3S-VERSION")
		for _, worker := range workers {
			fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\t%s\t%s\t%s\n", worker.Name, worker.Phase, worker.VMStatus, worker.Ready, worker.VMName, worker.NodeIP, orDash(worker.ManagementIP), worker.K8sVersion)
		}
		return w.Flush()
	},
}

var startWorkerCmd = &cobra.Command{
	Use: "worker <name>", Short: "Start an owned external worker VM and verify its original Node", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if externalWorkerStartWait <= 0 {
			return fmt.Errorf("--wait must be > 0")
		}
		return externalWorkerManager().Start(args[0], externalWorkerStartWait)
	},
}

var stopWorkerCmd = &cobra.Command{
	Use: "worker <name>", Short: "Stop only the local external-worker VM; does not drain its Node", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := externalWorkerManager().Stop(args[0]); err != nil {
			return err
		}
		ui.Infof("Stopped the local worker VM. Its external Node was not drained or deleted.")
		return nil
	},
}

var deleteWorkerCmd = &cobra.Command{
	Use: "worker <name>", Short: "Delete only the owned local VM and worker inventory", Args: cobra.ExactArgs(1),
	Long: `Delete local resources belonging to this external worker. This does not
drain workloads or delete the Node from the external cluster. Use your external
cluster credentials to drain and remove the remote Node before deleting its VM.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := externalWorkerManager().Delete(args[0]); err != nil {
			return err
		}
		ui.Infof("Deleted the local worker VM. Remove its remote Node separately with external-cluster credentials.")
		return nil
	},
}

var workerLogsRootCmd = &cobra.Command{Use: "logs", Short: "Show logs of an owned resource"}
var logsWorkerCmd = &cobra.Command{
	Use: "worker <name>", Short: "Show external worker network and agent logs", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		logs, err := externalWorkerManager().Logs(args[0])
		if err != nil {
			return err
		}
		fmt.Fprint(cmd.OutOrStdout(), logs)
		return nil
	},
}

func init() {
	f := joinWorkerCmd.Flags()
	f.StringVar(&externalWorkerCfg.Name, "name", "", "local external worker name")
	f.StringVar(&externalWorkerCfg.Server, "server", "", "external K3s HTTPS IPv4:PORT endpoint over WireGuard")
	f.StringVar(&externalWorkerCfg.TokenFile, "token-file", "", "owner-private full CA-pinned K3s secure token file")
	f.StringVar(&externalWorkerCfg.WireGuardFile, "wireguard-config", "", "owner-private single-peer split-route WireGuard config")
	f.StringVar(&externalWorkerCfg.NodeIP, "node-ip", "", "stable IPv4 WireGuard address advertised to Kubernetes")
	f.StringVar(&externalWorkerCfg.K8sVersion, "k8s-version", "", "pinned K3s release/minor matching the external server")
	f.StringVar(&externalWorkerCfg.PodCIDR, "pod-cidr", "10.42.0.0/16", "existing external cluster IPv4 pod CIDR")
	f.StringVar(&externalWorkerCfg.ServiceCIDR, "service-cidr", "10.43.0.0/16", "existing external cluster IPv4 service CIDR")
	f.StringVar(&externalWorkerCfg.CPUs, "cpus", "2", "worker vCPU count")
	f.StringVar(&externalWorkerCfg.Memory, "memory", "2G", "worker memory")
	f.StringVar(&externalWorkerCfg.DiskSize, "disk", "20G", "persistent worker disk size")
	f.DurationVar(&externalWorkerCfg.WaitTimeout, "wait", 10*time.Minute, "bounded boot/agent readiness wait")
	for _, name := range []string{"name", "server", "token-file", "wireguard-config", "node-ip", "k8s-version"} {
		_ = joinWorkerCmd.MarkFlagRequired(name)
	}
	getWorkersCmd.Flags().StringVarP(&externalWorkerOutput, "output", "o", "", "output format: json")
	startWorkerCmd.Flags().DurationVar(&externalWorkerStartWait, "wait", 10*time.Minute, "bounded readiness wait")
	joinCmd.AddCommand(joinWorkerCmd)
	getCmd.AddCommand(getWorkersCmd)
	startCmd.AddCommand(startWorkerCmd)
	stopCmd.AddCommand(stopWorkerCmd)
	deleteCmd.AddCommand(deleteWorkerCmd)
	workerLogsRootCmd.AddCommand(logsWorkerCmd)
	rootCmd.AddCommand(joinCmd, workerLogsRootCmd)
}

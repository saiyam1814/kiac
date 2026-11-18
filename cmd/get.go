package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/spf13/cobra"
)

var (
	getNodesName      string
	getClustersOutput string
	getKubeconfigName string
)

// renderKubeconfig returns a cluster's standalone kubeconfig. Tests replace
// it so command validation runs without the container runtime.
var renderKubeconfig = func(name string) (string, error) {
	return cluster.NewManager().Kubeconfig(name)
}

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "List resources",
}

var getClustersCmd = &cobra.Command{
	Use:     "clusters",
	Aliases: []string{"cluster"},
	Short:   "List kiac clusters",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		statuses, err := cluster.NewManager().Statuses()
		if err != nil {
			return err
		}
		switch getClustersOutput {
		case "json":
			// Empty stays a JSON array so scripts never special-case null.
			if statuses == nil {
				statuses = []cluster.ClusterStatus{}
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(statuses)
		case "", "wide":
		default:
			return fmt.Errorf("unknown output format %q (supported: wide, json)", getClustersOutput)
		}
		if len(statuses) == 0 {
			fmt.Println("No kiac clusters found.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		if getClustersOutput == "wide" {
			fmt.Fprintln(w, "NAME\tSTATUS\tK8S-VERSION\tCREATED")
			for _, s := range statuses {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					s.Name, s.Status, orDash(s.K8sVersion), orDash(cluster.FormatCreated(s.Created)))
			}
		} else {
			fmt.Fprintln(w, "NAME\tSTATUS")
			for _, s := range statuses {
				fmt.Fprintf(w, "%s\t%s\n", s.Name, s.Status)
			}
		}
		return w.Flush()
	},
}

var getNodesCmd = &cobra.Command{
	Use:   "nodes",
	Short: "List node VMs of a cluster",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		infos, err := cluster.NewManager().Nodes(getNodesName)
		if err != nil {
			return err
		}
		if len(infos) == 0 {
			fmt.Printf("No nodes found for cluster %q.\n", getNodesName)
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NODE\tSTATUS\tIMAGE")
		for _, i := range infos {
			fmt.Fprintf(w, "%s\t%s\t%s\n", i.Name, i.Status, i.Image)
		}
		return w.Flush()
	},
}

var getKubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig",
	Short: "Print a cluster's kubeconfig without merging it into ~/.kube/config",
	Long: `Print a standalone kubeconfig for one cluster to stdout, using the context
kiac-<name>. Nothing on disk changes.

The kubeconfig carries cluster-admin credentials. Its server is the control-plane
VM's current address; after a host reboot, run kiac resume cluster first.`,
	Example: `  kiac get kubeconfig --name dev > dev.kubeconfig
  KUBECONFIG=dev.kubeconfig kubectl get nodes`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !cluster.ValidName(getKubeconfigName) {
			return fmt.Errorf("invalid cluster name %q", getKubeconfigName)
		}
		kubeconfig, err := renderKubeconfig(getKubeconfigName)
		if err != nil {
			return err
		}
		// No banner: stdout must stay a valid kubeconfig for redirection.
		_, err = fmt.Fprint(cmd.OutOrStdout(), kubeconfig)
		return err
	},
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	getClustersCmd.Flags().StringVarP(&getClustersOutput, "output", "o", "", "output format: wide or json")
	getNodesCmd.Flags().StringVar(&getNodesName, "name", "dev", "cluster name")
	getKubeconfigCmd.Flags().StringVar(&getKubeconfigName, "name", "dev", "cluster name")
	getCmd.AddCommand(getClustersCmd, getNodesCmd, getKubeconfigCmd)
}

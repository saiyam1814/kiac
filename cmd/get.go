package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/spf13/cobra"
)

var getNodesName string

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "List resources",
}

var getClustersCmd = &cobra.Command{
	Use:     "clusters",
	Aliases: []string{"cluster"},
	Short:   "List kiac clusters",
	RunE: func(cmd *cobra.Command, args []string) error {
		names, err := cluster.NewManager().Clusters()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("No kiac clusters found.")
			return nil
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	},
}

var getNodesCmd = &cobra.Command{
	Use:   "nodes",
	Short: "List node VMs of a cluster",
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

func init() {
	getNodesCmd.Flags().StringVar(&getNodesName, "name", "dev", "cluster name")
	getCmd.AddCommand(getClustersCmd, getNodesCmd)
}

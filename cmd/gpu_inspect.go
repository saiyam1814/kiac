package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/spf13/cobra"
)

func newGPUInspectCommand() *cobra.Command {
	var name, output string
	command := &cobra.Command{
		Use:   "inspect",
		Short: "Explain GPU reservations and workload scheduling",
		Long:  "Read GPU inventory, DRA claims and Pod scheduling conditions across all namespaces. Memory figures are per-VM scheduler accounting, not physical GPU usage or a hard memory limit.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "json" && output != "text" {
				return fmt.Errorf("unknown output format %q (supported: text, json)", output)
			}
			report, err := cluster.NewManager().InspectGPU(name)
			if err != nil {
				return err
			}
			if output == "json" {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(report)
			}
			return printGPUInspection(cmd.OutOrStdout(), report)
		},
	}
	command.Flags().StringVar(&name, "name", "dev", "cluster name")
	command.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	return command
}

func printGPUInspection(out io.Writer, report cluster.GPUInspection) error {
	fmt.Fprintf(out, "Cluster %s (%s), %s via %s\n", report.Status.Cluster, report.Status.Distro, report.Status.Resource, report.Status.Driver)
	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "\nNODE\tSTATE\tWINDOW GiB\tDRIVER READY\tINVENTORY SCHEDULABLE")
	for _, node := range report.Status.Nodes {
		fmt.Fprintf(writer, "%s\t%s\t%.1f\t%t\t%t\n", node.Name, node.VMState, float64(node.MemoryMiB)/1024, node.DriverReady, node.Schedulable)
	}
	if len(report.Devices) > 0 {
		fmt.Fprintln(writer, "\nPOOL / DEVICE\tCAPACITY\tRESERVED\tUNRESERVED (ACCOUNTING)")
		for _, device := range report.Devices {
			reserved, unreserved := "unknown", "unknown"
			if device.Reserved != nil {
				reserved = *device.Reserved
			}
			if device.Unreserved != nil {
				unreserved = *device.Unreserved
			}
			fmt.Fprintf(writer, "%s / %s\t%s\t%s\t%s\n", device.Pool, device.Device, device.Capacity, reserved, unreserved)
		}
	}
	if len(report.Claims) > 0 {
		fmt.Fprintln(writer, "\nCLAIM\tSTATE\tREQUEST\tPOOL / DEVICE\tMEMORY\tSHARE ID\tPODS")
		for _, claim := range report.Claims {
			state := claim.State
			if claim.Deleting {
				state += " (deleting)"
			}
			if len(claim.Allocations) == 0 {
				fmt.Fprintf(writer, "%s/%s\t%s\t-\t-\t-\t-\t%s\n", claim.Namespace, claim.Name, state, strings.Join(claim.Consumers, ","))
			}
			for _, allocation := range claim.Allocations {
				access := state
				if allocation.AdminAccess {
					access += " (admin)"
				}
				fmt.Fprintf(writer, "%s/%s\t%s\t%s\t%s / %s\t%s\t%s\t%s\n", claim.Namespace, claim.Name, access, allocation.Request, allocation.Pool, allocation.Device, orDash(allocation.Memory), orDash(allocation.ShareID), strings.Join(claim.Consumers, ","))
			}
		}
	}
	fmt.Fprintln(writer, "\nWORKLOAD\tPHASE\tNODE\tCLAIMS\tREASON")
	for _, pod := range report.Workloads {
		fmt.Fprintf(writer, "%s/%s\t%s\t%s\t%s\t%s\n", pod.Namespace, pod.Name, pod.Phase, orDash(pod.Node), orDash(strings.Join(pod.Claims, ",")), orDash(pod.Reason))
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	for _, pod := range report.Workloads {
		if pod.Message != "" {
			fmt.Fprintf(out, "\n%s/%s: %s\n", pod.Namespace, pod.Name, pod.Message)
		}
	}
	if len(report.Workloads) == 0 {
		fmt.Fprintln(out, "No GPU workloads found.")
	}
	for _, note := range report.Notes {
		fmt.Fprintf(out, "\nNote: %s\n", note)
	}
	return nil
}

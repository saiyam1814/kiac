package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var gpuCmd = &cobra.Command{
	Use:   "gpu",
	Short: "Inspect and configure real Apple GPU nodes",
}

var gpuDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check the host dependencies for real Apple GPU nodes",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		if err := cluster.NewManager().GPUPreflight(); err != nil {
			ui.Check(false, "Apple GPU backend", err.Error())
			return err
		}
		ui.Check(true, "Apple GPU backend", "krunkit, vmnet-helper, and the Venus renderer are ready")
		return nil
	},
}

var (
	gpuStatusName   string
	gpuStatusOutput string
)

var gpuStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show real GPU nodes and Kubernetes resource-driver health",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		report, err := cluster.NewManager().GPUStatusForCluster(gpuStatusName)
		if err != nil {
			return err
		}
		if gpuStatusOutput == "json" {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(report)
		}
		if gpuStatusOutput != "text" {
			return fmt.Errorf("unknown output format %q (supported: text, json)", gpuStatusOutput)
		}
		ui.Banner(Version)
		fmt.Printf("\nCluster %s (%s), %s via %s\n", report.Cluster, report.Distro, report.Resource, report.Driver)
		for _, node := range report.Nodes {
			detail := fmt.Sprintf("%s, GPU window %.1f GiB, api=%s", node.VMState, float64(node.MemoryMiB)/1024, node.KubernetesAPI)
			ui.Check(node.RenderDevice && node.Schedulable, node.Name, detail)
		}
		compat := "disabled"
		if report.Compatibility.Installed {
			compat = "installed"
			if report.Compatibility.Ready {
				compat = "ready"
			}
			if len(report.Compatibility.Namespaces) > 0 {
				compat += " in " + strings.Join(report.Compatibility.Namespaces, ", ")
			}
		}
		ui.Infof("NVIDIA resource-name compatibility: %s", compat)
		return nil
	},
}

var gpuCompatCmd = &cobra.Command{
	Use:   "compat",
	Short: "Manage opt-in NVIDIA resource-name compatibility",
}

var gpuValuesCmd = &cobra.Command{
	Use:   "values TARGET",
	Short: "Print Apple GPU scheduling values for an inference integration",
	Long: `Print a YAML scheduling fragment for vllm-production-stack, KServe, or
Ollama. The generated values map the workload to kiac.dev/gpu and include the
GPU-node toleration. They do not turn a CUDA image into a Vulkan image.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		values, err := cluster.GPUValues(args[0])
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), values)
		return err
	},
}

var (
	gpuBenchName     string
	gpuBenchModel    string
	gpuBenchSkipHost bool
	gpuBenchOutput   string
	gpuBenchTimeout  time.Duration
)

var gpuBenchCmd = &cobra.Command{
	Use:   "bench",
	Short: "Compare native Metal and Kubernetes Venus inference",
	Long: `Run a reproducible TinyLlama llama-bench workload on the host Metal
backend, when llama-bench is installed, and inside a real kiac Apple GPU pod.
The default model, container image, and download checksum are pinned.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if gpuBenchOutput != "text" && gpuBenchOutput != "json" {
			return fmt.Errorf("unknown output format %q (supported: text, json)", gpuBenchOutput)
		}
		var report cluster.GPUBenchmarkReport
		run := func() error {
			var err error
			report, err = cluster.NewManager().RunGPUBenchmark(context.Background(), cluster.GPUBenchmarkOptions{
				Cluster: gpuBenchName, Model: gpuBenchModel, SkipHost: gpuBenchSkipHost, Timeout: gpuBenchTimeout,
			})
			return err
		}
		if gpuBenchOutput == "json" {
			if err := run(); err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(report)
		}
		ui.Banner(Version)
		ui.Infof("the first run downloads a pinned 638 MiB TinyLlama model into ~/.kiac/models")
		if err := ui.Step("Benchmarking native Metal and Kubernetes Venus", run); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "\nModel: %s\n", report.Model)
		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "BACKEND\tDEVICE\tPROMPT TOK/S\tGEN TOK/S")
		if report.Host != nil {
			fmt.Fprintf(writer, "%s\t%s\t%.2f\t%.2f\n", report.Host.Backend, report.Host.Device,
				report.Host.PromptTokensPerSecond, report.Host.GenerateTokensPerSecond)
		}
		fmt.Fprintf(writer, "%s\t%s\t%.2f\t%.2f\n", report.Kubernetes.Backend, report.Kubernetes.Device,
			report.Kubernetes.PromptTokensPerSecond, report.Kubernetes.GenerateTokensPerSecond)
		if err := writer.Flush(); err != nil {
			return err
		}
		if report.HostSkipped != "" {
			ui.Warnf("native Metal comparison skipped: %s", report.HostSkipped)
		}
		return nil
	},
}

var (
	gpuCompatName      string
	gpuCompatNamespace string
	gpuCompatRotate    bool
)

var gpuCompatEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Rewrite NVIDIA GPU requests in one namespace to kiac.dev/gpu",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		return cluster.NewManager().EnableGPUCompatibility(gpuCompatName, gpuCompatNamespace, gpuCompatRotate)
	},
}

var gpuCompatDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Stop rewriting NVIDIA GPU requests in one namespace",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		return cluster.NewManager().DisableGPUCompatibility(gpuCompatName, gpuCompatNamespace)
	},
}

func init() {
	gpuStatusCmd.Flags().StringVar(&gpuStatusName, "name", "dev", "cluster name")
	gpuStatusCmd.Flags().StringVarP(&gpuStatusOutput, "output", "o", "text", "output format: text or json")
	gpuBenchCmd.Flags().StringVar(&gpuBenchName, "name", "dev", "cluster name")
	gpuBenchCmd.Flags().StringVar(&gpuBenchModel, "model", "", "local GGUF model path (default: download the pinned TinyLlama benchmark model)")
	gpuBenchCmd.Flags().BoolVar(&gpuBenchSkipHost, "skip-host", false, "run only the Kubernetes Venus benchmark")
	gpuBenchCmd.Flags().StringVarP(&gpuBenchOutput, "output", "o", "text", "output format: text or json")
	gpuBenchCmd.Flags().DurationVar(&gpuBenchTimeout, "timeout", 20*time.Minute, "total benchmark timeout including model download and copy")
	for _, command := range []*cobra.Command{gpuCompatEnableCmd, gpuCompatDisableCmd} {
		command.Flags().StringVar(&gpuCompatName, "name", "dev", "cluster name")
		command.Flags().StringVarP(&gpuCompatNamespace, "namespace", "n", "default", "namespace to opt in or out")
	}
	gpuCompatEnableCmd.Flags().BoolVar(&gpuCompatRotate, "rotate-certificate", false, "replace the webhook's serving certificate")
	gpuCompatCmd.AddCommand(gpuCompatEnableCmd, gpuCompatDisableCmd)
	gpuCmd.AddCommand(gpuDoctorCmd, gpuStatusCmd, gpuBenchCmd, gpuValuesCmd, gpuCompatCmd)
}

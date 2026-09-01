package cmd

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var doctorFix bool

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that this Mac can run kiac clusters",
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		rt := runtime.New()
		failures := 0

		osVersion := macOSVersion()
		osOK := majorVersion(osVersion) >= 26
		detail := "macOS " + osVersion
		if !osOK {
			detail += " - multi-node networking needs macOS 26+; single-node may still work"
		}
		ui.Check(osOK, "macOS version", detail)

		cliOK := rt.Available()
		ver := ""
		if cliOK {
			ver, _ = rt.Version()
		}
		switch {
		case !cliOK:
			failures++
			ui.Check(false, "apple/container CLI", "not found - install from https://github.com/apple/container/releases")
		case runtime.ValidateNodeRuntimeVersion(ver) != nil:
			failures++
			ui.Check(false, "apple/container CLI", ver+" - upgrade to 1.2.1 or newer; 1.2.0 cannot boot Kubernetes nodes")
		case majorVersion(ver) < 1:
			failures++
			ui.Check(false, "apple/container CLI", ver+" - upgrade to 1.0.0+ for best results")
		default:
			ui.Check(true, "apple/container CLI", ver)
		}

		if cliOK {
			running := rt.SystemRunning()
			if doctorFix {
				if !running {
					ui.Check(false, "container system service", "stopped - fixing: container system start")
				}
				startErr := rt.SystemStart(true)
				nowRunning := rt.SystemRunning()
				if nowRunning {
					detail := "running"
					if !running {
						detail = "running (started by --fix)"
					}
					ui.Check(true, "container system service", detail)
				} else {
					failures++
					ui.Check(false, "container system service", "still not running after start; inspect with: container system status")
				}
				if startErr != nil {
					failures++
					ui.Check(false, "default kernel", "install failed: "+startErr.Error())
				} else {
					ui.Check(true, "default kernel", "configured (verified/installed by --fix)")
				}
			} else {
				if running {
					ui.Check(true, "container system service", "running")
					ui.Hintf("default kernel is not verified without --fix; run: kiac doctor --fix")
				} else {
					ui.Check(false, "container system service", "stopped - kiac will start it automatically on create, or run: kiac doctor --fix")
				}
			}
		}

		_, kubectlErr := exec.LookPath("kubectl")
		ui.Check(kubectlErr == nil, "kubectl", "")
		if kubectlErr != nil {
			failures++
		}

		arch := unameM()
		archOK := arch == "arm64"
		ui.Check(archOK, "Apple silicon", arch)
		if !archOK {
			failures++
		}

		if failures > 0 {
			return fmt.Errorf("%d check(s) failed", failures)
		}
		fmt.Println()
		ui.Hintf("all good - run: kiac create cluster")
		return nil
	},
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorFix, "fix", false, "attempt to fix failed checks (starts the container system service)")
}

func macOSVersion() string {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func unameM() string {
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func majorVersion(v string) int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	head, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(head)
	if err != nil {
		return 0
	}
	return n
}

package cmd

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var doctorFix bool
var doctorDNS bool

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
		var versionErr error
		if cliOK {
			ver, versionErr = rt.Version()
			if versionErr == nil {
				versionErr = runtime.ValidateNodeRuntimeVersion(ver)
			}
		}
		switch {
		case !cliOK:
			failures++
			ui.Check(false, "apple/container CLI", "not found - install from https://github.com/apple/container/releases")
		case versionErr != nil:
			failures++
			ui.Check(false, "apple/container CLI", versionErr.Error())
		default:
			ui.Check(true, "apple/container CLI", ver)
		}

		if cliOK {
			running := rt.SystemRunning()
			if doctorFix && versionErr == nil {
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
					if versionErr == nil {
						ui.Hintf("default kernel is not verified without --fix; run: kiac doctor --fix")
					}
				} else {
					detail := "stopped - kiac will start it automatically on create, or run: kiac doctor --fix"
					if versionErr != nil {
						detail = "stopped - upgrade apple/container before starting the service"
					}
					ui.Check(false, "container system service", detail)
				}
				if doctorFix && versionErr != nil {
					ui.Hintf("upgrade apple/container before rerunning kiac doctor --fix; the service and kernel were left unchanged")
				}
			}
		}

		if doctorDNS && cliOK && versionErr == nil {
			defaults, err := rt.DNSDefaults(5 * time.Second)
			if err != nil {
				failures++
				ui.Check(false, "apple/container DNS defaults", err.Error())
			} else {
				printDoctorDNS(defaults)
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
	doctorCmd.Flags().BoolVar(&doctorDNS, "dns", false, "show only merged apple/container DNS defaults (read-only; does not test guest resolution)")
}

func printDoctorDNS(defaults runtime.DNSDefaults) {
	ui.Check(true, "apple/container DNS defaults", "read-only; applies to new ordinary nodes, not existing nodes or krunkit")
	if len(defaults.Nameservers) == 0 {
		ui.Infof("nameservers: runtime network resolver (no configured override)")
	} else {
		ui.Infof("nameservers: %q", defaults.Nameservers)
	}
	if defaults.Domain == nil {
		ui.Infof("domain: unset")
	} else {
		ui.Infof("domain: %q (%s)", *defaults.Domain, defaults.DomainSource)
	}
	ui.Infof("search domains: %q", defaults.SearchDomains)
	ui.Infof("resolver options: %q", defaults.Options)
	ui.Hintf("kiac create cluster --dns replaces nameservers only; configured domain, search domains and options remain inherited")
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

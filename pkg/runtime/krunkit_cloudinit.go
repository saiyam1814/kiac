package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type cloudInitWriteFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions,omitempty"`
	Content     string `yaml:"content"`
}

type cloudInitConfig struct {
	SSHAuthorizedKeys []string             `yaml:"ssh_authorized_keys"`
	SSHPwauth         bool                 `yaml:"ssh_pwauth"`
	DisableRoot       bool                 `yaml:"disable_root"`
	Growpart          map[string]any       `yaml:"growpart"`
	ResizeRootFS      bool                 `yaml:"resize_rootfs"`
	WriteFiles        []cloudInitWriteFile `yaml:"write_files,omitempty"`
	RunCmd            [][]string           `yaml:"runcmd,omitempty"`
}

type cloudInitMetadata struct {
	InstanceID    string `yaml:"instance-id"`
	LocalHostname string `yaml:"local-hostname"`
}

type cloudInitNetwork struct {
	Version   int                            `yaml:"version"`
	Ethernets map[string]cloudInitNetworkNIC `yaml:"ethernets"`
}

type cloudInitNetworkNIC struct {
	Match          map[string]string `yaml:"match"`
	SetName        string            `yaml:"set-name"`
	DHCP4          bool              `yaml:"dhcp4"`
	DHCPIdentifier string            `yaml:"dhcp-identifier"`
	DHCPOverrides  map[string]bool   `yaml:"dhcp4-overrides,omitempty"`
	Nameservers    map[string]any    `yaml:"nameservers,omitempty"`
}

func (c *KrunkitClient) createCloudInitISO(nodeDir string, state KrunkitNodeState, publicKey string, dns []string) error {
	seedDir := filepath.Join(nodeDir, "cloud-init")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		return err
	}

	config := cloudInitConfig{
		SSHAuthorizedKeys: []string{strings.TrimSpace(publicKey)},
		SSHPwauth:         false,
		DisableRoot:       true,
		Growpart:          map[string]any{"mode": "auto", "devices": []string{"/"}},
		ResizeRootFS:      true,
		WriteFiles: []cloudInitWriteFile{
			{
				Path:        "/etc/udev/rules.d/70-kiac-gpu.rules",
				Permissions: "0644",
				Content:     "SUBSYSTEM==\"drm\", KERNEL==\"card[0-9]*\", MODE=\"0666\"\nSUBSYSTEM==\"drm\", KERNEL==\"renderD*\", MODE=\"0666\"\n",
			},
			{
				Path:        "/etc/kiac/runtime",
				Permissions: "0644",
				Content:     "krunkit\n",
			},
		},
		RunCmd: [][]string{
			{"udevadm", "control", "--reload-rules"},
			{"udevadm", "trigger", "--subsystem-match=drm"},
		},
	}

	if len(state.Mounts) > 0 {
		var fstab strings.Builder
		for i, mount := range state.Mounts {
			tag := fmt.Sprintf("kiac%d", i)
			options := "nofail,x-systemd.automount"
			if mount.ReadOnly {
				options += ",ro"
			}
			fmt.Fprintf(&fstab, "%s %s virtiofs %s 0 0\n", tag, escapeFstab(mount.Target), options)
			config.RunCmd = append(config.RunCmd,
				[]string{"mkdir", "-p", mount.Target},
				[]string{"mount", mount.Target},
			)
		}
		config.WriteFiles = append(config.WriteFiles, cloudInitWriteFile{
			Path:        "/etc/fstab.d/kiac.conf",
			Permissions: "0644",
			Content:     fstab.String(),
		})
		// Fedora does not read /etc/fstab.d. Keep the generated fragment for
		// diagnostics and append it once during first boot for persistence.
		config.RunCmd = append([][]string{{"sh", "-c", "cat /etc/fstab.d/kiac.conf >> /etc/fstab"}}, config.RunCmd...)
	}

	if err := writeYAML(filepath.Join(seedDir, "user-data"), config, true); err != nil {
		return err
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	if err := writeYAML(filepath.Join(seedDir, "meta-data"), cloudInitMetadata{InstanceID: id, LocalHostname: state.Name}, false); err != nil {
		return err
	}

	nic := cloudInitNetworkNIC{
		Match:          map[string]string{"macaddress": state.MAC},
		SetName:        "eth0",
		DHCP4:          true,
		DHCPIdentifier: "mac",
	}
	if len(dns) > 0 {
		nic.DHCPOverrides = map[string]bool{"use-dns": false}
		nic.Nameservers = map[string]any{"addresses": dns}
	}
	if err := writeYAML(filepath.Join(seedDir, "network-config"), cloudInitNetwork{
		Version:   2,
		Ethernets: map[string]cloudInitNetworkNIC{"eth0": nic},
	}, false); err != nil {
		return err
	}

	hdiutil, err := c.resolveBinary(c.HDIUtilBin, "/usr/bin/hdiutil")
	if err != nil {
		return fmt.Errorf("creating cloud-init disk: %w", err)
	}
	_ = os.Remove(state.Seed)
	args := []string{"makehybrid", "-iso", "-joliet", "-default-volume-name", "cidata", "-o", state.Seed, seedDir}
	out, err := exec.Command(hdiutil, args...).CombinedOutput()
	if err != nil {
		return &CommandError{Tool: hdiutil, Args: args, Output: string(out), Err: err}
	}
	if _, err := os.Stat(state.Seed); err != nil {
		return fmt.Errorf("cloud-init disk was not created at %s: %w", state.Seed, err)
	}
	return nil
}

func writeYAML(path string, value any, cloudConfig bool) error {
	raw, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	if cloudConfig {
		raw = append([]byte("#cloud-config\n"), raw...)
	}
	return os.WriteFile(path, raw, 0o600)
}

func escapeFstab(value string) string {
	replacer := strings.NewReplacer("\\", "\\134", " ", "\\040", "\t", "\\011", "#", "\\043")
	return replacer.Replace(value)
}

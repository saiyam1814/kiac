package runtime

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Mount describes a host directory bind-mounted into a node VM.
type Mount struct {
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"readOnly"`
}

// String returns Apple's container run --mount value.
func (m Mount) String() string {
	s := "type=bind,source=" + m.Source + ",target=" + m.Target
	if m.ReadOnly {
		s += ",readonly"
	}
	return s
}

// Mounts is a repeatable pflag.Value for Docker-style --mount specifications.
// Set appends rather than replaces so each --mount occurrence is preserved.
type Mounts []Mount

func (m *Mounts) Set(value string) error {
	mount, err := ParseMount(value)
	if err != nil {
		return err
	}
	*m = append(*m, mount)
	return nil
}

func (m *Mounts) String() string {
	if m == nil {
		return ""
	}
	values := make([]string, len(*m))
	for i, mount := range *m {
		values[i] = mount.String()
	}
	return strings.Join(values, ";")
}

func (*Mounts) Type() string { return "mount" }

// ParseMount parses the long-form syntax accepted by Apple Container.
func ParseMount(value string) (Mount, error) {
	var mount Mount
	seen := make(map[string]bool)
	for _, field := range strings.Split(value, ",") {
		key, val, hasValue := strings.Cut(field, "=")
		key = strings.TrimSpace(key)
		if seen[key] {
			return Mount{}, fmt.Errorf("invalid mount %q: duplicate %q field", value, key)
		}
		seen[key] = true
		switch key {
		case "type":
			if !hasValue || val != "bind" {
				return Mount{}, fmt.Errorf("invalid mount %q: type must be bind", value)
			}
		case "source":
			if !hasValue || val == "" {
				return Mount{}, fmt.Errorf("invalid mount %q: source is required", value)
			}
			mount.Source = val
		case "target":
			if !hasValue || val == "" {
				return Mount{}, fmt.Errorf("invalid mount %q: target is required", value)
			}
			mount.Target = val
		case "readonly":
			if hasValue {
				return Mount{}, fmt.Errorf("invalid mount %q: readonly does not take a value", value)
			}
			mount.ReadOnly = true
		default:
			return Mount{}, fmt.Errorf("invalid mount %q: unknown field %q", value, key)
		}
	}
	if !seen["type"] {
		return Mount{}, fmt.Errorf("invalid mount %q: type=bind is required", value)
	}
	if mount.Source == "" {
		return Mount{}, fmt.Errorf("invalid mount %q: source is required", value)
	}
	if mount.Target == "" {
		return Mount{}, fmt.Errorf("invalid mount %q: target is required", value)
	}
	return mount, nil
}

// ValidateMounts checks every mount before KIAC creates a node VM.
func ValidateMounts(mounts []Mount) error {
	targets := make(map[string]int, len(mounts))
	for i, mount := range mounts {
		label := fmt.Sprintf("mount %d (source %q, target %q)", i+1, mount.Source, mount.Target)
		if !filepath.IsAbs(mount.Source) {
			return fmt.Errorf("invalid %s: source must be an absolute host path", label)
		}
		if strings.Contains(mount.Source, ",") || strings.Contains(mount.Target, ",") {
			return fmt.Errorf("invalid %s: commas are not supported in mount paths by Apple Container's --mount syntax", label)
		}
		info, err := os.Stat(mount.Source)
		if err != nil {
			return fmt.Errorf("invalid %s: source is not accessible: %w", label, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("invalid %s: source must be a directory", label)
		}
		if !path.IsAbs(mount.Target) {
			return fmt.Errorf("invalid %s: target must be an absolute Linux path", label)
		}
		target := path.Clean(mount.Target)
		if previous, exists := targets[target]; exists {
			return fmt.Errorf("invalid %s: target duplicates mount %d", label, previous)
		}
		targets[target] = i + 1
	}
	return nil
}

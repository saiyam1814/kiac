package cluster

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// FileConfig is the YAML schema behind `create cluster --config`. It
// mirrors Config with friendly keys; every field is optional. Pointer
// fields distinguish "omitted" from an explicit zero value, and addon
// toggles are positive booleans that Merge maps onto the No* fields.
type FileConfig struct {
	Name       string     `yaml:"name"`
	Workers    *int       `yaml:"workers"`
	K8sVersion string     `yaml:"k8sVersion"`
	Image      string     `yaml:"image"`
	CNI        string     `yaml:"cni"`
	IPFamily   string     `yaml:"ipFamily"`
	DNS        []string   `yaml:"dns"`
	CPUs       string     `yaml:"cpus"`
	Memory     string     `yaml:"memory"`
	CPMemory   string     `yaml:"cpMemory"`
	Wait       string     `yaml:"wait"`
	Addons     FileAddons `yaml:"addons"`
}

// FileAddons toggles the optional cluster addons. Omitted keys keep the
// CLI defaults: metrics, storage, and loadBalancer on; observability
// and gateway off.
type FileAddons struct {
	Metrics       *bool `yaml:"metrics"`
	Storage       *bool `yaml:"storage"`
	LoadBalancer  *bool `yaml:"loadBalancer"`
	EdgeProxy     *bool `yaml:"edgeProxy"`
	Observability *bool `yaml:"observability"`
	Gateway       *bool `yaml:"gateway"`
}

// LoadConfigFile parses a cluster config file. Unknown keys are an
// error so a typo never silently falls back to a default.
func LoadConfigFile(path string) (*FileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var fc FileConfig
	if err := dec.Decode(&fc); err != nil {
		// An empty file is a valid "all defaults" config.
		if errors.Is(err, io.EOF) {
			return &FileConfig{}, nil
		}
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	return &fc, nil
}

// Merge writes file values into cfg for every knob whose CLI flag was
// not explicitly set. changed is cobra's Flags().Changed, so a flag
// given on the command line always wins over the file. k8sVersion is
// merged separately because the CLI resolves it to an image afterwards.
func (fc *FileConfig) Merge(cfg *Config, k8sVersion *string, changed func(string) bool) error {
	if fc.Name != "" && !changed("name") {
		cfg.Name = fc.Name
	}
	if fc.Workers != nil && !changed("workers") {
		cfg.Workers = *fc.Workers
	}
	if fc.K8sVersion != "" && !changed("k8s-version") {
		*k8sVersion = fc.K8sVersion
	}
	if fc.Image != "" && !changed("image") {
		cfg.Image = fc.Image
	}
	if fc.CNI != "" && !changed("cni") {
		cfg.CNI = fc.CNI
	}
	if fc.IPFamily != "" && !changed("ip-family") {
		cfg.IPFamily = IPFamily(fc.IPFamily)
	}
	if len(fc.DNS) > 0 && !changed("dns") {
		cfg.DNS = fc.DNS
	}
	if fc.CPUs != "" && !changed("cpus") {
		cfg.CPUs = fc.CPUs
	}
	if fc.CPMemory != "" && !changed("cp-memory") {
		cfg.CPMemory = fc.CPMemory
	}
	if fc.Memory != "" && !changed("memory") {
		cfg.Memory = fc.Memory
	}
	if fc.Wait != "" && !changed("wait") {
		d, err := time.ParseDuration(fc.Wait)
		if err != nil {
			return fmt.Errorf("invalid wait %q in config file (want a duration like 5m): %w", fc.Wait, err)
		}
		cfg.WaitTimeout = d
	}
	if fc.Addons.Metrics != nil && !changed("no-metrics") {
		cfg.NoMetrics = !*fc.Addons.Metrics
	}
	if fc.Addons.Storage != nil && !changed("no-storage") {
		cfg.NoStorage = !*fc.Addons.Storage
	}
	if fc.Addons.LoadBalancer != nil && !changed("no-lb") {
		cfg.NoLB = !*fc.Addons.LoadBalancer
	}
	if fc.Addons.EdgeProxy != nil && !changed("no-edge-proxy") {
		cfg.NoEdgeProxy = !*fc.Addons.EdgeProxy
	}
	if fc.Addons.Observability != nil && !changed("observability") {
		cfg.Observability = *fc.Addons.Observability
	}
	if fc.Addons.Gateway != nil && !changed("gateway") {
		cfg.Gateway = *fc.Addons.Gateway
	}
	return nil
}

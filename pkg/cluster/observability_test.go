package cluster

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// pinnedImage requires an explicit numeric version tag; bare names,
// :latest, and digests-only references all fail it.
var pinnedImage = regexp.MustCompile(`^[a-z0-9./_-]+:v?\d[A-Za-z0-9._-]*$`)

func TestObservabilityManifests(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		want     []string // substrings that must appear
	}{
		{"namespace", obsNamespaceManifest, []string{"kiac-observability"}},
		{"node-exporter", obsNodeExporterManifest, []string{"quay.io/prometheus/node-exporter", "DaemonSet"}},
		{"kube-state-metrics", obsKubeStateMetricsManifest, []string{"registry.k8s.io/kube-state-metrics/kube-state-metrics", "ClusterRoleBinding"}},
		{"prometheus", obsPrometheusManifest, []string{"quay.io/prometheus/prometheus", "--storage.tsdb.retention.time=6h", "insecure_skip_verify: true"}},
		{"grafana", obsGrafanaManifest, []string{"docker.io/grafana/grafana", "type: LoadBalancer", "GF_AUTH_ANONYMOUS_ENABLED"}},
	}
	if got := len(observabilityManifests()); got != len(cases) {
		t.Fatalf("observabilityManifests() returned %d manifests, want %d", got, len(cases))
	}

	var images []string
	for _, c := range cases {
		for _, want := range c.want {
			if !strings.Contains(c.manifest, want) {
				t.Errorf("%s: missing %q", c.name, want)
			}
		}
		dec := yaml.NewDecoder(strings.NewReader(c.manifest))
		for {
			var doc map[string]any
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: does not parse as YAML: %v", c.name, err)
			}
			if doc == nil {
				continue
			}
			if doc["kind"] == nil || doc["apiVersion"] == nil {
				t.Errorf("%s: document missing kind or apiVersion: %v", c.name, doc)
			}
			images = append(images, collectImages(doc)...)
		}
	}

	if len(images) < 4 {
		t.Fatalf("found only %d container images across manifests, want at least 4: %v", len(images), images)
	}
	for _, img := range images {
		if !pinnedImage.MatchString(img) {
			t.Errorf("image %q is not pinned to a version tag", img)
		}
	}
}

// collectImages walks a decoded YAML document for "image" values.
func collectImages(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "image" {
				if s, ok := val.(string); ok {
					out = append(out, s)
				}
				continue
			}
			out = append(out, collectImages(val)...)
		}
	case []any:
		for _, e := range x {
			out = append(out, collectImages(e)...)
		}
	}
	return out
}

package cluster

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// decodeAll parses every document in a multi-doc YAML stream and
// returns how many are non-empty.
func decodeAll(t *testing.T, name, manifest string) int {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	docs := 0
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("%s: invalid YAML after %d doc(s): %v", name, docs, err)
		}
		if len(doc) > 0 {
			docs++
		}
	}
	return docs
}

func TestGatewayManifests(t *testing.T) {
	// Images must be pinned to an exact version tag: "latest", bare
	// names, or major-only tags would make cluster creates drift.
	pinned := regexp.MustCompile(`^image:\s*"?[a-z0-9./-]+:v?\d+\.\d+\.\d+`)

	cases := []struct {
		name     string
		manifest string
		minDocs  int
		contains []string
	}{
		{
			name:     "gateway-api-crds",
			manifest: gatewayCRDsManifest,
			minDocs:  5, // experimental channel: standard kinds + TLSRoute, BackendTLSPolicy, ...
			contains: []string{
				"gateway.networking.k8s.io/bundle-version: v1.5.1",
				"name: httproutes.gateway.networking.k8s.io",
				"name: gateways.gateway.networking.k8s.io",
				// Traefik's informers require these beyond the standard channel.
				"name: tlsroutes.gateway.networking.k8s.io",
				"name: backendtlspolicies.gateway.networking.k8s.io",
			},
		},
		{
			name:     "traefik",
			manifest: gatewayTraefikManifest,
			minDocs:  6, // Namespace, SA, CR, CRB, Deployment, Service
			contains: []string{
				"namespace: kiac-gateway",
				"--providers.kubernetesgateway=true",
				"type: LoadBalancer",
			},
		},
		{
			name:     "default-gateway",
			manifest: gatewayDefaultManifest,
			minDocs:  2, // GatewayClass, Gateway
			contains: []string{
				"controllerName: traefik.io/gateway-controller",
				"name: kiac",
				"from: All",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decodeAll(t, c.name, c.manifest); got < c.minDocs {
				t.Errorf("parsed %d docs, want at least %d", got, c.minDocs)
			}
			for _, want := range c.contains {
				if !strings.Contains(c.manifest, want) {
					t.Errorf("missing %q", want)
				}
			}
			for _, line := range strings.Split(c.manifest, "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "image:") {
					continue
				}
				if !pinned.MatchString(line) {
					t.Errorf("unpinned image reference: %q", line)
				}
			}
		})
	}
}

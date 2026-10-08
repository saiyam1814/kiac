package cluster

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestLoadImagesRegistersKubernetesReferenceOnEveryNode(t *testing.T) {
	for _, tt := range []struct {
		image string
		want  string
	}{
		{"swift-agent:latest", "docker.io/library/swift-agent:latest"},
		{"swift-agent", "docker.io/library/swift-agent:latest"},
		{"team/swift-agent:dev", "docker.io/team/swift-agent:dev"},
		{"docker.io/swift-agent:dev", "docker.io/library/swift-agent:dev"},
		{"docker.io/library/swift-agent:latest", "docker.io/library/swift-agent:latest"},
		{"index.docker.io/team/swift-agent:dev", "docker.io/team/swift-agent:dev"},
		{"ghcr.io/team/swift-agent:dev", "ghcr.io/team/swift-agent:dev"},
		{"localhost:5000/swift-agent:dev", "localhost:5000/swift-agent:dev"},
		{"swift-agent@sha256:" + strings.Repeat("a", 64), "docker.io/library/swift-agent@sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(tt.image, func(t *testing.T) {
			rt := newLoadTestRuntime()
			m := &Manager{rt: rt}
			if err := m.LoadImages("bionic", []string{tt.image}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(rt.saved, []string{tt.image}) {
				t.Fatalf("exported %v, want original local reference %q", rt.saved, tt.image)
			}
			for _, node := range rt.nodes {
				if !slices.Contains(rt.imported[node.Name], tt.want) {
					t.Errorf("node %s imported %v; Kubernetes resolves the pod image as %q", node.Name, rt.imported[node.Name], tt.want)
				}
				if !slices.Contains(rt.verified[node.Name], tt.want) {
					t.Errorf("node %s did not verify %q", node.Name, tt.want)
				}
			}
			assertLoadArchivesRemoved(t, rt)
		})
	}
}

func TestLoadImagesWithoutArchiveNames(t *testing.T) {
	rt := newLoadTestRuntime()
	rt.anonymous = true
	if err := (&Manager{rt: rt}).LoadImages("bionic", []string{"swift-agent:latest"}); err != nil {
		t.Fatal(err)
	}
	for _, node := range rt.nodes {
		if !slices.Equal(rt.imported[node.Name], []string{"docker.io/library/swift-agent:latest"}) {
			t.Errorf("node %s imported %v, want explicitly named image", node.Name, rt.imported[node.Name])
		}
	}
	assertLoadArchivesRemoved(t, rt)
}

func TestLoadImagesExportsAndImportsEveryImage(t *testing.T) {
	rt := newLoadTestRuntime()
	images := []string{"api:dev", "web:dev"}
	if err := (&Manager{rt: rt}).LoadImages("bionic", images); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.saved, images) {
		t.Fatalf("exports = %v, want %v", rt.saved, images)
	}
	for _, node := range rt.nodes {
		for _, want := range []string{"docker.io/library/api:dev", "docker.io/library/web:dev"} {
			if !slices.Contains(rt.imported[node.Name], want) {
				t.Errorf("node %s missing %q: %v", node.Name, want, rt.imported[node.Name])
			}
		}
	}
	assertLoadArchivesRemoved(t, rt)
}

func TestLoadImagesRejectsInvalidReferenceBeforeExport(t *testing.T) {
	rt := newLoadTestRuntime()
	if err := (&Manager{rt: rt}).LoadImages("bionic", []string{"not a valid image"}); err == nil {
		t.Fatal("expected invalid image reference error")
	}
	if len(rt.saved) != 0 {
		t.Fatalf("exported invalid reference: %v", rt.saved)
	}
}

func TestLoadImagesFailures(t *testing.T) {
	for _, stage := range []string{"list", "save", "import", "verify"} {
		t.Run(stage, func(t *testing.T) {
			rt := newLoadTestRuntime()
			rt.fail = stage
			err := (&Manager{rt: rt}).LoadImages("bionic", []string{"swift-agent:latest"})
			if !errors.Is(err, errLoadTest) {
				t.Fatalf("error = %v, want original %s failure", err, stage)
			}
			if (stage == "import" || stage == "verify") && (!strings.Contains(err.Error(), rt.nodes[0].Name) || !strings.Contains(err.Error(), "swift-agent:latest")) {
				t.Errorf("%s error lacks node/image context: %v", stage, err)
			}
			assertLoadArchivesRemoved(t, rt)
		})
	}
}

func TestLoadImagesRejectsMissingReferenceAfterSuccessfulImport(t *testing.T) {
	for _, output := range []string{"", "swift-agent:latest\n", "docker.io/library/other:latest\n", "warning: docker.io/library/swift-agent:latest not found\n"} {
		t.Run(output, func(t *testing.T) {
			rt := newLoadTestRuntime()
			rt.verifyOutput = &output
			err := (&Manager{rt: rt}).LoadImages("bionic", []string{"swift-agent:latest"})
			if err == nil || !strings.Contains(err.Error(), "docker.io/library/swift-agent:latest") || !strings.Contains(err.Error(), rt.nodes[0].Name) {
				t.Fatalf("error = %v, want missing expected reference and node", err)
			}
			assertLoadArchivesRemoved(t, rt)
		})
	}
}

func TestLoadImagesVerificationAllowsRuntimeWarnings(t *testing.T) {
	rt := newLoadTestRuntime()
	out := "time=2026-10-08 level=warning msg=DEPRECATION\ndocker.io/library/swift-agent:latest\n"
	rt.verifyOutput = &out
	if err := (&Manager{rt: rt}).LoadImages("bionic", []string{"swift-agent:latest"}); err != nil {
		t.Fatalf("valid image name with runtime warning rejected: %v", err)
	}
	assertLoadArchivesRemoved(t, rt)
}

func TestLoadImagesMissingCluster(t *testing.T) {
	rt := newLoadTestRuntime()
	rt.nodes = nil
	if err := (&Manager{rt: rt}).LoadImages("bionic", []string{"swift-agent:latest"}); err == nil || !strings.Contains(err.Error(), "no cluster") {
		t.Fatalf("error = %v, want missing cluster", err)
	}
	if len(rt.saved) != 0 {
		t.Fatalf("exported images for missing cluster: %v", rt.saved)
	}
}

var errLoadTest = errors.New("image runtime failed")

type loadTestRuntime struct {
	runtime.HostRuntime
	nodes        []runtime.Info
	saved        []string
	paths        []string
	imported     map[string][]string
	verified     map[string][]string
	fail         string
	anonymous    bool
	verifyOutput *string
}

func newLoadTestRuntime() *loadTestRuntime {
	return &loadTestRuntime{
		nodes: []runtime.Info{
			{Name: "kiac-bionic-control-plane"},
			{Name: "kiac-bionic-worker-1"},
			{Name: "kiac-bionic-worker-2"},
		},
		imported: make(map[string][]string),
		verified: make(map[string][]string),
	}
}

func (r *loadTestRuntime) List(string) ([]runtime.Info, error) {
	if r.fail == "list" {
		return nil, errLoadTest
	}
	return r.nodes, nil
}

func (r *loadTestRuntime) ImageSave(image, path string) error {
	r.saved = append(r.saved, image)
	r.paths = append(r.paths, path)
	if r.fail == "save" {
		return errLoadTest
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	// Apple image save preserves a locally built short reference in the
	// descriptor annotations. ctr trusts io.containerd.image.name verbatim;
	// Kubernetes' CRI lookup instead expands Docker Hub names and :latest.
	annotations := map[string]string{
		"com.apple.containerization.image.name": image,
		"io.containerd.image.name":              image,
		"org.opencontainers.image.ref.name":     image,
	}
	if r.anonymous {
		annotations = nil
	}
	index := map[string]any{"schemaVersion": 2, "manifests": []any{map[string]any{
		"annotations": annotations,
	}}}
	body, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "index.json", Mode: 0o644, Size: int64(len(body))}); err != nil {
		return err
	}
	if _, err := tw.Write(body); err != nil {
		return err
	}
	return tw.Close()
}

func (r *loadTestRuntime) ExecStdin(node string, input io.Reader, command ...string) error {
	if r.fail == "import" {
		return errLoadTest
	}
	if len(command) < 6 || !slices.Equal(command[:5], []string{"ctr", "-n", "k8s.io", "image", "import"}) || command[len(command)-1] != "-" {
		return fmt.Errorf("unexpected import command: %v", command)
	}
	tr := tar.NewReader(input)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "index.json" {
		return fmt.Errorf("archive was not rewound for %s: %v", node, err)
	}
	var index struct {
		Manifests []struct {
			Annotations map[string]string
		}
	}
	if err := json.NewDecoder(tr).Decode(&index); err != nil {
		return err
	}
	for _, manifest := range index.Manifests {
		if name := manifest.Annotations["io.containerd.image.name"]; name != "" {
			r.imported[node] = append(r.imported[node], name)
		}
	}
	for i := 5; i+1 < len(command); i++ {
		if command[i] == "--index-name" {
			r.imported[node] = append(r.imported[node], command[i+1])
		}
	}
	return nil
}

func (r *loadTestRuntime) ExecTimeout(node string, timeout time.Duration, command ...string) (string, error) {
	if r.fail == "verify" {
		return "", errLoadTest
	}
	if timeout <= 0 || timeout > 10*time.Second || len(command) != 7 || !slices.Equal(command[:6], []string{"ctr", "-n", "k8s.io", "images", "ls", "-q"}) || !strings.HasPrefix(command[6], "name==") {
		return "", fmt.Errorf("unexpected verification: timeout %s, command %v", timeout, command)
	}
	name := strings.TrimPrefix(command[6], "name==")
	r.verified[node] = append(r.verified[node], name)
	if r.verifyOutput != nil {
		return *r.verifyOutput, nil
	}
	if slices.Contains(r.imported[node], name) {
		return name + "\n", nil
	}
	return "", nil
}

func assertLoadArchivesRemoved(t *testing.T, rt *loadTestRuntime) {
	t.Helper()
	for _, path := range rt.paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("archive %s was not removed: %v", path, err)
		}
	}
}

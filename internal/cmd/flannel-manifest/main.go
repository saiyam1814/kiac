// Command flannel-manifest regenerates pkg/cluster/assets/flannel.yaml from
// an upstream flannel release. It downloads the release's kube-flannel.yml,
// pins every image reference to its registry digest, and adds the
// liveness/readiness probes upstream ships in its Documentation copy but
// not in the release asset, so a version bump reproduces kiac's two local
// edits instead of hand-applying them.
//
//	make flannel-manifest FLANNEL_VERSION=v0.28.9
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

func main() {
	version := flag.String("version", "", "flannel release tag, e.g. v0.28.9")
	out := flag.String("out", "pkg/cluster/assets/flannel.yaml", "manifest to write")
	flag.Parse()
	if *version == "" {
		fmt.Fprintln(os.Stderr, "usage: flannel-manifest -version vX.Y.Z [-out path]")
		os.Exit(2)
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	url := fmt.Sprintf("https://github.com/flannel-io/flannel/releases/download/%s/kube-flannel.yml", *version)
	body, err := get(client, url, "")
	if err != nil {
		fail(err)
	}
	digests := map[string]string{}
	for _, image := range Images(body) {
		digest, err := ghcrDigest(client, image)
		if err != nil {
			fail(fmt.Errorf("%s: %w", image, err))
		}
		digests[image] = digest
	}
	patched, err := Patch(body, digests)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*out, []byte(patched), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s from %s\n", *out, url)
	for image, digest := range digests {
		fmt.Printf("  %s@%s\n", image, digest)
	}
	fmt.Printf("now set FlannelVersion = %q in pkg/cluster/flannel.go and run: go test ./pkg/cluster -run Flannel\n", *version)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "flannel-manifest:", err)
	os.Exit(1)
}

var imageLine = regexp.MustCompile(`(?m)^(\s*image: )(\S+)$`)

// Images lists the distinct image references in a manifest.
func Images(manifest string) []string {
	seen := map[string]bool{}
	var images []string
	for _, m := range imageLine.FindAllStringSubmatch(manifest, -1) {
		if !seen[m[2]] {
			seen[m[2]] = true
			images = append(images, m[2])
		}
	}
	return images
}

// Patch pins every image line to its digest and inserts the health probes
// into the kube-flannel container. It refuses manifests whose shape it does
// not recognize rather than emitting a half-patched file.
func Patch(manifest string, digests map[string]string) (string, error) {
	for image, digest := range digests {
		if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
			return "", fmt.Errorf("digest for %s is not a sha256: %q", image, digest)
		}
	}
	patched := imageLine.ReplaceAllStringFunc(manifest, func(line string) string {
		m := imageLine.FindStringSubmatch(line)
		digest, ok := digests[m[2]]
		if !ok || strings.Contains(m[2], "@") {
			return line
		}
		return m[1] + m[2] + "@" + digest
	})
	for image := range digests {
		if !strings.Contains(patched, image+"@") {
			return "", fmt.Errorf("image %s not found in manifest", image)
		}
	}

	lines := strings.Split(patched, "\n")
	nameAt := indexOf(lines, "        name: kube-flannel")
	argAt := indexOf(lines, "        - --kube-subnet-mgr")
	if nameAt < 0 || argAt < 0 || argAt > nameAt {
		return "", fmt.Errorf("kube-flannel container not found in the expected shape; review the manifest by hand")
	}
	if indexOf(lines, "        readinessProbe:") >= 0 {
		return "", fmt.Errorf("manifest already carries a readinessProbe; refusing to add a second one")
	}
	var out []string
	for i, line := range lines {
		switch i {
		case argAt:
			out = append(out, line, "        - --healthz-port=8081")
		case nameAt:
			out = append(out,
				"        livenessProbe:",
				"          httpGet:",
				"            path: /healthz",
				"            port: healthz",
				"          initialDelaySeconds: 10",
				"          periodSeconds: 30",
				line,
				"        ports:",
				"        - containerPort: 8081",
				"          name: healthz",
				"          protocol: TCP",
				"        readinessProbe:",
				"          httpGet:",
				"            path: /readyz",
				"            port: healthz",
				"          initialDelaySeconds: 5",
				"          periodSeconds: 10",
			)
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n"), nil
}

// indexOf returns the position of the only line equal to want, or -1
// when it is absent or ambiguous.
func indexOf(lines []string, want string) int {
	at := -1
	for i, line := range lines {
		if line == want {
			if at >= 0 {
				return -1
			}
			at = i
		}
	}
	return at
}

// ghcrDigest resolves a ghcr.io/<repo>:<tag> reference to the digest of
// its manifest index through the registry API with an anonymous pull token.
func ghcrDigest(client *http.Client, image string) (string, error) {
	rest, ok := strings.CutPrefix(image, "ghcr.io/")
	if !ok {
		return "", fmt.Errorf("only ghcr.io images are supported")
	}
	repo, tag, ok := strings.Cut(rest, ":")
	if !ok || strings.Contains(tag, "/") {
		return "", fmt.Errorf("expected ghcr.io/<repo>:<tag>")
	}
	raw, err := get(client, "https://ghcr.io/token?scope=repository:"+repo+":pull", "")
	if err != nil {
		return "", err
	}
	var token struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(raw), &token); err != nil || token.Token == "" {
		return "", fmt.Errorf("no anonymous pull token for %s", repo)
	}
	req, err := http.NewRequest(http.MethodHead, "https://ghcr.io/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry answered %s", resp.Status)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("registry returned no digest")
	}
	return digest, nil
}

func get(client *http.Client, url, accept string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

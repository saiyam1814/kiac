package cluster

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"text/template"
	"time"

	"github.com/saiyam1814/kiac/pkg/ui"
)

const (
	gpuCompatName           = "kiac-gpu-compat"
	gpuCompatNamespaceLabel = "kiac.dev/gpu-compat"
	gpuCompatServerName     = "kiac-gpu-compat.kube-system.svc"
	gpuCompatRenewBefore    = 30 * 24 * time.Hour
)

type gpuCompatTemplateData struct {
	ControlPlane string
	ServerCert   string
	ServerKey    string
	CABundle     string
	TLSID        string
}

// GPUCompatibilityStatus describes the optional NVIDIA resource-name bridge.
// It never implies CUDA support; the bridge only rewrites scheduling metadata.
type GPUCompatibilityStatus struct {
	Installed  bool     `json:"installed"`
	Ready      bool     `json:"ready"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// EnableGPUCompatibility installs the small, opt-in admission webhook and
// enables it for one namespace. Existing healthy certificates are reused
// unless rotateCertificate is true.
func (m *Manager) EnableGPUCompatibility(name, namespace string, rotateCertificate bool) error {
	cp, distro, err := m.gpuClusterContext(name)
	if err != nil {
		return err
	}
	if !validNamespaceName(namespace) {
		return fmt.Errorf("invalid namespace %q: use a Kubernetes DNS label", namespace)
	}
	if _, err := m.gpuKubectl(cp, distro, "get", "namespace", namespace, "-o", "name"); err != nil {
		return fmt.Errorf("namespace %q does not exist: %w", namespace, err)
	}

	status, err := m.GPUCompatibilityStatus(name)
	if err != nil {
		return err
	}
	if rotateCertificate || !status.Installed || !status.Ready {
		if err := ui.Step("Installing GPU compatibility webhook", func() error {
			if err := m.uploadGPUAgent(cp); err != nil {
				return err
			}
			data, err := newGPUCompatTLS(time.Now())
			if err != nil {
				return err
			}
			data.ControlPlane = cp
			// Avoid a fail-closed CA mismatch while the serving certificate and
			// webhook trust bundle are replaced.
			if _, err := m.gpuKubectl(cp, distro, "delete", "mutatingwebhookconfiguration", gpuCompatName,
				"--ignore-not-found"); err != nil {
				return err
			}
			coreManifest, err := renderGPUCompatManifest("core", gpuCompatCoreManifest, data)
			if err != nil {
				return err
			}
			if err := m.gpuKubectlStdin(cp, distro, strings.NewReader(coreManifest), "apply", "-f", "-"); err != nil {
				return err
			}
			if _, err := m.gpuKubectl(cp, distro, "rollout", "status", "deployment/"+gpuCompatName,
				"-n", "kube-system", "--timeout=2m"); err != nil {
				return err
			}
			webhookManifest, err := renderGPUCompatManifest("webhook", gpuCompatWebhookManifest, data)
			if err != nil {
				return err
			}
			return m.gpuKubectlStdin(cp, distro, strings.NewReader(webhookManifest), "apply", "-f", "-")
		}); err != nil {
			return err
		}
	}

	if err := ui.Step("Enabling NVIDIA resource compatibility in "+namespace, func() error {
		_, err := m.gpuKubectl(cp, distro, "label", "namespace", namespace,
			gpuCompatNamespaceLabel+"=nvidia", "--overwrite")
		return err
	}); err != nil {
		return err
	}
	ui.Successf("Namespace %q maps NVIDIA resource names to %s; CUDA and NVML remain unavailable.", namespace, gpuResourceName)
	return nil
}

// DisableGPUCompatibility removes a namespace from the webhook scope. When no
// opted-in namespaces remain, it removes the webhook and its idle Deployment.
func (m *Manager) DisableGPUCompatibility(name, namespace string) error {
	cp, distro, err := m.gpuClusterContext(name)
	if err != nil {
		return err
	}
	if !validNamespaceName(namespace) {
		return fmt.Errorf("invalid namespace %q: use a Kubernetes DNS label", namespace)
	}
	if _, err := m.gpuKubectl(cp, distro, "label", "namespace", namespace, gpuCompatNamespaceLabel+"-"); err != nil {
		return err
	}
	remaining, err := m.gpuKubectl(cp, distro, "get", "namespaces", "-l", gpuCompatNamespaceLabel+"=nvidia", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(remaining) == "" {
		if _, err := m.gpuKubectl(cp, distro, "delete", "mutatingwebhookconfiguration", gpuCompatName, "--ignore-not-found"); err != nil {
			return err
		}
		if _, err := m.gpuKubectl(cp, distro, "delete", "deployment,service,secret", gpuCompatName,
			"-n", "kube-system", "--ignore-not-found"); err != nil {
			return err
		}
	}
	ui.Successf("NVIDIA resource compatibility disabled in namespace %q.", namespace)
	return nil
}

// GPUCompatibilityStatus reports installation health and opted-in namespaces.
func (m *Manager) GPUCompatibilityStatus(name string) (GPUCompatibilityStatus, error) {
	cp, distro, err := m.gpuClusterContext(name)
	if err != nil {
		return GPUCompatibilityStatus{}, err
	}
	status := GPUCompatibilityStatus{}
	deployment, err := m.gpuKubectl(cp, distro, "get", "deployment", gpuCompatName, "-n", "kube-system",
		"--ignore-not-found", "-o", "jsonpath={.metadata.name}{' '}{.status.readyReplicas}")
	if err != nil {
		return status, err
	}
	webhook, err := m.gpuKubectl(cp, distro, "get", "mutatingwebhookconfiguration", gpuCompatName,
		"--ignore-not-found", "-o", "name")
	if err != nil {
		return status, err
	}
	secret, err := m.gpuKubectl(cp, distro, "get", "secret", gpuCompatName, "-n", "kube-system",
		"--ignore-not-found", "-o", "name")
	if err != nil {
		return status, err
	}
	status.Installed = strings.TrimSpace(deployment) != "" && strings.TrimSpace(webhook) != "" && strings.TrimSpace(secret) != ""
	if status.Installed && strings.HasSuffix(strings.TrimSpace(deployment), " 1") {
		serverCert, err := m.gpuKubectl(cp, distro, "get", "secret", gpuCompatName, "-n", "kube-system",
			"-o", "jsonpath={.data.tls\\.crt}")
		if err != nil {
			return status, err
		}
		caBundle, err := m.gpuKubectl(cp, distro, "get", "mutatingwebhookconfiguration", gpuCompatName,
			"-o", "jsonpath={.webhooks[0].clientConfig.caBundle}")
		if err != nil {
			return status, err
		}
		status.Ready = gpuCompatTLSHealthy(serverCert, caBundle, time.Now())
	}
	namespaces, err := m.gpuKubectl(cp, distro, "get", "namespaces", "-l", gpuCompatNamespaceLabel+"=nvidia",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{'\\n'}{end}")
	if err != nil {
		return status, err
	}
	for _, namespace := range strings.Fields(namespaces) {
		status.Namespaces = append(status.Namespaces, namespace)
	}
	return status, nil
}

func (m *Manager) gpuClusterContext(name string) (string, string, error) {
	if !ValidName(name) {
		return "", "", fmt.Errorf("invalid cluster name %q", name)
	}
	infos, err := m.rt.List(prefix(name))
	if err != nil {
		return "", "", err
	}
	if len(infos) == 0 {
		return "", "", fmt.Errorf("no cluster named %q found", name)
	}
	cp := ControlPlane(name)
	foundCP := false
	foundGPU := false
	for _, info := range infos {
		if info.Name == cp {
			foundCP = strings.EqualFold(info.Status, "running")
		}
		if strings.HasPrefix(info.Name, prefix(name)+"gpu-") {
			foundGPU = true
		}
	}
	if !foundCP {
		return "", "", fmt.Errorf("control-plane VM %s is not running", cp)
	}
	if !foundGPU {
		return "", "", fmt.Errorf("cluster %q has no real Apple GPU nodes", name)
	}
	return cp, distroFromNodes(infos), nil
}

func (m *Manager) uploadGPUAgent(node string) error {
	binary, err := gpuAgentBinary()
	if err != nil {
		return fmt.Errorf("reading embedded GPU agent: %w", err)
	}
	return m.rt.ExecStdin(node, bytes.NewReader(binary), "sh", "-euc", `
install -d -m 0755 /usr/local/libexec/kiac
tmp="$(mktemp /usr/local/libexec/kiac/kiac-gpu-agent.XXXXXX)"
cat > "$tmp"
chmod 0755 "$tmp"
mv "$tmp" /usr/local/libexec/kiac/kiac-gpu-agent
`)
}

func newGPUCompatTLS(now time.Time) (gpuCompatTemplateData, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return gpuCompatTemplateData{}, err
	}
	caSerial, err := randomSerial()
	if err != nil {
		return gpuCompatTemplateData{}, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "kiac-gpu-compat-ca"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(3, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return gpuCompatTemplateData{}, err
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return gpuCompatTemplateData{}, err
	}
	serverSerial, err := randomSerial()
	if err != nil {
		return gpuCompatTemplateData{}, err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: serverSerial,
		Subject:      pkix.Name{CommonName: gpuCompatServerName},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{
			"kiac-gpu-compat",
			"kiac-gpu-compat.kube-system",
			"kiac-gpu-compat.kube-system.svc",
			"kiac-gpu-compat.kube-system.svc.cluster.local",
		},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return gpuCompatTemplateData{}, err
	}
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return gpuCompatTemplateData{}, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	serverPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER})
	digest := sha256.Sum256(serverDER)
	return gpuCompatTemplateData{
		ServerCert: base64.StdEncoding.EncodeToString(serverPEM),
		ServerKey:  base64.StdEncoding.EncodeToString(serverKeyPEM),
		CABundle:   base64.StdEncoding.EncodeToString(caPEM),
		TLSID:      hex.EncodeToString(digest[:8]),
	}, nil
}

func gpuCompatTLSHealthy(serverCert, caBundle string, now time.Time) bool {
	serverPEM, err := base64.StdEncoding.DecodeString(strings.TrimSpace(serverCert))
	if err != nil {
		return false
	}
	caPEM, err := base64.StdEncoding.DecodeString(strings.TrimSpace(caBundle))
	if err != nil {
		return false
	}
	serverBlock, _ := pem.Decode(serverPEM)
	if serverBlock == nil || serverBlock.Type != "CERTIFICATE" {
		return false
	}
	certificate, err := x509.ParseCertificate(serverBlock.Bytes)
	if err != nil {
		return false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return false
	}
	verify := func(at time.Time) bool {
		_, err := certificate.Verify(x509.VerifyOptions{
			DNSName:     gpuCompatServerName,
			Roots:       roots,
			CurrentTime: at,
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		return err == nil
	}
	return verify(now) && verify(now.Add(gpuCompatRenewBefore))
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func renderGPUCompatManifest(name, source string, data gpuCompatTemplateData) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(source)
	if err != nil {
		return "", err
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return "", err
	}
	return rendered.String(), nil
}

func validNamespaceName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	return ValidName(name)
}

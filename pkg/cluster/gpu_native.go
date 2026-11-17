package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/nativegpu"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const nativeNamespace = "kiac-native-gpu"
const nativeDriver = "metal.kiac.dev"
const nativeManagedLabel = "kiac.dev/native-gpu"
const nativeAgentImage = "docker.io/library/busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0"

type NativeGPUOptions struct {
	Cluster  string
	Node     string
	StateDir string
}

// InstallNativeGPU attaches one explicitly selected ordinary node to the Mac
// host service. This is independent of the existing Venus GPU cluster backend.
func (m *Manager) InstallNativeGPU(ctx context.Context, opts NativeGPUOptions) error {
	if !ValidName(opts.Cluster) {
		return errors.New("invalid cluster name")
	}
	credentials, err := nativegpu.AdminCredentials(opts.StateDir)
	if err != nil {
		return err
	}
	endpoint, err := url.Parse(credentials.Endpoint)
	if err != nil {
		return errors.New("invalid native GPU endpoint")
	}
	ip := net.ParseIP(endpoint.Hostname())
	if ip == nil || ip.IsLoopback() || !ip.IsPrivate() {
		return errors.New("Pods need a private VM-network endpoint: restart 'kiac gpu native serve' with --listen <Mac VM-network IP>:5181")
	}
	response, err := nativegpu.Request(ctx, credentials, http.MethodGet, "/v1/admin/health", nil)
	if err != nil {
		return fmt.Errorf("native GPU host service is not ready: %w", err)
	}
	response.Body.Close()
	nodes, err := m.Nodes(opts.Cluster)
	if err != nil {
		return err
	}
	selected := opts.Node
	if selected != "" && !strings.HasPrefix(selected, prefix(opts.Cluster)) {
		selected = prefix(opts.Cluster) + selected
	}
	if selected == "" {
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
		for _, node := range nodes {
			if strings.Contains(node.Name, "-worker-") && strings.EqualFold(node.Status, "running") {
				selected = node.Name
				break
			}
		}
		if selected == "" {
			return errors.New("select a running owned node with --node (a worker is recommended)")
		}
	}
	owned := false
	for _, node := range nodes {
		if node.Name == selected && strings.EqualFold(node.Status, "running") {
			owned = true
		}
	}
	if !owned {
		return errors.New("selected native GPU node is not a running node owned by this cluster")
	}
	raw, err := nativeKubectl(ctx, opts.Cluster, nil, nil, "get", "node", selected, "-o", "json")
	if err != nil {
		return err
	}
	var node corev1.Node
	if json.Unmarshal(raw, &node) != nil || node.Name != selected {
		return errors.New("native GPU target node inventory is invalid")
	}
	if err := requireDRAKubernetesVersion(node.Status.NodeInfo.KubeletVersion); err != nil {
		return err
	}
	if node.Spec.Unschedulable {
		return errors.New("selected native GPU node is cordoned")
	}
	for _, object := range nativeOwnedObjects() {
		if err := checkNativeOwnership(ctx, opts.Cluster, object); err != nil {
			return err
		}
	}
	existing, err := nativeDeployment(ctx, opts.Cluster)
	if err != nil {
		return err
	}
	if err := validateNativeRetarget(existing, selected); err != nil {
		return err
	}
	slices, err := readNativeSlices(ctx, opts.Cluster)
	if err != nil {
		return err
	}
	if _, err := nativeSliceNames(slices, selected, node.UID); err != nil {
		return err
	}
	if err := m.uploadGPUAgent(selected); err != nil {
		return fmt.Errorf("upload native DRA agent: %w", err)
	}
	ca, err := os.ReadFile(credentials.CAFile)
	if err != nil {
		return err
	}
	agent, err := gpuAgentBinary()
	if err != nil {
		return err
	}
	manifest, err := nativeManifest(selected, credentials.Endpoint, credentials.Token, ca, agent)
	if err != nil {
		return err
	}
	if _, err := nativeKubectl(ctx, opts.Cluster, strings.NewReader(manifest), []string{credentials.Token}, "apply", "-f", "-"); err != nil {
		return err
	}
	if _, err := nativeKubectl(ctx, opts.Cluster, nil, nil, "-n", nativeNamespace, "rollout", "status", "deployment/"+nativeNamespace, "--timeout=3m"); err != nil {
		return err
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		slices, sliceErr := readNativeSlices(ctx, opts.Cluster)
		raw, podErr := nativeKubectl(ctx, opts.Cluster, nil, nil, "-n", nativeNamespace, "get", "pods", "-l", "app.kubernetes.io/name="+nativeNamespace, "-o", "json")
		var pods corev1.PodList
		if sliceErr == nil && podErr == nil && json.Unmarshal(raw, &pods) == nil && nativeReadySlice(slices, pods, selected, node.UID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("native DRA plugin started but did not publish a healthy host-service device; inspect its logs in namespace kiac-native-gpu")
}

// A previous agent's healthy slice can outlive its Pod. Require fresh health
// published by the current ready agent, including after same-node reinstall.
func nativeReadySlice(slices resourceapi.ResourceSliceList, pods corev1.PodList, node string, nodeUID types.UID) bool {
	if _, err := nativeSliceNames(slices, node, nodeUID); err != nil {
		return false
	}
	var podUID types.UID
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		if podUID != "" || pod.UID == "" || pod.Spec.NodeName != node || pod.Status.Phase != corev1.PodRunning {
			return false
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			return false
		}
		podUID = pod.UID
	}
	if podUID == "" {
		return false
	}
	for _, slice := range slices.Items {
		if slice.Spec.Driver != nativeDriver {
			continue
		}
		for _, device := range slice.Spec.Devices {
			attribute := device.Attributes["driverPodUID"]
			if device.Name == "metal-0" && len(device.Taints) == 0 && attribute.StringValue != nil && *attribute.StringValue == string(podUID) {
				return true
			}
		}
	}
	return false
}

func checkNativeOwnership(ctx context.Context, cluster, object string) error {
	args := []string{"get", object, "-o", "json", "--ignore-not-found"}
	if strings.HasPrefix(object, "deployment/") || strings.HasPrefix(object, "secret/") || strings.HasPrefix(object, "serviceaccount/") || strings.HasPrefix(object, "job/") {
		args = append(args, "-n", nativeNamespace)
	}
	raw, err := nativeKubectl(ctx, cluster, nil, nil, args...)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var info struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Metadata.Labels[nativeManagedLabel] != "true" {
		return fmt.Errorf("refusing to replace %s: it is not managed by Kiac native GPU", object)
	}
	return nil
}

func (m *Manager) UninstallNativeGPU(ctx context.Context, cluster string) error {
	if !ValidName(cluster) {
		return errors.New("invalid cluster name")
	}
	for _, object := range append(nativeOwnedObjects(), "job/"+nativeNamespace+"-cleanup") {
		if err := checkNativeOwnership(ctx, cluster, object); err != nil {
			return err
		}
	}
	if err := requireNoNativeClaims(ctx, cluster); err != nil {
		return err
	}
	deployment, err := nativeDeployment(ctx, cluster)
	if err != nil {
		return err
	}
	slices, err := readNativeSlices(ctx, cluster)
	if err != nil {
		return err
	}
	selected := ""
	if deployment != nil {
		selected = deployment.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
	}
	if selected == "" {
		for _, slice := range slices.Items {
			if slice.Spec.Driver == nativeDriver {
				return errors.New("native ResourceSlices exist without the owned installation; restore the installation before cleanup")
			}
		}
	}
	if selected != "" {
		if !strings.HasPrefix(selected, prefix(cluster)) {
			return errors.New("native installation node does not belong to this cluster")
		}
		infos, err := m.Nodes(cluster)
		if err != nil {
			return err
		}
		owned := false
		for _, info := range infos {
			if info.Name == selected && strings.EqualFold(info.Status, "running") {
				owned = true
			}
		}
		if !owned {
			return errors.New("resume the native GPU node before uninstalling so its lease credentials can be cleaned safely")
		}
		raw, err := nativeKubectl(ctx, cluster, nil, nil, "get", "node", selected, "-o", "json")
		if err != nil {
			return err
		}
		var node corev1.Node
		if json.Unmarshal(raw, &node) != nil {
			return errors.New("cannot verify native node identity")
		}
		if _, err := nativeSliceNames(slices, selected, node.UID); err != nil {
			return err
		}
		if _, err := nativeKubectl(ctx, cluster, nil, nil, "-n", nativeNamespace, "scale", "deployment/"+nativeNamespace, "--replicas=0"); err != nil {
			return err
		}
		raw, err = nativeKubectl(ctx, cluster, nil, nil, "-n", nativeNamespace, "get", "pods", "-l", "app.kubernetes.io/name="+nativeNamespace, "-o", "json")
		if err != nil {
			return err
		}
		var pods corev1.PodList
		if json.Unmarshal(raw, &pods) != nil {
			return errors.New("cannot verify native driver shutdown")
		}
		if len(pods.Items) > 0 {
			args := []string{"-n", nativeNamespace, "wait", "--for=delete", "--timeout=90s"}
			for _, pod := range pods.Items {
				args = append(args, "pod/"+pod.Name)
			}
			if _, err := nativeKubectl(ctx, cluster, nil, nil, args...); err != nil {
				return err
			}
		}
		slices, err = readNativeSlices(ctx, cluster)
		if err != nil {
			return err
		}
		names, err := nativeSliceNames(slices, selected, node.UID)
		if err != nil {
			return err
		}
		for _, name := range names {
			if _, err := nativeKubectl(ctx, cluster, nil, nil, "delete", "resourceslice/"+name, "--ignore-not-found"); err != nil {
				return err
			}
		}
		if err := requireNoNativeClaims(ctx, cluster); err != nil {
			return err
		}
		job, err := nativeCleanupJob(deployment)
		if err != nil {
			return err
		}
		if _, err := nativeKubectl(ctx, cluster, nil, nil, "-n", nativeNamespace, "delete", "job/"+nativeNamespace+"-cleanup", "--ignore-not-found", "--wait=true", "--timeout=30s"); err != nil {
			return err
		}
		if _, err := nativeKubectl(ctx, cluster, bytes.NewReader(job), nil, "apply", "-f", "-"); err != nil {
			return err
		}
		if _, err := nativeKubectl(ctx, cluster, nil, nil, "-n", nativeNamespace, "wait", "--for=condition=complete", "job/"+nativeNamespace+"-cleanup", "--timeout=100s"); err != nil {
			return fmt.Errorf("native lease cleanup did not complete; restore the host service and retry uninstall: %w", err)
		}
	}
	_, err = nativeKubectl(ctx, cluster, nil, nil, "delete", "namespace/"+nativeNamespace, "clusterrole/"+nativeNamespace, "clusterrolebinding/"+nativeNamespace, "deviceclass/"+nativeDriver, "--ignore-not-found", "--wait=true", "--timeout=2m")
	return err
}

func nativeOwnedObjects() []string {
	return []string{"namespace/" + nativeNamespace, "clusterrole/" + nativeNamespace, "clusterrolebinding/" + nativeNamespace, "deviceclass/" + nativeDriver, "deployment/" + nativeNamespace, "secret/" + nativeNamespace, "serviceaccount/" + nativeNamespace}
}

func nativeDeployment(ctx context.Context, cluster string) (*appsv1.Deployment, error) {
	raw, err := nativeKubectl(ctx, cluster, nil, nil, "-n", nativeNamespace, "get", "deployment", nativeNamespace, "-o", "json", "--ignore-not-found")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var deployment appsv1.Deployment
	if err := json.Unmarshal(raw, &deployment); err != nil {
		return nil, errors.New("invalid native GPU deployment")
	}
	return &deployment, nil
}

func validateNativeRetarget(deployment *appsv1.Deployment, node string) error {
	if deployment != nil && deployment.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != node {
		return errors.New("native GPU alpha cannot change nodes while installed; delete native consumers/claims and uninstall before selecting a different node")
	}
	return nil
}

func readNativeSlices(ctx context.Context, cluster string) (resourceapi.ResourceSliceList, error) {
	raw, err := nativeKubectl(ctx, cluster, nil, nil, "get", "resourceslices.resource.k8s.io", "-o", "json")
	if err != nil {
		return resourceapi.ResourceSliceList{}, err
	}
	var list resourceapi.ResourceSliceList
	err = json.Unmarshal(raw, &list)
	return list, err
}

func nativeSliceNames(list resourceapi.ResourceSliceList, node string, uid types.UID) ([]string, error) {
	var names []string
	for _, slice := range list.Items {
		if slice.Spec.Driver != nativeDriver {
			continue
		}
		owned := false
		for _, owner := range slice.OwnerReferences {
			if owner.APIVersion == "v1" && owner.Kind == "Node" && owner.Name == node && owner.UID == uid && uid != "" {
				owned = true
			}
		}
		if !owned || slice.Spec.NodeName == nil || *slice.Spec.NodeName != node {
			return nil, errors.New("native GPU inventory belongs to a different or unknown node; clean up the previous installation before proceeding")
		}
		names = append(names, slice.Name)
	}
	return names, nil
}

func requireNoNativeClaims(ctx context.Context, cluster string) error {
	raw, err := nativeKubectl(ctx, cluster, nil, nil, "get", "resourceclaims.resource.k8s.io", "-A", "-o", "json")
	if err != nil {
		return err
	}
	var claims resourceapi.ResourceClaimList
	if json.Unmarshal(raw, &claims) != nil {
		return errors.New("cannot verify native GPU claim cleanup")
	}
	for _, claim := range claims.Items {
		if claim.Status.Allocation != nil {
			for _, allocation := range claim.Status.Allocation.Devices.Results {
				if allocation.Driver == nativeDriver {
					return fmt.Errorf("delete native GPU consumers and ResourceClaim %s/%s before uninstalling", claim.Namespace, claim.Name)
				}
			}
		}
	}
	return nil
}

func nativeCleanupJob(deployment *appsv1.Deployment) ([]byte, error) {
	if deployment == nil || len(deployment.Spec.Template.Spec.Containers) != 1 {
		return nil, errors.New("cannot derive safe native cleanup job")
	}
	template := deployment.Spec.Template.DeepCopy()
	template.Labels = map[string]string{"app.kubernetes.io/name": nativeNamespace + "-cleanup", nativeManagedLabel: "true"}
	template.Annotations = nil
	no := false
	template.Spec.AutomountServiceAccountToken = &no
	template.Spec.RestartPolicy = corev1.RestartPolicyNever
	found := false
	for i, arg := range template.Spec.Containers[0].Args {
		if arg == "--mode=native-dra" {
			template.Spec.Containers[0].Args[i] = "--mode=native-cleanup"
			found = true
		}
	}
	if !found {
		return nil, errors.New("native deployment has unexpected agent arguments")
	}
	zero := int32(0)
	deadline := int64(90)
	ttl := int32(300)
	job := batchv1.Job{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}, ObjectMeta: metav1.ObjectMeta{Name: nativeNamespace + "-cleanup", Namespace: nativeNamespace, Labels: map[string]string{nativeManagedLabel: "true"}}, Spec: batchv1.JobSpec{Template: *template, BackoffLimit: &zero, ActiveDeadlineSeconds: &deadline, TTLSecondsAfterFinished: &ttl}}
	return json.Marshal(job)
}

func nativeKubectl(ctx context.Context, cluster string, input io.Reader, secrets []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "kubectl", append([]string{"--context", "kiac-" + cluster, "--request-timeout=30s"}, args...)...)
	command.Stdin = input
	output, err := command.CombinedOutput()
	if err == nil {
		return output, nil
	}
	message := string(output)
	for _, secret := range secrets {
		message = strings.ReplaceAll(message, secret, "<redacted>")
		message = strings.ReplaceAll(message, base64.StdEncoding.EncodeToString([]byte(secret)), "<redacted>")
	}
	if len(message) > 4096 {
		message = message[:4096] + "…"
	}
	return nil, fmt.Errorf("native GPU Kubernetes operation failed: %w: %s", err, strings.TrimSpace(message))
}

func nativeManifest(node, endpoint, token string, ca, agent []byte) (string, error) {
	labels := map[string]string{"app.kubernetes.io/name": nativeNamespace, "app.kubernetes.io/managed-by": "kiac", nativeManagedLabel: "true"}
	metadata := func(name string, namespaced bool) map[string]any {
		m := map[string]any{"name": name, "labels": labels}
		if namespaced {
			m["namespace"] = nativeNamespace
		}
		return m
	}
	objects := []any{
		map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": metadata(nativeNamespace, false)},
		map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": metadata(nativeNamespace, true)},
		map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": metadata(nativeNamespace, true), "type": "Opaque", "data": map[string][]byte{"ca.crt": ca, "admin.token": []byte(token)}},
		map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": metadata(nativeNamespace, false), "rules": []any{
			map[string]any{"apiGroups": []string{"resource.k8s.io"}, "resources": []string{"resourceclaims"}, "verbs": []string{"get", "list", "watch"}},
			map[string]any{"apiGroups": []string{"resource.k8s.io"}, "resources": []string{"resourceclaims/status"}, "verbs": []string{"update"}},
			map[string]any{"apiGroups": []string{"resource.k8s.io"}, "resources": []string{"resourceclaims/driver"}, "resourceNames": []string{nativeDriver}, "verbs": []string{"associated-node:update", "associated-node:patch"}},
			map[string]any{"apiGroups": []string{"resource.k8s.io"}, "resources": []string{"resourceslices"}, "verbs": []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			map[string]any{"apiGroups": []string{""}, "resources": []string{"nodes"}, "verbs": []string{"get"}},
		}},
		map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": metadata(nativeNamespace, false), "roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": nativeNamespace}, "subjects": []any{map[string]any{"kind": "ServiceAccount", "name": nativeNamespace, "namespace": nativeNamespace}}},
		map[string]any{"apiVersion": "resource.k8s.io/v1", "kind": "DeviceClass", "metadata": metadata(nativeDriver, false), "spec": map[string]any{"selectors": []any{map[string]any{"cel": map[string]any{"expression": "device.driver == 'metal.kiac.dev'"}}}}},
	}
	agentHash := sha256.Sum256(agent)
	credentialHash := sha256.Sum256(append(append([]byte{}, ca...), []byte(token)...))
	volumes := []any{
		map[string]any{"name": "agent", "hostPath": map[string]any{"path": gpuAgentNodePath, "type": "File"}},
		map[string]any{"name": "plugins", "hostPath": map[string]any{"path": "/var/lib/kubelet/plugins", "type": "DirectoryOrCreate"}},
		map[string]any{"name": "registry", "hostPath": map[string]any{"path": "/var/lib/kubelet/plugins_registry", "type": "DirectoryOrCreate"}},
		map[string]any{"name": "cdi", "hostPath": map[string]any{"path": "/etc/cdi", "type": "DirectoryOrCreate"}},
		map[string]any{"name": "broker", "secret": map[string]any{"secretName": nativeNamespace, "defaultMode": 256}},
	}
	mounts := []any{
		map[string]any{"name": "agent", "mountPath": "/opt/kiac/kiac-gpu-agent", "readOnly": true},
		map[string]any{"name": "plugins", "mountPath": "/var/lib/kubelet/plugins"},
		map[string]any{"name": "registry", "mountPath": "/var/lib/kubelet/plugins_registry"},
		map[string]any{"name": "cdi", "mountPath": "/etc/cdi"},
		map[string]any{"name": "broker", "mountPath": "/etc/kiac-metal", "readOnly": true},
	}
	container := map[string]any{"name": "agent", "image": nativeAgentImage, "imagePullPolicy": "IfNotPresent", "command": []string{"/opt/kiac/kiac-gpu-agent"}, "args": []string{"--mode=native-dra", "--node-name=$(NODE_NAME)", "--pod-uid=$(POD_UID)", "--native-node-name=" + node, "--native-endpoint=" + endpoint, "--native-ca=/etc/kiac-metal/ca.crt", "--native-admin-token-file=/etc/kiac-metal/admin.token", "--native-lease-ttl=120s"}, "env": []any{map[string]any{"name": "NODE_NAME", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "spec.nodeName"}}}, map[string]any{"name": "POD_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}}}, "securityContext": map[string]any{"privileged": true, "readOnlyRootFilesystem": true}, "resources": map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "64Mi"}, "limits": map[string]string{"memory": "128Mi"}}, "volumeMounts": mounts}
	objects = append(objects, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata(nativeNamespace, true), "spec": map[string]any{"replicas": 1, "strategy": map[string]any{"type": "Recreate"}, "selector": map[string]any{"matchLabels": map[string]string{"app.kubernetes.io/name": nativeNamespace}}, "template": map[string]any{"metadata": map[string]any{"labels": labels, "annotations": map[string]string{"kiac.dev/native-agent-sha256": hex.EncodeToString(agentHash[:]), "kiac.dev/native-credentials-sha256": hex.EncodeToString(credentialHash[:])}}, "spec": map[string]any{"serviceAccountName": nativeNamespace, "hostNetwork": true, "dnsPolicy": "ClusterFirstWithHostNet", "nodeSelector": map[string]string{"kubernetes.io/hostname": node}, "tolerations": []any{map[string]any{"operator": "Exists", "effect": "NoSchedule"}}, "containers": []any{container}, "volumes": volumes}}}})
	var rendered strings.Builder
	for i, object := range objects {
		if i > 0 {
			rendered.WriteString("\n---\n")
		}
		data, err := json.Marshal(object)
		if err != nil {
			return "", err
		}
		rendered.Write(data)
		rendered.WriteByte('\n')
	}
	return rendered.String(), nil
}

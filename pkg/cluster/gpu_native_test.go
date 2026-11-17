package cluster

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestValidateNativeRetargetPreservesAnExistingInstallation(t *testing.T) {
	const node = "kiac-demo-worker-1"
	if err := validateNativeRetarget(nil, node); err != nil {
		t.Fatalf("first installation rejected: %v", err)
	}
	for _, target := range []string{node, "kiac-demo-worker-2", ""} {
		t.Run("existing-"+target, func(t *testing.T) {
			deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": target}},
			}}}
			err := validateNativeRetarget(deployment, node)
			if target == node && err != nil {
				t.Fatalf("idempotent installation rejected: %v", err)
			}
			if target != node && err == nil {
				t.Fatal("existing installation could silently move or adopt an unknown node")
			}
		})
	}
}

func nativeOwnedTestSlice(name, node string, uid types.UID) resourceapi.ResourceSlice {
	return resourceapi.ResourceSlice{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{
		{APIVersion: "v1", Kind: "Node", Name: node, UID: uid},
	}}, Spec: resourceapi.ResourceSliceSpec{Driver: nativeDriver, NodeName: &node}}
}

func TestNativeSliceNamesSelectsOnlyTheVerifiedNodeInventory(t *testing.T) {
	const node = "kiac-demo-worker-1"
	uid := types.UID("owned-node-uid")
	owned := nativeOwnedTestSlice("native-owned", node, uid)
	foreign := nativeOwnedTestSlice("venus-inventory", "another-node", "another-node-uid")
	foreign.Spec.Driver = "gpu.kiac.dev"
	names, err := nativeSliceNames(resourceapi.ResourceSliceList{Items: []resourceapi.ResourceSlice{foreign, owned}}, node, uid)
	if err != nil || !reflect.DeepEqual(names, []string{owned.Name}) {
		t.Fatalf("selected slices %v, error %v; wanted only the owned native slice", names, err)
	}
	names, err = nativeSliceNames(resourceapi.ResourceSliceList{}, node, uid)
	if err != nil || len(names) != 0 {
		t.Fatalf("empty inventory was not idempotent: %v, %v", names, err)
	}
}

func TestNativeSliceNamesRefusesAmbiguousOrForeignNativeOwnership(t *testing.T) {
	const node = "kiac-demo-worker-1"
	uid := types.UID("owned-node-uid")
	for name, mutate := range map[string]func(*resourceapi.ResourceSlice){
		"other-node":           func(slice *resourceapi.ResourceSlice) { other := "kiac-demo-worker-2"; slice.Spec.NodeName = &other },
		"missing-node":         func(slice *resourceapi.ResourceSlice) { slice.Spec.NodeName = nil },
		"missing-owner":        func(slice *resourceapi.ResourceSlice) { slice.OwnerReferences = nil },
		"recreated-node":       func(slice *resourceapi.ResourceSlice) { slice.OwnerReferences[0].UID = "replacement-node-uid" },
		"different-owner-name": func(slice *resourceapi.ResourceSlice) { slice.OwnerReferences[0].Name = "another-node" },
		"pod-owner":            func(slice *resourceapi.ResourceSlice) { slice.OwnerReferences[0].Kind = "Pod" },
		"foreign-node-api":     func(slice *resourceapi.ResourceSlice) { slice.OwnerReferences[0].APIVersion = "example.com/v1" },
	} {
		t.Run(name, func(t *testing.T) {
			owned := nativeOwnedTestSlice("verified-first", node, uid)
			unverified := nativeOwnedTestSlice("must-not-delete", node, uid)
			mutate(&unverified)
			if _, err := nativeSliceNames(resourceapi.ResourceSliceList{Items: []resourceapi.ResourceSlice{owned, unverified}}, node, uid); err == nil {
				t.Fatal("native inventory with unverified ownership was eligible for deletion")
			}
		})
	}
}

func TestNativeReadySliceRequiresCurrentPodHealth(t *testing.T) {
	const node = "kiac-demo-worker-1"
	newUID := "current-agent"
	slice := nativeOwnedTestSlice("native", node, "node-uid")
	slice.Spec.Devices = []resourceapi.Device{{Name: "metal-0", Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"driverPodUID": {StringValue: &newUID},
	}}}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID(newUID)}, Spec: corev1.PodSpec{NodeName: node}, Status: corev1.PodStatus{
		Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}}
	for _, test := range []struct {
		name   string
		mutate func(*resourceapi.ResourceSlice, *corev1.PodList)
	}{
		{"old-agent", func(s *resourceapi.ResourceSlice, _ *corev1.PodList) {
			old := "old-agent"
			s.Spec.Devices[0].Attributes["driverPodUID"] = resourceapi.DeviceAttribute{StringValue: &old}
		}},
		{"missing-identity", func(s *resourceapi.ResourceSlice, _ *corev1.PodList) {
			delete(s.Spec.Devices[0].Attributes, "driverPodUID")
		}},
		{"unhealthy", func(s *resourceapi.ResourceSlice, _ *corev1.PodList) {
			s.Spec.Devices[0].Taints = []resourceapi.DeviceTaint{{Key: "metal.kiac.dev/unavailable", Effect: resourceapi.DeviceTaintEffectNoSchedule}}
		}},
		{"not-ready", func(_ *resourceapi.ResourceSlice, p *corev1.PodList) { p.Items[0].Status.Conditions = nil }},
		{"not-running", func(_ *resourceapi.ResourceSlice, p *corev1.PodList) { p.Items[0].Status.Phase = corev1.PodPending }},
		{"wrong-node", func(_ *resourceapi.ResourceSlice, p *corev1.PodList) { p.Items[0].Spec.NodeName = "other" }},
		{"terminating", func(_ *resourceapi.ResourceSlice, p *corev1.PodList) {
			now := metav1.Now()
			p.Items[0].DeletionTimestamp = &now
		}},
		{"no-agent", func(_ *resourceapi.ResourceSlice, p *corev1.PodList) { p.Items = nil }},
		{"multiple-agents", func(_ *resourceapi.ResourceSlice, p *corev1.PodList) { p.Items = append(p.Items, *pod.DeepCopy()) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			inventory := resourceapi.ResourceSliceList{Items: []resourceapi.ResourceSlice{*slice.DeepCopy()}}
			pods := corev1.PodList{Items: []corev1.Pod{*pod.DeepCopy()}}
			if !nativeReadySlice(inventory, pods, node, "node-uid") {
				t.Fatal("current healthy agent rejected")
			}
			test.mutate(&inventory.Items[0], &pods)
			if nativeReadySlice(inventory, pods, node, "node-uid") {
				t.Fatal("stale or unavailable agent reported ready")
			}
		})
	}
}

func nativeManifestTestObjects(t *testing.T, token string, ca, agent []byte) map[string]json.RawMessage {
	t.Helper()
	manifest, err := nativeManifest("kiac-demo-worker-1", "https://192.168.64.1:5181", token, ca, agent)
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(manifest))
	objects := map[string]json.RawMessage{}
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("manifest is not valid Kubernetes YAML/JSON: %v", err)
		}
		raw, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		var identity struct {
			metav1.TypeMeta
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &identity); err != nil || identity.Kind == "" || identity.Metadata.Name == "" {
			t.Fatalf("invalid manifest object: %s, %v", raw, err)
		}
		key := identity.Kind + "/" + identity.Metadata.Name
		if _, exists := objects[key]; exists {
			t.Fatalf("duplicate manifest object %s", key)
		}
		objects[key] = raw
	}
	return objects
}

func nativeManifestTestDecode(t *testing.T, objects map[string]json.RawMessage, kind, name string, out any) {
	t.Helper()
	raw, ok := objects[kind+"/"+name]
	if !ok {
		t.Fatalf("missing %s/%s", kind, name)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

func TestNativeManifestPublishesOneExplicitHostServiceWithoutReplacingVenus(t *testing.T) {
	objects := nativeManifestTestObjects(t, strings.Repeat("a", 64), []byte("public-ca"), []byte("test-agent"))
	var deployment appsv1.Deployment
	nativeManifestTestDecode(t, objects, "Deployment", nativeNamespace, &deployment)
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 || deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("native driver must have one non-overlapping owner")
	}
	if !reflect.DeepEqual(deployment.Spec.Template.Spec.NodeSelector, map[string]string{"kubernetes.io/hostname": "kiac-demo-worker-1"}) {
		t.Fatal("native service is not restricted to its explicitly selected node")
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatal("native driver must not deploy additional host-service consumers")
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if !regexp.MustCompile(`@sha256:[a-f0-9]{64}$`).MatchString(container.Image) {
		t.Fatalf("driver container image is not digest-pinned: %s", container.Image)
	}
	if !slices.Contains(container.Args, "--mode=native-dra") || !slices.Contains(container.Args, "--native-node-name=kiac-demo-worker-1") {
		t.Fatal("driver mode does not enforce the selected native node")
	}
	var class resourceapi.DeviceClass
	nativeManifestTestDecode(t, objects, "DeviceClass", nativeDriver, &class)
	if class.Spec.ExtendedResourceName != nil {
		t.Fatal("native service must not masquerade as a GPU extended-resource device")
	}
	if len(class.Spec.Selectors) != 1 || class.Spec.Selectors[0].CEL == nil || class.Spec.Selectors[0].CEL.Expression != "device.driver == 'metal.kiac.dev'" {
		t.Fatal("native DeviceClass can select another GPU backend")
	}
	for key, raw := range objects {
		if strings.HasPrefix(key, "DaemonSet/") || strings.Contains(string(raw), `"nvidia.com/gpu"`) || strings.Contains(string(raw), `"kiac.dev/gpu"`) || strings.Contains(string(raw), `"gpu.kiac.dev"`) {
			t.Fatalf("native installation could replicate or replace existing GPU inventory: %s", key)
		}
	}
}

func TestNativeManifestConfinesHostAdminCredentialsToTheDriver(t *testing.T) {
	token := strings.Repeat("private-admin-", 5)
	ca := []byte("public-ca-only")
	objects := nativeManifestTestObjects(t, token, ca, []byte("test-agent"))
	var secret corev1.Secret
	nativeManifestTestDecode(t, objects, "Secret", nativeNamespace, &secret)
	if len(secret.Data) != 2 || string(secret.Data["admin.token"]) != token || !bytes.Equal(secret.Data["ca.crt"], ca) {
		t.Fatal("driver secret must contain only the public CA and host admin token")
	}
	var deployment appsv1.Deployment
	nativeManifestTestDecode(t, objects, "Deployment", nativeNamespace, &deployment)
	raw, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(token)) || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString([]byte(token)))) {
		t.Fatal("admin token leaked into the pod template, argv or environment")
	}
	brokerVolume := ""
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			if volume.Secret.SecretName != nativeNamespace || volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode&0o077 != 0 {
				t.Fatal("host admin secret is not mounted with private file permissions")
			}
			brokerVolume = volume.Name
		}
		if volume.HostPath != nil && (volume.HostPath.Path == "/" || strings.HasPrefix(volume.HostPath.Path, "/dev/")) {
			t.Fatal("native service driver must not inject host root or a DRM device")
		}
	}
	found := false
	for _, mount := range deployment.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.Name == brokerVolume && brokerVolume != "" {
			found = true
			if !mount.ReadOnly {
				t.Fatal("driver's mounted admin credential is writable")
			}
		}
	}
	if !found {
		t.Fatal("driver cannot read its private admin credential")
	}
}

func TestNativeManifestRefreshesTheDriverWhenCredentialsOrAgentChange(t *testing.T) {
	token, ca, agent := strings.Repeat("a", 64), []byte("public-ca"), []byte("agent-one")
	baseline := nativeManifestTestObjects(t, token, ca, agent)
	var before appsv1.Deployment
	nativeManifestTestDecode(t, baseline, "Deployment", nativeNamespace, &before)
	for name, variant := range map[string]struct {
		token     string
		ca, agent []byte
	}{
		"identical":           {token, ca, agent},
		"credential-rotation": {strings.Repeat("b", 64), ca, agent},
		"ca-rotation":         {token, []byte("replacement-ca"), agent},
		"agent-update":        {token, ca, []byte("agent-two")},
	} {
		t.Run(name, func(t *testing.T) {
			objects := nativeManifestTestObjects(t, variant.token, variant.ca, variant.agent)
			var after appsv1.Deployment
			nativeManifestTestDecode(t, objects, "Deployment", nativeNamespace, &after)
			same := reflect.DeepEqual(before.Spec.Template, after.Spec.Template)
			if same != (name == "identical") {
				t.Fatalf("pod-template refresh = %t for %s", !same, name)
			}
		})
	}
}

func TestNativeManifestRBACScopesClaimAssociationToItsOwnDriver(t *testing.T) {
	objects := nativeManifestTestObjects(t, strings.Repeat("a", 64), []byte("public-ca"), []byte("agent"))
	var role rbacv1.ClusterRole
	nativeManifestTestDecode(t, objects, "ClusterRole", nativeNamespace, &role)
	found := false
	for _, rule := range role.Rules {
		if slices.Contains(rule.Verbs, "*") || slices.Contains(rule.Resources, "*") || slices.Contains(rule.APIGroups, "*") {
			t.Fatal("native driver role grants wildcard access")
		}
		if slices.Contains(rule.Resources, "secrets") {
			t.Fatal("mounted host credential must not grant cluster-wide Secret reads")
		}
		if slices.Contains(rule.Resources, "resourceclaims/driver") {
			found = true
			if !reflect.DeepEqual(rule.ResourceNames, []string{nativeDriver}) || !slices.Contains(rule.Verbs, "associated-node:update") {
				t.Fatal("claim association can target another GPU driver")
			}
		}
	}
	if !found {
		t.Fatal("native driver lacks scoped claim association permission")
	}
	var binding rbacv1.ClusterRoleBinding
	nativeManifestTestDecode(t, objects, "ClusterRoleBinding", nativeNamespace, &binding)
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != role.Name || len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" || binding.Subjects[0].Namespace != nativeNamespace || binding.Subjects[0].Name != nativeNamespace {
		t.Fatal("native role is bound outside the driver service account")
	}
}

func TestNativeCleanupJobReusesOwnedPathsWithoutKubernetesCredentialsOrRenewal(t *testing.T) {
	objects := nativeManifestTestObjects(t, strings.Repeat("a", 64), []byte("public-ca"), []byte("agent"))
	var deployment appsv1.Deployment
	nativeManifestTestDecode(t, objects, "Deployment", nativeNamespace, &deployment)
	raw, err := nativeCleanupJob(&deployment)
	if err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	if job.Namespace != nativeNamespace || job.Labels[nativeManagedLabel] != "true" {
		t.Fatal("cleanup job is not owned by the native installation")
	}
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("offline cleanup must not receive Kubernetes API credentials")
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 0 || *job.Spec.ActiveDeadlineSeconds > 120 {
		t.Fatal("cleanup can retry or run without a bounded deadline")
	}
	if !reflect.DeepEqual(pod.NodeSelector, deployment.Spec.Template.Spec.NodeSelector) || !reflect.DeepEqual(pod.Volumes, deployment.Spec.Template.Spec.Volumes) {
		t.Fatal("cleanup changed the verified node or owned checkpoint paths")
	}
	if len(pod.Containers) != 1 || !slices.Contains(pod.Containers[0].Args, "--mode=native-cleanup") || slices.Contains(pod.Containers[0].Args, "--mode=native-dra") {
		t.Fatal("cleanup job could start a renewing native driver")
	}
	if reflect.DeepEqual(job.Spec.Template.Labels, deployment.Spec.Template.Labels) {
		t.Fatal("driver deployment could adopt its cleanup job's pod")
	}
}

func TestNativeCleanupJobRejectsUnknownDeploymentShape(t *testing.T) {
	if _, err := nativeCleanupJob(nil); err == nil {
		t.Fatal("cleanup accepted an absent installation")
	}
	objects := nativeManifestTestObjects(t, strings.Repeat("a", 64), []byte("public-ca"), []byte("agent"))
	var original appsv1.Deployment
	nativeManifestTestDecode(t, objects, "Deployment", nativeNamespace, &original)
	for name, mutate := range map[string]func(*appsv1.Deployment){
		"missing-agent": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers = nil },
		"unexpected-sidecar": func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, corev1.Container{Name: "unowned"})
		},
		"missing-native-mode": func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Args = nil },
	} {
		t.Run(name, func(t *testing.T) {
			deployment := original.DeepCopy()
			mutate(deployment)
			if _, err := nativeCleanupJob(deployment); err == nil {
				t.Fatal("cleanup copied an unexpected installation")
			}
		})
	}
}

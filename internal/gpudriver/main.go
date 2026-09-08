package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	cdspec "tags.cncf.io/container-device-interface/specs-go"
)

const (
	driverName     = "gpu.kiac.dev"
	deviceName     = "venus-0"
	resourceName   = "kiac.dev/gpu"
	memoryLabel    = "kiac.dev/gpu.memory"
	cdiDeviceID    = "kiac.dev/gpu=venus"
	defaultCDIRoot = "/etc/cdi"
)

type options struct {
	mode              string
	nodeName          string
	podUID            string
	cdiRoot           string
	registrarDir      string
	pluginsDir        string
	renderDevice      string
	optionalCard      string
	memoryOverrideMiB int
	listenAddress     string
	tlsCert           string
	tlsKey            string
}

type driver struct {
	nodeName string
	cancel   context.CancelCauseFunc
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "kiac-gpu-agent: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	opts := options{}
	flag.StringVar(&opts.mode, "mode", "dra", "agent mode: dra or webhook")
	flag.StringVar(&opts.nodeName, "node-name", os.Getenv("NODE_NAME"), "Kubernetes node name")
	flag.StringVar(&opts.podUID, "pod-uid", os.Getenv("POD_UID"), "driver pod UID for rolling updates")
	flag.StringVar(&opts.cdiRoot, "cdi-root", defaultCDIRoot, "host CDI specification directory")
	flag.StringVar(&opts.registrarDir, "registrar-directory", kubeletplugin.KubeletRegistryDir, "kubelet plugin registry directory")
	flag.StringVar(&opts.pluginsDir, "plugins-directory", kubeletplugin.KubeletPluginsDir, "kubelet plugin data directory")
	flag.StringVar(&opts.renderDevice, "render-device", "/dev/dri/renderD128", "Venus render device")
	flag.StringVar(&opts.optionalCard, "card-device", "/dev/dri/card0", "optional DRM card device")
	flag.IntVar(&opts.memoryOverrideMiB, "memory-mib", 0, "override detected GPU window in MiB (tests only)")
	flag.StringVar(&opts.listenAddress, "listen", ":9443", "webhook listen address")
	flag.StringVar(&opts.tlsCert, "tls-cert", "", "webhook TLS certificate")
	flag.StringVar(&opts.tlsKey, "tls-key", "", "webhook TLS private key")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	if opts.mode == "webhook" {
		return runWebhook(opts)
	}
	if opts.mode != "dra" {
		return fmt.Errorf("unknown --mode %q (supported: dra, webhook)", opts.mode)
	}
	if opts.nodeName == "" {
		return errors.New("--node-name is required")
	}
	if err := requireCharacterDevice(opts.renderDevice); err != nil {
		return fmt.Errorf("real Venus render device: %w", err)
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster Kubernetes credentials: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(klog.NewContext(ctx, klog.Background()))
	defer cancel(nil)

	memoryMiB := opts.memoryOverrideMiB
	if memoryMiB == 0 {
		memoryMiB, err = nodeGPUMemoryMiB(ctx, client, opts.nodeName)
		if err != nil {
			return err
		}
	}
	resources, err := driverResources(opts.nodeName, memoryMiB)
	if err != nil {
		return err
	}
	if err := writeCDISpec(opts.cdiRoot, opts.renderDevice, opts.optionalCard); err != nil {
		return fmt.Errorf("write CDI specification: %w", err)
	}
	pluginDataDir := filepath.Join(opts.pluginsDir, driverName)
	if err := os.MkdirAll(pluginDataDir, 0o750); err != nil {
		return fmt.Errorf("create kubelet plugin directory: %w", err)
	}

	drv := &driver{nodeName: opts.nodeName, cancel: cancel}
	helper, err := kubeletplugin.Start(ctx, drv,
		kubeletplugin.KubeClient(client),
		kubeletplugin.NodeName(opts.nodeName),
		kubeletplugin.DriverName(driverName),
		kubeletplugin.RegistrarDirectoryPath(opts.registrarDir),
		kubeletplugin.PluginDataDirectoryPath(pluginDataDir),
		kubeletplugin.RollingUpdate(types.UID(opts.podUID)),
		kubeletplugin.HealthService(false),
	)
	if err != nil {
		return fmt.Errorf("start kubelet DRA plugin: %w", err)
	}
	defer helper.Stop()
	if err := helper.PublishResources(ctx, resources); err != nil {
		return fmt.Errorf("publish GPU resources: %w", err)
	}

	klog.FromContext(ctx).Info("published real Apple GPU", "node", opts.nodeName, "driver", driverName, "memoryMiB", memoryMiB)
	<-ctx.Done()
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

func nodeGPUMemoryMiB(ctx context.Context, client kubernetes.Interface, nodeName string) (int, error) {
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("read GPU node %s: %w", nodeName, err)
	}
	raw := node.Labels[memoryLabel]
	memoryMiB, err := strconv.Atoi(raw)
	if err != nil || memoryMiB <= 0 {
		return 0, fmt.Errorf("node %s has invalid %s label %q", nodeName, memoryLabel, raw)
	}
	return memoryMiB, nil
}

func driverResources(nodeName string, memoryMiB int) (resourceslice.DriverResources, error) {
	wholeGiB := memoryMiB / 1024
	if wholeGiB < 1 {
		return resourceslice.DriverResources{}, fmt.Errorf("GPU memory window %d MiB is smaller than 1 GiB", memoryMiB)
	}
	memory := resource.MustParse(fmt.Sprintf("%dGi", wholeGiB))
	oneGiB := resource.MustParse("1Gi")
	defaultMemory := memory.DeepCopy()
	maxMemory := memory.DeepCopy()
	allowMultiple := true
	product := "apple-silicon"
	api := "venus"
	actualMemory := int64(memoryMiB)

	device := resourceapi.Device{
		Name: deviceName,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"product":   {StringValue: &product},
			"api":       {StringValue: &api},
			"memoryMiB": {IntValue: &actualMemory},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {
				Value: memory,
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: &defaultMemory,
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min:  &oneGiB,
						Step: &oneGiB,
						Max:  &maxMemory,
					},
				},
			},
		},
		AllowMultipleAllocations: &allowMultiple,
	}
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {Slices: []resourceslice.Slice{{Devices: []resourceapi.Device{device}}}},
		},
	}, nil
}

func requireCharacterDevice(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("%s is not a character device", path)
	}
	return nil
}

func writeCDISpec(root, renderDevice, optionalCard string) error {
	nodes := []*cdspec.DeviceNode{{
		Path: renderDevice, HostPath: renderDevice, Type: "c", Permissions: "rw",
	}}
	if optionalCard != "" {
		if info, err := os.Stat(optionalCard); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			nodes = append(nodes, &cdspec.DeviceNode{
				Path: optionalCard, HostPath: optionalCard, Type: "c", Permissions: "rw",
			})
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	spec := cdspec.Spec{
		Version: "0.6.0",
		Kind:    resourceName,
		Devices: []cdspec.Device{{
			Name: "venus",
			ContainerEdits: cdspec.ContainerEdits{
				DeviceNodes: nodes,
				Env: []string{
					"KIAC_GPU_API=venus",
					"KIAC_GPU_PRODUCT=apple-silicon",
				},
			},
		}},
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".kiac-gpu-cdi-*")
	if err != nil {
		return err
	}
	temporary := tmp.Name()
	defer os.Remove(temporary)
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(spec); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(root, "kiac-gpu.json"))
}

func (d *driver) PrepareResourceClaims(_ context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	results := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		prepared := kubeletplugin.PrepareResult{}
		if claim.Status.Allocation == nil {
			prepared.Err = errors.New("resource claim is not allocated")
			results[claim.UID] = prepared
			continue
		}
		for _, allocation := range claim.Status.Allocation.Devices.Results {
			if allocation.Driver != driverName {
				continue
			}
			if allocation.Pool != d.nodeName || allocation.Device != deviceName {
				prepared.Err = fmt.Errorf("unknown GPU allocation %s/%s", allocation.Pool, allocation.Device)
				prepared.Devices = nil
				break
			}
			prepared.Devices = append(prepared.Devices, kubeletplugin.Device{
				Requests:     []string{allocation.Request},
				PoolName:     allocation.Pool,
				DeviceName:   allocation.Device,
				CDIDeviceIDs: []string{cdiDeviceID},
				ShareID:      allocation.ShareID,
			})
		}
		if prepared.Err == nil && len(prepared.Devices) == 0 {
			prepared.Err = errors.New("resource claim has no allocation for gpu.kiac.dev")
		}
		results[claim.UID] = prepared
	}
	return results, nil
}

func (*driver) UnprepareResourceClaims(_ context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	results := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		results[claim.UID] = nil
	}
	return results, nil
}

func (*driver) WatchHealthStatus(context.Context, chan<- kubeletplugin.DeviceHealthReport) error {
	return kubeletplugin.ErrHealthNotSupported
}

func (d *driver) HandleError(ctx context.Context, err error, msg string) {
	utilruntime.HandleErrorWithContext(ctx, err, msg)
	if !errors.Is(err, kubeletplugin.ErrRecoverable) {
		d.cancel(fmt.Errorf("%s: %w", msg, err))
	}
}

const compatAnnotation = "kiac.dev/rewrote-gpu-resource"

type patchOperation struct {
	Operation string `json:"op"`
	Path      string `json:"path"`
	Value     any    `json:"value,omitempty"`
}

func runWebhook(opts options) error {
	if opts.tlsCert == "" || opts.tlsKey == "" {
		return errors.New("--tls-cert and --tls-key are required in webhook mode")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/mutate", serveMutation)
	server := &http.Server{
		Addr:              opts.listenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServeTLS(opts.tlsCert, opts.tlsKey)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func serveMutation(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if request.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, request.Body, 2<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(w, "invalid AdmissionReview", http.StatusBadRequest)
		return
	}
	response := mutateAdmission(review.Request)
	response.UID = review.Request.UID
	out := admissionv1.AdmissionReview{
		TypeMeta: review.TypeMeta,
		Response: response,
	}
	if out.APIVersion == "" {
		out.APIVersion = "admission.k8s.io/v1"
		out.Kind = "AdmissionReview"
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func mutateAdmission(request *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	response := &admissionv1.AdmissionResponse{Allowed: true}
	if request.Operation != admissionv1.Create || request.Kind.Group != "" || request.Kind.Kind != "Pod" {
		return response
	}
	var pod corev1.Pod
	if err := json.Unmarshal(request.Object.Raw, &pod); err != nil {
		return deniedResponse(fmt.Sprintf("decode Pod: %v", err))
	}
	patches, rewritten, err := nvidiaCompatibilityPatches(&pod)
	if err != nil {
		return deniedResponse(err.Error())
	}
	if len(rewritten) == 0 {
		return response
	}
	patch, err := json.Marshal(patches)
	if err != nil {
		return deniedResponse(fmt.Sprintf("encode mutation patch: %v", err))
	}
	patchType := admissionv1.PatchTypeJSONPatch
	response.PatchType = &patchType
	response.Patch = patch
	response.Warnings = []string{
		"Kiac mapped NVIDIA resource names to a real Apple Venus GPU; CUDA and NVML are unavailable, so use a Vulkan-capable image",
	}
	return response
}

func deniedResponse(message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result:  &metav1.Status{Status: metav1.StatusFailure, Message: message, Code: http.StatusUnprocessableEntity},
	}
}

func nvidiaCompatibilityPatches(pod *corev1.Pod) ([]patchOperation, []string, error) {
	patches := []patchOperation{}
	rewrittenSet := map[string]bool{}
	for i := range pod.Spec.InitContainers {
		base := fmt.Sprintf("/spec/initContainers/%d/resources", i)
		if err := rewriteRequirements(&pod.Spec.InitContainers[i].Resources, base, &patches, rewrittenSet); err != nil {
			return nil, nil, fmt.Errorf("init container %s: %w", pod.Spec.InitContainers[i].Name, err)
		}
	}
	for i := range pod.Spec.Containers {
		base := fmt.Sprintf("/spec/containers/%d/resources", i)
		if err := rewriteRequirements(&pod.Spec.Containers[i].Resources, base, &patches, rewrittenSet); err != nil {
			return nil, nil, fmt.Errorf("container %s: %w", pod.Spec.Containers[i].Name, err)
		}
	}
	if len(rewrittenSet) > 1 {
		aliases := make([]string, 0, len(rewrittenSet))
		for name := range rewrittenSet {
			aliases = append(aliases, name)
		}
		sort.Strings(aliases)
		return nil, nil, fmt.Errorf("cannot combine multiple NVIDIA GPU resource names: %s", strings.Join(aliases, ", "))
	}
	if len(rewrittenSet) == 0 {
		return nil, nil, nil
	}

	rewritten := make([]string, 0, len(rewrittenSet))
	for name := range rewrittenSet {
		rewritten = append(rewritten, name)
	}
	sort.Strings(rewritten)
	annotationValue := strings.Join(rewritten, ",")
	if pod.Annotations == nil {
		patches = append(patches, patchOperation{Operation: "add", Path: "/metadata/annotations", Value: map[string]string{compatAnnotation: annotationValue}})
	} else {
		patches = append(patches, patchOperation{Operation: "add", Path: "/metadata/annotations/" + jsonPointerToken(compatAnnotation), Value: annotationValue})
	}

	if !toleratesGPUNode(pod.Spec.Tolerations) {
		toleration := corev1.Toleration{Key: resourceName, Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}
		if pod.Spec.Tolerations == nil {
			patches = append(patches, patchOperation{Operation: "add", Path: "/spec/tolerations", Value: []corev1.Toleration{toleration}})
		} else {
			patches = append(patches, patchOperation{Operation: "add", Path: "/spec/tolerations/-", Value: toleration})
		}
	}
	return patches, rewritten, nil
}

func rewriteRequirements(requirements *corev1.ResourceRequirements, base string, patches *[]patchOperation, rewritten map[string]bool) error {
	for _, entry := range []struct {
		name string
		list corev1.ResourceList
	}{{"limits", requirements.Limits}, {"requests", requirements.Requests}} {
		keys := make([]string, 0, len(entry.list))
		nvidiaAliases := 0
		for name := range entry.list {
			keys = append(keys, string(name))
			if name == "nvidia.com/gpu" || strings.HasPrefix(string(name), "nvidia.com/mig-") {
				nvidiaAliases++
			}
		}
		if nvidiaAliases > 1 {
			return fmt.Errorf("cannot combine multiple NVIDIA GPU resource names in %s", entry.name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			if name != "nvidia.com/gpu" && !strings.HasPrefix(name, "nvidia.com/mig-") {
				continue
			}
			quantity := entry.list[corev1.ResourceName(name)]
			if existing, found := entry.list[corev1.ResourceName(resourceName)]; found && !existing.Equal(quantity) {
				return fmt.Errorf("cannot combine %s=%s with %s=%s", name, quantity.String(), resourceName, existing.String())
			}
			delete(entry.list, corev1.ResourceName(name))
			entry.list[corev1.ResourceName(resourceName)] = quantity
			path := base + "/" + entry.name
			*patches = append(*patches,
				patchOperation{Operation: "add", Path: path + "/" + jsonPointerToken(resourceName), Value: quantity.String()},
				patchOperation{Operation: "remove", Path: path + "/" + jsonPointerToken(name)},
			)
			rewritten[name] = true
		}
	}
	return nil
}

func jsonPointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func toleratesGPUNode(tolerations []corev1.Toleration) bool {
	for _, toleration := range tolerations {
		if toleration.Key != resourceName || (toleration.Effect != "" && toleration.Effect != corev1.TaintEffectNoSchedule) {
			continue
		}
		if toleration.Operator == corev1.TolerationOpExists ||
			(toleration.Operator == corev1.TolerationOpEqual && toleration.Value == "true") {
			return true
		}
	}
	return false
}

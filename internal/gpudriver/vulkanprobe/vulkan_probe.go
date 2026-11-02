package vulkanprobe

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// These are the LP64 Vulkan 1.0 C layouts, not Go bindings generated at build
// time. Only pointers to structs cross the ABI. Handles are pointer-sized on
// the supported linux/arm64 guest. Layouts and constants come from:
// https://github.com/KhronosGroup/Vulkan-Headers/blob/v1.4.328/include/vulkan/vulkan_core.h
// Keep the explicit ABI size/offset checks in vulkan_probe_test.go in sync.
type vkInstanceCreateInfo struct {
	SType          uint32
	Next           unsafe.Pointer
	Flags          uint32
	Application    unsafe.Pointer
	LayerCount     uint32
	Layers         unsafe.Pointer
	ExtensionCount uint32
	Extensions     unsafe.Pointer
}

type vkDeviceQueueCreateInfo struct {
	SType                uint32
	Next                 unsafe.Pointer
	Flags, Family, Count uint32
	Priorities           *float32
}

type vkDeviceCreateInfo struct {
	SType             uint32
	Next              unsafe.Pointer
	Flags, QueueCount uint32
	Queues            *vkDeviceQueueCreateInfo
	LayerCount        uint32
	Layers            unsafe.Pointer
	ExtensionCount    uint32
	Extensions        unsafe.Pointer
	Features          unsafe.Pointer
}

type vkQueueFamilyProperties struct {
	Flags, Count, TimestampBits uint32
	Granularity                 [3]uint32
}

type vkPhysicalDeviceProperties struct {
	APIVersion, DriverVersion, VendorID, DeviceID, DeviceType uint32
	Name                                                      [256]byte
	PipelineCacheUUID                                         [16]byte
	// VkPhysicalDeviceLimits and VkPhysicalDeviceSparseProperties are unused.
	// The complete LP64 VkPhysicalDeviceProperties is 824 bytes. Retaining its
	// full, eight-byte-aligned tail lets the loader write the complete object.
	_               [4]byte
	LimitsAndSparse [66]uint64
}

type vkMemoryType struct{ Flags, Heap uint32 }
type vkMemoryHeap struct {
	Size  uint64
	Flags uint32
	_     uint32
}
type vkMemoryProperties struct {
	TypeCount uint32
	Types     [32]vkMemoryType
	HeapCount uint32
	Heaps     [16]vkMemoryHeap
}

type vkBufferCreateInfo struct {
	SType                       uint32
	Next                        unsafe.Pointer
	Flags                       uint32
	Size                        uint64
	Usage, Sharing, FamilyCount uint32
	Families                    *uint32
}
type vkMemoryRequirements struct {
	Size, Alignment uint64
	Types           uint32
	_               uint32
}
type vkMemoryAllocateInfo struct {
	SType uint32
	Next  unsafe.Pointer
	Size  uint64
	Type  uint32
}
type vkCommandPoolCreateInfo struct {
	SType         uint32
	Next          unsafe.Pointer
	Flags, Family uint32
}
type vkCommandBufferAllocateInfo struct {
	SType        uint32
	Next         unsafe.Pointer
	Pool         uintptr
	Level, Count uint32
}
type vkCommandBufferBeginInfo struct {
	SType       uint32
	Next        unsafe.Pointer
	Flags       uint32
	Inheritance unsafe.Pointer
}
type vkFenceCreateInfo struct {
	SType uint32
	Next  unsafe.Pointer
	Flags uint32
}
type vkMemoryBarrier struct {
	SType               uint32
	Next                unsafe.Pointer
	Source, Destination uint32
}
type vkSubmitInfo struct {
	SType                      uint32
	Next                       unsafe.Pointer
	WaitCount                  uint32
	WaitSemaphores, WaitStages unsafe.Pointer
	CommandCount               uint32
	Commands                   *uintptr
	SignalCount                uint32
	SignalSemaphores           unsafe.Pointer
}

const (
	vkSuccess               int32  = 0
	vkTimeout               int32  = 2
	vkDeviceLost            int32  = -4
	vulkanProbeBytes               = 64
	vulkanProbeSentinel     uint32 = 0x6b696163
	vulkanProbeFenceTimeout        = 2 * time.Second
)

type vulkanAPI struct {
	// A failed idle wait cannot prove that submitted commands stopped. In
	// that case leave their native ownership to probe-process teardown.
	unsafeInFlight        bool
	createInstance        func(*vkInstanceCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyInstance       func(uintptr, unsafe.Pointer)
	enumerateDevices      func(uintptr, *uint32, *uintptr) int32
	getProperties         func(uintptr, *vkPhysicalDeviceProperties)
	getQueueFamilies      func(uintptr, *uint32, *vkQueueFamilyProperties)
	getMemoryProperties   func(uintptr, *vkMemoryProperties)
	createDevice          func(uintptr, *vkDeviceCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyDevice         func(uintptr, unsafe.Pointer)
	getDeviceQueue        func(uintptr, uint32, uint32, *uintptr)
	createBuffer          func(uintptr, *vkBufferCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyBuffer         func(uintptr, uintptr, unsafe.Pointer)
	getBufferRequirements func(uintptr, uintptr, *vkMemoryRequirements)
	allocateMemory        func(uintptr, *vkMemoryAllocateInfo, unsafe.Pointer, *uintptr) int32
	freeMemory            func(uintptr, uintptr, unsafe.Pointer)
	bindBufferMemory      func(uintptr, uintptr, uintptr, uint64) int32
	mapMemory             func(uintptr, uintptr, uint64, uint64, uint32, *unsafe.Pointer) int32
	unmapMemory           func(uintptr, uintptr)
	createCommandPool     func(uintptr, *vkCommandPoolCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyCommandPool    func(uintptr, uintptr, unsafe.Pointer)
	allocateCommands      func(uintptr, *vkCommandBufferAllocateInfo, *uintptr) int32
	beginCommand          func(uintptr, *vkCommandBufferBeginInfo) int32
	fillBuffer            func(uintptr, uintptr, uint64, uint64, uint32)
	pipelineBarrier       func(uintptr, uint32, uint32, uint32, uint32, *vkMemoryBarrier, uint32, unsafe.Pointer, uint32, unsafe.Pointer)
	endCommand            func(uintptr) int32
	createFence           func(uintptr, *vkFenceCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyFence          func(uintptr, uintptr, unsafe.Pointer)
	queueSubmit           func(uintptr, uint32, *vkSubmitInfo, uintptr) int32
	waitForFences         func(uintptr, uint32, *uintptr, uint32, uint64) int32
	deviceWaitIdle        func(uintptr) int32
}

// Run must run in the bounded probe subprocess, never in the driver
// monitor itself: a native loader/driver call (including error cleanup) can
// hang despite a Go context. The parent watchdog is the final deadline.
// Success proves Venus command submission, a transfer and host readback; it
// does not establish shader execution, performance or memory isolation.
func Run(ctx context.Context) error {
	if runtime.GOOS != "linux" || unsafe.Sizeof(uintptr(0)) != 8 {
		return fmt.Errorf("Vulkan transfer probe requires a 64-bit Linux guest")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	library, err := purego.Dlopen("libvulkan.so.1", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return fmt.Errorf("load Vulkan loader: %w", err)
	}
	var api *vulkanAPI
	defer func() {
		if api == nil || !api.unsafeInFlight {
			_ = purego.Dlclose(library)
		}
	}()
	api, err = loadVulkanAPI(library)
	if err != nil {
		return err
	}
	return probeVulkan(ctx, api)
}

func loadVulkanAPI(library uintptr) (*vulkanAPI, error) {
	v := &vulkanAPI{}
	for _, entry := range []struct {
		name   string
		target any
	}{
		{"vkCreateInstance", &v.createInstance}, {"vkDestroyInstance", &v.destroyInstance},
		{"vkEnumeratePhysicalDevices", &v.enumerateDevices}, {"vkGetPhysicalDeviceProperties", &v.getProperties},
		{"vkGetPhysicalDeviceQueueFamilyProperties", &v.getQueueFamilies}, {"vkGetPhysicalDeviceMemoryProperties", &v.getMemoryProperties},
		{"vkCreateDevice", &v.createDevice}, {"vkDestroyDevice", &v.destroyDevice}, {"vkGetDeviceQueue", &v.getDeviceQueue},
		{"vkCreateBuffer", &v.createBuffer}, {"vkDestroyBuffer", &v.destroyBuffer}, {"vkGetBufferMemoryRequirements", &v.getBufferRequirements},
		{"vkAllocateMemory", &v.allocateMemory}, {"vkFreeMemory", &v.freeMemory}, {"vkBindBufferMemory", &v.bindBufferMemory},
		{"vkMapMemory", &v.mapMemory}, {"vkUnmapMemory", &v.unmapMemory},
		{"vkCreateCommandPool", &v.createCommandPool}, {"vkDestroyCommandPool", &v.destroyCommandPool},
		{"vkAllocateCommandBuffers", &v.allocateCommands}, {"vkBeginCommandBuffer", &v.beginCommand},
		{"vkCmdFillBuffer", &v.fillBuffer}, {"vkCmdPipelineBarrier", &v.pipelineBarrier}, {"vkEndCommandBuffer", &v.endCommand},
		{"vkCreateFence", &v.createFence}, {"vkDestroyFence", &v.destroyFence},
		{"vkQueueSubmit", &v.queueSubmit}, {"vkWaitForFences", &v.waitForFences}, {"vkDeviceWaitIdle", &v.deviceWaitIdle},
	} {
		address, err := purego.Dlsym(library, entry.name)
		if err != nil || address == 0 {
			return nil, fmt.Errorf("load Vulkan entry point %s: %v", entry.name, err)
		}
		purego.RegisterFunc(entry.target, address)
	}
	return v, nil
}

func vkResult(operation string, result int32) error {
	if result == vkSuccess {
		return nil
	}
	return fmt.Errorf("%s failed (VkResult %d)", operation, result)
}

func probeVulkan(ctx context.Context, v *vulkanAPI) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var instance uintptr
	instanceInfo := vkInstanceCreateInfo{SType: 1}
	if err := vkResult("vkCreateInstance", v.createInstance(&instanceInfo, nil, &instance)); err != nil {
		return err
	}
	if instance == 0 {
		return fmt.Errorf("vkCreateInstance returned a null instance")
	}
	defer func() {
		if !v.unsafeInFlight {
			v.destroyInstance(instance, nil)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	var count uint32
	if err := vkResult("vkEnumeratePhysicalDevices", v.enumerateDevices(instance, &count, nil)); err != nil {
		return err
	}
	if count == 0 || count > 32 {
		return fmt.Errorf("unexpected Vulkan physical device count %d", count)
	}
	devices := make([]uintptr, count)
	if err := vkResult("vkEnumeratePhysicalDevices", v.enumerateDevices(instance, &count, &devices[0])); err != nil {
		return err
	}
	if count == 0 || count > uint32(len(devices)) {
		return fmt.Errorf("Vulkan physical device count changed unexpectedly")
	}
	var physical uintptr
	for _, device := range devices[:count] {
		if err := ctx.Err(); err != nil {
			return err
		}
		if device == 0 {
			continue
		}
		var properties vkPhysicalDeviceProperties
		v.getProperties(device, &properties)
		name := string(properties.Name[:])
		if end := strings.IndexByte(name, 0); end >= 0 {
			name = name[:end]
		}
		if properties.DeviceType != 4 && strings.HasPrefix(name, "Virtio-GPU Venus (Apple ") && strings.HasSuffix(name, ")") {
			physical = device
			break
		}
	}
	if physical == 0 {
		return fmt.Errorf("no real Apple Virtio-GPU Venus device found; software Vulkan is unsupported")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	v.getQueueFamilies(physical, &count, nil)
	if count == 0 || count > 128 {
		return fmt.Errorf("unexpected Vulkan queue family count %d", count)
	}
	families := make([]vkQueueFamilyProperties, count)
	v.getQueueFamilies(physical, &count, &families[0])
	if count == 0 || count > uint32(len(families)) {
		return fmt.Errorf("Vulkan queue family count changed unexpectedly")
	}
	family := uint32(0)
	for ; family < count; family++ {
		// Graphics or compute queues support vkCmdFillBuffer even with a 1.0
		// device. A transfer-only queue would require Vulkan 1.1 or maintenance1.
		if families[family].Count > 0 && families[family].Flags&3 != 0 {
			break
		}
	}
	if family == count {
		return fmt.Errorf("Venus has no usable Vulkan graphics/compute queue")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	priority := float32(1)
	queueInfo := vkDeviceQueueCreateInfo{SType: 2, Family: family, Count: 1, Priorities: &priority}
	deviceInfo := vkDeviceCreateInfo{SType: 3, QueueCount: 1, Queues: &queueInfo}
	var device uintptr
	var pins runtime.Pinner
	pins.Pin(&priority)
	pins.Pin(&queueInfo)
	result := v.createDevice(physical, &deviceInfo, nil, &device)
	pins.Unpin()
	runtime.KeepAlive(queueInfo)
	runtime.KeepAlive(priority)
	if err := vkResult("vkCreateDevice", result); err != nil {
		return err
	}
	if device == 0 {
		return fmt.Errorf("vkCreateDevice returned a null device")
	}
	var buffer, memory, pool, fence uintptr
	var mapped unsafe.Pointer
	mapping, submitted, completed := false, false, false
	defer func() {
		// VK_TIMEOUT does not release pending command resources. This wait is
		// intentionally inside the watchdog child; if the driver hangs, the
		// parent terminates the process rather than destroying in-flight data.
		if submitted && !completed {
			result := v.deviceWaitIdle(device)
			if result != vkSuccess && result != vkDeviceLost {
				v.unsafeInFlight = true
				return
			}
		}
		if fence != 0 {
			v.destroyFence(device, fence, nil)
		}
		if pool != 0 {
			v.destroyCommandPool(device, pool, nil)
		}
		if buffer != 0 {
			v.destroyBuffer(device, buffer, nil)
		}
		if mapping {
			v.unmapMemory(device, memory)
		}
		if memory != 0 {
			v.freeMemory(device, memory, nil)
		}
		v.destroyDevice(device, nil)
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	var queue uintptr
	v.getDeviceQueue(device, family, 0, &queue)
	if queue == 0 {
		return fmt.Errorf("vkGetDeviceQueue returned a null queue")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bufferInfo := vkBufferCreateInfo{SType: 12, Size: vulkanProbeBytes, Usage: 2}
	var created uintptr
	if err := vkResult("vkCreateBuffer", v.createBuffer(device, &bufferInfo, nil, &created)); err != nil {
		return err
	}
	buffer = created
	if buffer == 0 {
		return fmt.Errorf("vkCreateBuffer returned a null buffer")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var requirements vkMemoryRequirements
	v.getBufferRequirements(device, buffer, &requirements)
	if requirements.Size < vulkanProbeBytes || requirements.Size > 1<<20 || requirements.Types == 0 {
		return fmt.Errorf("invalid or excessive Vulkan buffer memory requirements")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var properties vkMemoryProperties
	v.getMemoryProperties(physical, &properties)
	if properties.TypeCount > 32 || properties.HeapCount > 16 {
		return fmt.Errorf("invalid Vulkan memory property counts")
	}
	memoryType := uint32(0)
	for ; memoryType < properties.TypeCount; memoryType++ {
		candidate := properties.Types[memoryType]
		if requirements.Types&(1<<memoryType) != 0 && candidate.Flags&6 == 6 && candidate.Heap < properties.HeapCount && properties.Heaps[candidate.Heap].Size >= requirements.Size {
			break
		}
	}
	if memoryType == properties.TypeCount {
		return fmt.Errorf("Venus lacks compatible host-visible coherent memory")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	allocation := vkMemoryAllocateInfo{SType: 5, Size: requirements.Size, Type: memoryType}
	created = 0
	if err := vkResult("vkAllocateMemory", v.allocateMemory(device, &allocation, nil, &created)); err != nil {
		return err
	}
	memory = created
	if memory == 0 {
		return fmt.Errorf("vkAllocateMemory returned null memory")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := vkResult("vkBindBufferMemory", v.bindBufferMemory(device, buffer, memory, 0)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := vkResult("vkMapMemory", v.mapMemory(device, memory, 0, vulkanProbeBytes, 0, &mapped)); err != nil {
		return err
	}
	mapping = true
	if mapped == nil {
		return fmt.Errorf("vkMapMemory returned a null mapping")
	}
	words := unsafe.Slice((*uint32)(mapped), vulkanProbeBytes/4)
	// Start with a different value so a no-op or stale buffer cannot pass.
	clear(words)
	if err := ctx.Err(); err != nil {
		return err
	}
	poolInfo := vkCommandPoolCreateInfo{SType: 39, Family: family}
	created = 0
	if err := vkResult("vkCreateCommandPool", v.createCommandPool(device, &poolInfo, nil, &created)); err != nil {
		return err
	}
	pool = created
	if pool == 0 {
		return fmt.Errorf("vkCreateCommandPool returned a null pool")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	commandInfo := vkCommandBufferAllocateInfo{SType: 40, Pool: pool, Count: 1}
	var command uintptr
	if err := vkResult("vkAllocateCommandBuffers", v.allocateCommands(device, &commandInfo, &command)); err != nil {
		return err
	}
	if command == 0 {
		return fmt.Errorf("vkAllocateCommandBuffers returned a null command buffer")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	begin := vkCommandBufferBeginInfo{SType: 42, Flags: 1}
	if err := vkResult("vkBeginCommandBuffer", v.beginCommand(command, &begin)); err != nil {
		return err
	}
	v.fillBuffer(command, buffer, 0, vulkanProbeBytes, vulkanProbeSentinel)
	// Transfer writes must be made available to host reads before the fence.
	barrier := vkMemoryBarrier{SType: 46, Source: 0x1000, Destination: 0x2000}
	v.pipelineBarrier(command, 0x1000, 0x4000, 0, 1, &barrier, 0, nil, 0, nil)
	if err := vkResult("vkEndCommandBuffer", v.endCommand(command)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fenceInfo := vkFenceCreateInfo{SType: 8}
	created = 0
	if err := vkResult("vkCreateFence", v.createFence(device, &fenceInfo, nil, &created)); err != nil {
		return err
	}
	fence = created
	if fence == 0 {
		return fmt.Errorf("vkCreateFence returned a null fence")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	submit := vkSubmitInfo{SType: 4, CommandCount: 1, Commands: &command}
	pins.Pin(&command)
	result = v.queueSubmit(queue, 1, &submit, fence)
	pins.Unpin()
	runtime.KeepAlive(command)
	if err := vkResult("vkQueueSubmit", result); err != nil {
		return err
	}
	submitted = true
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := vulkanProbeFenceTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	if timeout <= 0 {
		return context.DeadlineExceeded
	}
	result = v.waitForFences(device, 1, &fence, 1, uint64(timeout))
	completed = result == vkSuccess || result == vkDeviceLost
	if result == vkTimeout {
		return fmt.Errorf("Vulkan transfer fence timed out after %s", timeout)
	}
	if err := vkResult("vkWaitForFences", result); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, value := range words {
		if value != vulkanProbeSentinel {
			return fmt.Errorf("Vulkan transfer readback mismatch at word %d", i)
		}
	}
	runtime.KeepAlive(mapped)
	return nil
}

package vulkanprobe

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// LP64 sizes/offsets independently checked against the Khronos C header. These
// catch an ABI regression even when the fake entry points below still compile.
func TestVulkanProbeABI(t *testing.T) {
	checks := []struct {
		name      string
		got, want uintptr
	}{
		{"pointer", unsafe.Sizeof(uintptr(0)), 8},
		{"instance", unsafe.Sizeof(vkInstanceCreateInfo{}), 64},
		{"instance.application", unsafe.Offsetof(vkInstanceCreateInfo{}.Application), 24},
		{"queue create", unsafe.Sizeof(vkDeviceQueueCreateInfo{}), 40},
		{"queue priorities", unsafe.Offsetof(vkDeviceQueueCreateInfo{}.Priorities), 32},
		{"device create", unsafe.Sizeof(vkDeviceCreateInfo{}), 72},
		{"device queues", unsafe.Offsetof(vkDeviceCreateInfo{}.Queues), 24},
		{"physical properties", unsafe.Sizeof(vkPhysicalDeviceProperties{}), 824},
		{"physical name", unsafe.Offsetof(vkPhysicalDeviceProperties{}.Name), 20},
		{"physical limits", unsafe.Offsetof(vkPhysicalDeviceProperties{}.LimitsAndSparse), 296},
		{"queue properties", unsafe.Sizeof(vkQueueFamilyProperties{}), 24},
		{"memory properties", unsafe.Sizeof(vkMemoryProperties{}), 520},
		{"memory heaps", unsafe.Offsetof(vkMemoryProperties{}.Heaps), 264},
		{"buffer create", unsafe.Sizeof(vkBufferCreateInfo{}), 56},
		{"buffer size", unsafe.Offsetof(vkBufferCreateInfo{}.Size), 24},
		{"buffer families", unsafe.Offsetof(vkBufferCreateInfo{}.Families), 48},
		{"memory requirements", unsafe.Sizeof(vkMemoryRequirements{}), 24},
		{"memory allocate", unsafe.Sizeof(vkMemoryAllocateInfo{}), 32},
		{"command pool", unsafe.Sizeof(vkCommandPoolCreateInfo{}), 24},
		{"command allocate", unsafe.Sizeof(vkCommandBufferAllocateInfo{}), 32},
		{"command begin", unsafe.Sizeof(vkCommandBufferBeginInfo{}), 32},
		{"fence create", unsafe.Sizeof(vkFenceCreateInfo{}), 24},
		{"memory barrier", unsafe.Sizeof(vkMemoryBarrier{}), 24},
		{"submit", unsafe.Sizeof(vkSubmitInfo{}), 72},
		{"submit commands", unsafe.Offsetof(vkSubmitInfo{}.Commands), 48},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s size/offset = %d, want %d", check.name, check.got, check.want)
		}
	}
}

type fakeVulkan struct {
	api                     vulkanAPI
	t                       *testing.T
	fail                    string
	result                  int32
	null                    string
	name                    string
	deviceType              uint32
	deviceCount, queueCount uint32
	queueFlags              uint32
	memoryFlags             uint32
	requirements            vkMemoryRequirements
	data                    [vulkanProbeBytes / 4]uint32
	fill                    uint32
	corrupt                 bool
	noWrite                 bool
	barrier                 bool
	events                  []string
	onCall                  func(string)
	fenceTimeout            uint64
	idleResult              int32
}

func newFakeVulkan(t *testing.T) *fakeVulkan {
	f := &fakeVulkan{t: t, result: -3, name: "Virtio-GPU Venus (Apple M1 Max)", deviceType: 1,
		deviceCount: 1, queueCount: 1, queueFlags: 3, memoryFlags: 6,
		requirements: vkMemoryRequirements{Size: 4096, Alignment: 4096, Types: 1}}
	call := func(name string) int32 {
		if f.onCall != nil {
			f.onCall(name)
		}
		if f.fail == name {
			return f.result
		}
		return vkSuccess
	}
	created := func(name string, out *uintptr, value uintptr) int32 {
		// Error outputs are undefined in Vulkan. Deliberately write a garbage
		// handle too: callers must never clean up a failed creation result.
		*out = value
		if f.null == name {
			*out = 0
		}
		return call(name)
	}
	destroy := func(name string, handle, want uintptr) {
		if handle != want {
			t.Errorf("%s destroyed handle %d, want %d", name, handle, want)
		}
		f.events = append(f.events, name)
	}
	f.api = vulkanAPI{
		createInstance: func(info *vkInstanceCreateInfo, allocator unsafe.Pointer, out *uintptr) int32 {
			if info.SType != 1 || info.Next != nil || allocator != nil {
				t.Error("invalid instance creation inputs")
			}
			return created("instance", out, 1)
		},
		destroyInstance: func(h uintptr, _ unsafe.Pointer) { destroy("instance", h, 1) },
		enumerateDevices: func(_ uintptr, count *uint32, out *uintptr) int32 {
			if out != nil {
				*out = 2
			}
			*count = f.deviceCount
			return call("enumerate")
		},
		getProperties: func(_ uintptr, out *vkPhysicalDeviceProperties) {
			out.DeviceType = f.deviceType
			copy(out.Name[:], f.name)
		},
		getQueueFamilies: func(_ uintptr, count *uint32, out *vkQueueFamilyProperties) {
			*count = f.queueCount
			if out != nil {
				out.Count = 1
				out.Flags = f.queueFlags
			}
		},
		getMemoryProperties: func(_ uintptr, out *vkMemoryProperties) {
			out.TypeCount = 1
			out.HeapCount = 1
			out.Types[0] = vkMemoryType{Flags: f.memoryFlags}
			out.Heaps[0].Size = 1 << 20
		},
		createDevice: func(_ uintptr, info *vkDeviceCreateInfo, _ unsafe.Pointer, out *uintptr) int32 {
			if info.SType != 3 || info.QueueCount != 1 || info.Queues == nil || info.Queues.Count != 1 || info.Queues.Priorities == nil || *info.Queues.Priorities != 1 {
				t.Error("invalid queue device creation")
			}
			return created("device", out, 3)
		},
		destroyDevice: func(h uintptr, _ unsafe.Pointer) { destroy("device", h, 3) },
		getDeviceQueue: func(_ uintptr, _, _ uint32, out *uintptr) {
			*out = 4
			if f.null == "queue" {
				*out = 0
			}
		},
		createBuffer: func(_ uintptr, info *vkBufferCreateInfo, _ unsafe.Pointer, out *uintptr) int32 {
			if info.SType != 12 || info.Size != 64 || info.Usage != 2 || info.Sharing != 0 {
				t.Error("invalid transfer buffer inputs")
			}
			return created("buffer", out, 5)
		},
		destroyBuffer:         func(_ uintptr, h uintptr, _ unsafe.Pointer) { destroy("buffer", h, 5) },
		getBufferRequirements: func(_, _ uintptr, out *vkMemoryRequirements) { *out = f.requirements },
		allocateMemory: func(_ uintptr, info *vkMemoryAllocateInfo, _ unsafe.Pointer, out *uintptr) int32 {
			if info.SType != 5 || info.Size != f.requirements.Size || info.Type != 0 {
				t.Error("invalid memory allocation")
			}
			return created("memory", out, 6)
		},
		freeMemory: func(_ uintptr, h uintptr, _ unsafe.Pointer) { destroy("memory", h, 6) },
		bindBufferMemory: func(_, _, _ uintptr, offset uint64) int32 {
			if offset != 0 {
				t.Error("unexpected memory offset")
			}
			return call("bind")
		},
		mapMemory: func(_, _ uintptr, offset, size uint64, flags uint32, out *unsafe.Pointer) int32 {
			if offset != 0 || size != 64 || flags != 0 {
				t.Error("invalid memory mapping")
			}
			*out = unsafe.Pointer(&f.data[0])
			if f.null == "map" {
				*out = nil
			}
			return call("map")
		},
		unmapMemory: func(_, _ uintptr) { f.events = append(f.events, "unmap") },
		createCommandPool: func(_ uintptr, info *vkCommandPoolCreateInfo, _ unsafe.Pointer, out *uintptr) int32 {
			if info.SType != 39 {
				t.Error("invalid command pool")
			}
			return created("pool", out, 7)
		},
		destroyCommandPool: func(_ uintptr, h uintptr, _ unsafe.Pointer) { destroy("pool", h, 7) },
		allocateCommands: func(_ uintptr, info *vkCommandBufferAllocateInfo, out *uintptr) int32 {
			if info.SType != 40 || info.Pool != 7 || info.Count != 1 || info.Level != 0 {
				t.Error("invalid command allocation")
			}
			return created("command", out, 8)
		},
		beginCommand: func(_ uintptr, info *vkCommandBufferBeginInfo) int32 {
			if info.SType != 42 || info.Flags != 1 {
				t.Error("invalid command begin")
			}
			return call("begin")
		},
		fillBuffer: func(_, buffer uintptr, offset, size uint64, value uint32) {
			if buffer != 5 || offset != 0 || size != 64 {
				t.Error("invalid fill command")
			}
			f.fill = value
		},
		pipelineBarrier: func(_ uintptr, src, dst, deps, count uint32, info *vkMemoryBarrier, buffers uint32, _ unsafe.Pointer, images uint32, _ unsafe.Pointer) {
			if src != 0x1000 || dst != 0x4000 || deps != 0 || count != 1 || info.SType != 46 || info.Source != 0x1000 || info.Destination != 0x2000 || buffers != 0 || images != 0 {
				t.Error("missing transfer-to-host dependency")
			}
			f.barrier = true
		},
		endCommand: func(_ uintptr) int32 { return call("end") },
		createFence: func(_ uintptr, info *vkFenceCreateInfo, _ unsafe.Pointer, out *uintptr) int32 {
			if info.SType != 8 || info.Flags != 0 {
				t.Error("invalid fence creation")
			}
			return created("fence", out, 9)
		},
		destroyFence: func(_ uintptr, h uintptr, _ unsafe.Pointer) { destroy("fence", h, 9) },
		queueSubmit: func(queue uintptr, count uint32, info *vkSubmitInfo, fence uintptr) int32 {
			if queue != 4 || count != 1 || info.SType != 4 || info.CommandCount != 1 || info.Commands == nil || *info.Commands != 8 || info.WaitCount != 0 || info.SignalCount != 0 || fence != 9 || !f.barrier {
				t.Error("invalid command submission")
			}
			for _, value := range f.data {
				if value != 0 {
					t.Error("readback was not initialized before submission")
				}
			}
			return call("submit")
		},
		waitForFences: func(_ uintptr, count uint32, fence *uintptr, all uint32, timeout uint64) int32 {
			f.fenceTimeout = timeout
			if count != 1 || *fence != 9 || all != 1 || timeout == 0 || timeout > uint64(2*time.Second) {
				t.Error("invalid or unbounded fence wait")
			}
			if !f.noWrite {
				for i := range f.data {
					f.data[i] = f.fill
				}
			}
			if f.corrupt {
				f.data[len(f.data)-1] ^= 1
			}
			return call("wait")
		},
		deviceWaitIdle: func(_ uintptr) int32 { f.events = append(f.events, "idle"); return f.idleResult },
	}
	return f
}

func TestVulkanProbeTransferAndCleanup(t *testing.T) {
	f := newFakeVulkan(t)
	for i := range f.data {
		f.data[i] = vulkanProbeSentinel
	}
	if err := probeVulkan(t.Context(), &f.api); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.events, []string{"fence", "pool", "buffer", "unmap", "memory", "device", "instance"}) {
		t.Fatalf("cleanup order: %v", f.events)
	}
}

func TestVulkanProbeCreationErrorsCleanOwnedResources(t *testing.T) {
	for _, tc := range []struct {
		stage string
		want  []string
	}{
		{"instance", nil},
		{"enumerate", []string{"instance"}},
		{"device", []string{"instance"}},
		{"buffer", []string{"device", "instance"}},
		{"memory", []string{"buffer", "device", "instance"}},
		{"bind", []string{"buffer", "memory", "device", "instance"}},
		{"map", []string{"buffer", "memory", "device", "instance"}},
		{"pool", []string{"buffer", "unmap", "memory", "device", "instance"}},
		{"command", []string{"pool", "buffer", "unmap", "memory", "device", "instance"}},
		{"begin", []string{"pool", "buffer", "unmap", "memory", "device", "instance"}},
		{"end", []string{"pool", "buffer", "unmap", "memory", "device", "instance"}},
		{"fence", []string{"pool", "buffer", "unmap", "memory", "device", "instance"}},
		{"submit", []string{"fence", "pool", "buffer", "unmap", "memory", "device", "instance"}},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			f := newFakeVulkan(t)
			f.fail = tc.stage
			if err := probeVulkan(t.Context(), &f.api); err == nil {
				t.Fatal("failed API was accepted")
			}
			if !reflect.DeepEqual(f.events, tc.want) {
				t.Fatalf("cleanup %v, want %v", f.events, tc.want)
			}
		})
	}
}

func TestVulkanProbeFenceFailureSafety(t *testing.T) {
	for _, result := range []int32{vkTimeout, vkDeviceLost, -13} {
		t.Run(vkResult("wait", result).Error(), func(t *testing.T) {
			f := newFakeVulkan(t)
			f.fail = "wait"
			f.result = result
			if err := probeVulkan(t.Context(), &f.api); err == nil {
				t.Fatal("failed fence was accepted")
			}
			if result != vkDeviceLost && f.events[0] != "idle" {
				t.Fatalf("pending resources destroyed without an idle wait: %v", f.events)
			}
			if result == vkDeviceLost && f.events[0] != "fence" {
				t.Fatalf("device-lost cleanup: %v", f.events)
			}
		})
	}
	f := newFakeVulkan(t)
	f.fail, f.result, f.idleResult = "wait", vkTimeout, -13
	if err := probeVulkan(t.Context(), &f.api); err == nil {
		t.Fatal("timed-out fence was accepted")
	}
	if !reflect.DeepEqual(f.events, []string{"idle"}) || !f.api.unsafeInFlight {
		t.Fatalf("unproven pending work was destroyed: %v", f.events)
	}
}

func TestVulkanProbeRejectsFalseSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fakeVulkan)
		want   string
	}{
		{"software name", func(f *fakeVulkan) { f.name = "llvmpipe (LLVM 20)" }, "no real Apple"},
		{"software type", func(f *fakeVulkan) { f.deviceType = 4 }, "no real Apple"},
		{"non Apple Venus", func(f *fakeVulkan) { f.name = "Virtio-GPU Venus (Intel)" }, "no real Apple"},
		{"no devices", func(f *fakeVulkan) { f.deviceCount = 0 }, "device count"},
		{"excess devices", func(f *fakeVulkan) { f.deviceCount = 33 }, "device count"},
		{"no queues", func(f *fakeVulkan) { f.queueCount = 0 }, "queue family count"},
		{"excess queues", func(f *fakeVulkan) { f.queueCount = 129 }, "queue family count"},
		{"no graphics or compute", func(f *fakeVulkan) { f.queueFlags = 4 }, "usable Vulkan"},
		{"non coherent memory", func(f *fakeVulkan) { f.memoryFlags = 2 }, "coherent memory"},
		{"unmatched memory", func(f *fakeVulkan) { f.requirements.Types = 2 }, "coherent memory"},
		{"large memory", func(f *fakeVulkan) { f.requirements.Size = 2 << 20 }, "excessive"},
		{"small memory", func(f *fakeVulkan) { f.requirements.Size = 32 }, "requirements"},
		{"missing GPU write", func(f *fakeVulkan) { f.noWrite = true }, "readback mismatch"},
		{"last word corrupt", func(f *fakeVulkan) { f.corrupt = true }, "word 15"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVulkan(t)
			tc.change(f)
			err := probeVulkan(t.Context(), &f.api)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestVulkanProbeRejectsNullNativeResults(t *testing.T) {
	for _, stage := range []string{"instance", "device", "queue", "buffer", "memory", "map", "pool", "command", "fence"} {
		t.Run(stage, func(t *testing.T) {
			f := newFakeVulkan(t)
			f.null = stage
			if err := probeVulkan(t.Context(), &f.api); err == nil || !strings.Contains(err.Error(), "null") {
				t.Fatalf("got %v, want null-result rejection", err)
			}
		})
	}
}

func TestVulkanProbeContextAndFenceDeadline(t *testing.T) {
	f := newFakeVulkan(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := probeVulkan(ctx, &f.api); err != context.Canceled {
		t.Fatalf("canceled context: %v", err)
	}
	if len(f.events) != 0 {
		t.Fatalf("canceled probe acquired resources: %v", f.events)
	}
	f = newFakeVulkan(t)
	ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := probeVulkan(ctx, &f.api); err != nil {
		t.Fatal(err)
	}
	if f.fenceTimeout > uint64(100*time.Millisecond) {
		t.Fatalf("fence ignored shorter context deadline: %d", f.fenceTimeout)
	}
	f = newFakeVulkan(t)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	f.onCall = func(name string) {
		if name == "submit" {
			cancel()
		}
	}
	if err := probeVulkan(ctx, &f.api); err != context.Canceled {
		t.Fatalf("mid-probe cancellation: %v", err)
	}
	if f.events[0] != "idle" {
		t.Fatalf("canceled pending submission cleanup: %v", f.events)
	}
}

import Foundation
import Metal

// One fixed, bounded operation; callers cannot supply Metal shader source.
enum ProbeError: Error { case failed(String) }

func emit(_ object: [String: Any]) throws {
    let data = try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write(Data([10]))
}

do {
    guard let device = MTLCreateSystemDefaultDevice() else {
        throw ProbeError.failed("No native Metal device")
    }
    let families = (1...11).filter {
        guard let family = MTLGPUFamily(rawValue: 1000 + $0) else { return false }
        return device.supportsFamily(family)
    }.map { "Apple\($0)" }
    let identity: [String: Any] = [
        "name": device.name,
        "unifiedMemory": device.hasUnifiedMemory,
        "supportedAppleFamilies": families,
        "recommendedWorkingSetBytes": device.recommendedMaxWorkingSetSize,
        "maxBufferBytes": device.maxBufferLength,
        "supportsRaytracingAPI": device.supportsRaytracing
    ]
    if CommandLine.arguments.dropFirst().first == "capabilities" {
        try emit(["backend": "native-metal", "device": identity])
    } else if CommandLine.arguments.dropFirst().first == "self-test" {
        let input = FileHandle.standardInput.readDataToEndOfFile()
        guard input.count <= 16384,
              let request = try JSONSerialization.jsonObject(with: input) as? [String: Any],
              Set(request.keys) == Set(["count"]),
              let number = request["count"] as? NSNumber,
              CFGetTypeID(number) != CFBooleanGetTypeID(),
              number.doubleValue == Double(number.intValue),
              (1...65536).contains(number.intValue) else {
            throw ProbeError.failed("count must be an integer from 1 to 65536")
        }
        let count = number.intValue
        let bytes = count * MemoryLayout<Float>.stride
        guard let a = device.makeBuffer(length: bytes, options: .storageModeShared),
              let b = device.makeBuffer(length: bytes, options: .storageModeShared),
              let result = device.makeBuffer(length: bytes, options: .storageModeShared),
              let queue = device.makeCommandQueue() else {
            throw ProbeError.failed("Metal resource allocation failed")
        }
        let ap = a.contents().bindMemory(to: Float.self, capacity: count)
        let bp = b.contents().bindMemory(to: Float.self, capacity: count)
        for i in 0..<count {
            ap[i] = Float(i % 1024) * 0.25
            bp[i] = Float((i * 3) % 2048) * 0.125
        }
        let source = """
        #include <metal_stdlib>
        using namespace metal;
        kernel void vector_add(device const float *a [[buffer(0)]],
                               device const float *b [[buffer(1)]],
                               device float *out [[buffer(2)]],
                               uint i [[thread_position_in_grid]]) {
            out[i] = a[i] + b[i];
        }
        """
        let library = try device.makeLibrary(source: source, options: nil)
        guard let function = library.makeFunction(name: "vector_add") else {
            throw ProbeError.failed("Metal function missing")
        }
        let pipeline = try device.makeComputePipelineState(function: function)
        guard let command = queue.makeCommandBuffer(),
              let encoder = command.makeComputeCommandEncoder() else {
            throw ProbeError.failed("Metal command allocation failed")
        }
        encoder.setComputePipelineState(pipeline)
        encoder.setBuffer(a, offset: 0, index: 0)
        encoder.setBuffer(b, offset: 0, index: 1)
        encoder.setBuffer(result, offset: 0, index: 2)
        encoder.dispatchThreads(MTLSize(width: count, height: 1, depth: 1),
            threadsPerThreadgroup: MTLSize(width: min(256, pipeline.maxTotalThreadsPerThreadgroup), height: 1, depth: 1))
        encoder.endEncoding()
        command.commit()
        command.waitUntilCompleted()
        guard command.status == .completed else {
            throw ProbeError.failed("GPU command did not complete")
        }
        let rp = result.contents().bindMemory(to: Float.self, capacity: count)
        var maximumError: Float = 0
        var checksum: Double = 0
        for i in 0..<count {
            maximumError = max(maximumError, abs(rp[i] - (ap[i] + bp[i])))
            checksum += Double(rp[i])
        }
        guard maximumError == 0 else { throw ProbeError.failed("GPU result mismatch") }
        try emit([
            "backend": "native-metal", "device": identity,
            "operation": "vector-add", "count": count,
            "verifiedElements": count, "maximumError": maximumError,
            "checksum": checksum, "commandStatus": "completed",
            "gpuMilliseconds": max(0, command.gpuEndTime - command.gpuStartTime) * 1000
        ])
    } else {
        throw ProbeError.failed("Expected capabilities or self-test operation")
    }
} catch {
    FileHandle.standardError.write(Data("Native Metal probe failed: \(error)\n".utf8))
    exit(1)
}

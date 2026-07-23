# NPU execution: how it works, and how to test it honestly

This document explains why NPUs need a different stack than GPUs, what
NexusRun does about it today, and how to measure whether an NPU is actually
worth using on a given machine.

**Current status:** NexusRun detects NPUs and schedules for them, but does not
yet execute on them. The ONNX Runtime backend returns an explicit error rather
than silently falling back to CPU — a silent fallback would make every
benchmark and capability claim meaningless.

---

## Why an NPU is not just a small GPU

A GPU and an NPU look interchangeable on a spec sheet and are not.

**GPUs run general programs.** llama.cpp compiles CUDA/Metal/Vulkan kernels that
handle dynamic shapes, growing KV caches, and arbitrary quantization schemes at
runtime. You hand it a GGUF file and it runs.

**NPUs run compiled graphs.** They are fixed-function matrix engines built for
statically-shaped, heavily-quantized (INT8/INT4) inference. Reaching one means
compiling the model ahead of time with a vendor toolchain into a vendor-specific
binary. There is no equivalent of "point it at a GGUF and go."

Three consequences follow, and they shape the whole design:

1. **There is no single NPU backend.** Each vendor ships its own execution
   provider, and the compiled artifact from one does not run on another.
2. **Autoregressive decoding is a bad fit for the hardware's strengths.** NPUs
   were designed for convolutional and encoder workloads with fixed shapes. LLM
   token generation has a growing KV cache and a shape that changes every step.
   Vendors work around this with bucketed static shapes and by splitting
   prefill from decode — which is why prefill often accelerates well on an NPU
   while decode does not.
3. **Token generation is memory-bandwidth-bound, not compute-bound.** An NPU
   sharing system RAM at ~100 GB/s cannot beat a discrete GPU with 500+ GB/s of
   dedicated bandwidth, no matter how many TOPS it advertises. TOPS ratings
   describe compute, and compute is usually not the bottleneck for decode.

**The realistic expectation:** on current hardware an NPU generally will not
beat a decent discrete GPU on tokens/sec. Where it wins is **performance per
watt**, sustained thermals, and leaving the GPU free for other work. On a laptop
on battery, or a fanless edge box, that is often the thing that matters. Optimize
for the right metric.

## The vendor landscape

| Vendor | Hardware | Linux driver | Execution provider |
|---|---|---|---|
| Intel | Core Ultra (Meteor Lake and later) | `intel_vpu` | OpenVINO |
| AMD | Ryzen AI (XDNA / XDNA2) | `amdxdna` | VitisAI / Ryzen AI |
| Qualcomm | Snapdragon X (Hexagon) | `qaic` (server parts) | QNN |
| Apple | Neural Engine (M-series) | n/a (CoreML) | CoreML |
| Rockchip | RK3588 and similar | `rocket` / `rknpu` | RKNN toolkit |

On Linux, NPUs surface through the kernel's **accel subsystem** at
`/sys/class/accel/accel*`, which is how `internal/hardware` detects them. The
bound driver name identifies the vendor. On macOS the ANE is assumed present on
Apple Silicon. On Windows, device enumeration happens through the execution
provider itself.

Check what your machine exposes:

```bash
ls -l /sys/class/accel/          # Linux: NPU device nodes
nexus doctor                     # what NexusRun makes of it
```

## Why OCI packaging matters more here than anywhere else

Because each vendor needs its own precompiled artifact, a genuinely portable
NPU unit has to carry several — an OpenVINO blob for Intel, a QNN context
binary for Qualcomm, and a GGUF for the CPU/GPU fallback path.

This is exactly the problem OCI image indexes already solve for CPU
architectures. A unit becomes a manifest index whose descriptors are selected by
accelerator instead of by `linux/amd64`, and the runtime pulls only the layer
matching the host. Every registry, mirror, signing tool, and retention policy
that already exists keeps working.

Inventing a bespoke `.nx` archive would have meant rebuilding all of that
infrastructure to get the same result. That is the single strongest argument for
the OCI-native decision.

## How to test NPU vs CPU properly

`nexus bench` reports median tokens/sec per device. Throughput alone is the
wrong metric for an NPU, though. Measure all four:

**1. Prefill vs decode, separately.** Bench reports `PROMPT t/s` (prefill) and
`EVAL tok/s` (decode). NPUs frequently win prefill and lose decode. If your
workload is long-document summarization, prefill dominates and the NPU may be a
clear win. For chat, decode dominates.

```bash
nexus bench -m ollama:llama3.1:8b --runs 5
```

**2. Power draw.** This is where NPUs justify themselves.

```bash
# Intel/AMD on Linux — RAPL energy counters, joules
cat /sys/class/powercap/intel-rapl:0/energy_uj

# macOS
sudo powermetrics --samplers cpu_power,gpu_power -i 1000 -n 10
```

Sample energy before and after a fixed-token run on each device and divide by
tokens generated. Tokens per joule is the metric that decides whether an agent
can run all day on a laptop battery.

**3. Sustained throughput, not burst.** Run for several minutes. A CPU that wins
the first 30 seconds often loses after thermal throttling, while an NPU holds a
flat line. A single short benchmark hides this entirely.

**4. Output quality parity.** NPU paths often require more aggressive
quantization (INT8 or INT4) than the GGUF you were running on CPU. A 2x speedup
that quietly degrades output is not a speedup. Compare generations on identical
prompts at temperature 0 before believing any number.

## Implementation plan

The remaining work to make NPU execution real:

1. **ONNX Runtime bindings.** Requires cgo, which conflicts with the current
   CGO-free static-binary property. The likely resolution is an optional
   `nexus-npu` helper binary the main CLI shells out to, keeping the core
   portable — the same shape as the existing llama.cpp integration.
2. **A model conversion path.** GGUF is not an NPU format. Units targeting NPUs
   need an ONNX or vendor-compiled artifact, produced at build time and stored
   as a separate OCI layer.
3. **Per-vendor execution provider selection**, driven by the detected driver.
4. **Verification on real hardware** — Intel Core Ultra and Snapdragon X are the
   two that matter most. None of this can be validated on the current
   development machine, which has no NPU.

Until those land, `nexus doctor` will keep saying `onnxruntime unavailable`, and
that is the honest answer.

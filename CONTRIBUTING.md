# Contributing

## Getting set up

You need Go 1.26+ and, to actually run models, either `llama-cli` on your PATH
or Ollama installed.

```bash
git clone https://github.com/verdictlayer/nexusrun
cd nexusrun
make build        # → bin/nexus (version-stamped)
make test
make vet
make dist         # cross-compile every platform into dist/
```

Verify your environment with `./bin/nexus doctor`. If it reports no usable
devices, install llama.cpp before filing a bug.

**[docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) is the full developer guide** —
the repo map, the fast inner loop, how to add an execution backend or a model
source, how to test hardware paths you don't own, and the release process.
Read it before your first change; read
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) if you want to know why any of it
is shaped the way it is.

## What help is most valuable

**Hardware verification is the bottleneck.** The core was developed on a
machine with no NPU and a CPU-only llama.cpp build, so no generation has ever
run on a GPU or NPU here — several paths are written but unverified. If you
have any of the following, running `nexus doctor` and `nexus bench` and opening
an issue with the output is genuinely useful:

- Intel Core Ultra (NPU via `intel_vpu`)
- AMD Ryzen AI (NPU via `amdxdna`)
- Qualcomm Snapdragon X
- Apple Silicon (Metal GPU + Neural Engine)
- A working CUDA or ROCm setup
- A Raspberry Pi or similar edge device

**Other high-value areas:** NPU execution via ONNX Runtime (see
[docs/NPU.md](docs/NPU.md)), a persistent `llama-server` backend, multi-unit
composition, and sandboxing.

## Conventions

- Standard library first. Every new dependency has to earn its place; the
  static, CGO-free binary is a feature, not an accident.
- Comments explain *why*, not *what*. If a comment restates the code, delete it.
- Errors say what to do next. `nexus doctor` exists because "no backend
  available" on its own is a useless message.
- **Never silently fall back.** If an NPU or GPU cannot be used, say so loudly.
  A quiet fallback to CPU turns every benchmark and capability claim into a lie.
  This is the project's one hard rule.
- Tests for anything involving untrusted input — archives, manifests, registry
  references. Path traversal in tar extraction is already covered; keep it that
  way.

## Pull requests

Run `make vet test` before opening one. Describe what hardware you tested on,
and be explicit about what you could not test.

# Architecture

This document explains *why* the system is shaped the way it is. For how to
work inside those decisions — building, the repo map, adding a backend,
testing — see [DEVELOPMENT.md](DEVELOPMENT.md). For the complete CLI and
manifest reference, see [REFERENCE.md](REFERENCE.md).

## Layout

```
cmd/nexus/              CLI entry point and command definitions
internal/manifest/      nexus.yaml schema, parsing, validation
internal/hardware/      CPU/GPU/NPU detection
internal/engine/        backend abstraction + capability probing
internal/unit/          OCI packaging, registry push/pull, export/import
internal/store/         content-addressed blobs, model resolution, run records
internal/bench/         cross-device throughput measurement
internal/daemon/        warm-model pool (keeps weights resident)
internal/runner/        script-unit execution + sandbox re-exec helper
internal/sandbox/       Landlock policy derivation and enforcement
internal/compose/       sequential unit pipelines
internal/server/        local web console + daemon HTTP API
```

Dependencies are deliberately few: `oras-go` (OCI), `cobra` (CLI), `yaml.v3`,
and `go-landlock` (sandboxing). Everything else is the standard library, and
CGO is off, so every build is a single static binary — verified by
cross-compiling all eight targets with `CGO_ENABLED=0`.

## Decisions worth explaining

### OCI artifacts instead of a custom format

The original spec called for a bespoke `.nx` archive. Units are OCI artifacts
instead, because a custom format would have meant reimplementing content
addressing, layer deduplication, registry protocols, authentication, mirroring,
signing, and retention — all of which already exist and are battle-tested.

The payoff is immediate: `nexus push` targets ghcr.io, Docker Hub, ECR, Harbor,
or a local zot with no server-side support for NexusRun whatsoever. It also
gives a clean answer to multi-vendor NPU artifacts (see [NPU.md](NPU.md)).

`.nx` survives as an *export format* — an OCI image layout in a tarball, for
sneakernet and air-gapped transfer. It is a container for the same content, not
a competing format.

### Models are referenced, not embedded (by default)

Embedding multi-gigabyte weights in every artifact makes units slow to build,
slow to share, and wasteful when ten units use the same base model.

A unit therefore stores a model *source*. On first run the runtime resolves it:

- `ollama:<name>` — locates the weights layer in Ollama's own blob store and
  runs it in place. No copy, no re-download.
- `hf:<org>/<repo>/<file>` — Hugging Face resolve endpoint.
- `https://…` — direct download into the content-addressed store.
- a path — used where it is.

Downloads are content-addressed and verified against `sha256` when the manifest
pins one, so the same model shared by many units costs one copy on disk.

`--seal` inverts this for air-gapped deployment: weights become OCI layers and
the artifact is fully self-contained.

### Capability probing over hardware detection

The most important design decision, and the one that came directly from
building it.

The development machine has a Quadro RTX 5000 with a working driver —
`nvidia-smi` reports it, and so does `nexus doctor`. It also has a llama.cpp
compiled without CUDA, so `llama-cli --list-devices` returns nothing and that
GPU is unreachable in practice. A tool that detected the GPU and scheduled onto
it would have promised acceleration and silently delivered CPU speeds.

So every backend implements `Probe() Capability`, reporting what it can
*actually execute on right now* — for llama.cpp, by asking `--list-devices`
what it was compiled with. Scheduling uses the intersection of detected
hardware and probed capability. `nexus doctor` prints both lists and explicitly
calls out hardware present but unusable, with the reason.

Detection answers "what is in this machine." Probing answers "what can run
today." Only the second one is safe to schedule against.

### Backends shell out rather than link

The llama.cpp backend executes `llama-cli` and parses its performance counters
instead of linking libllama. This keeps the binary CGO-free and portable, lets
users upgrade llama.cpp independently, and avoids ABI coupling to a
fast-moving C++ project. The cost is process startup per generation, which is
noise next to model load time. A long-lived `llama-server` backend is the
natural optimization when it starts to matter.

### Warm models live in a daemon, not the CLI

`llama-cli` reloads weights on every invocation. Starting a server, running one
prompt, and killing it is strictly worse — you pay the load either way and add
startup overhead. So `LlamaServer` is deliberately excluded from one-shot
scheduling and used only by `internal/daemon`.

`nexus serve` runs that pool; `nexus run` probes for it with a 300ms dial
timeout and falls back to direct execution if nothing answers. The daemon is
therefore pure upside: present, runs are fast; absent, everything still works.
Measured on a 2.2 GB model: 11.8s cold, 2.5s warm.

### Sandboxing by re-exec

Landlock policies are inherited across `exec` and cannot be dropped, so the
policy is applied in a helper process that then *becomes* the interpreter via
`syscall.Exec`. No unconfined parent survives.

One trap worth recording: `RestrictPaths()` with no rules does not test for
Landlock support, it enforces a deny-everything policy. Support detection must
query the ABI version instead. Getting this wrong sandboxed the CLI itself
before it could exec anything.

### The console is read-only

`nexus serve` exposes inspection — units, hardware, run history — plus exactly
one mutating endpoint, `POST /api/run`, which the CLI uses to reach warm models.
It binds to loopback by default. Do not expose it publicly without putting
authentication in front of it: it executes units by design.

## Execution flow

```
nexus run my-agent:0.1.0
    │
    ├─ resolve unit from OCI store        internal/unit
    ├─ unpack source layer to temp dir
    ├─ resolve model source → local path  internal/store
    │     (Ollama blob, cached download, or local file)
    ├─ detect hardware                    internal/hardware
    ├─ probe backend capability           internal/engine
    ├─ select best (backend, device)      intersection of the two
    ├─ generate, streaming to stdout
    └─ record run + output                internal/store → nexus logs / console
```

## Known gaps

- NPU execution is detected and scheduled but not implemented.
- GPU offload is implemented but unverified: the development machine's GPU is
  detected, but its llama.cpp build has no CUDA compiled in, so no generation
  has run on it.
- Composition is a sequential pipeline; there is no DAG or conditional branching.
- Sandboxing is Linux-only (Landlock). macOS and Windows have no implementation,
  and script units refuse to run there without `--no-sandbox`.
- The `tools:` manifest field is accepted but unused; MCP integration is the
  intended path.

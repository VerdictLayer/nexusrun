# Reference

Complete reference for the CLI, the manifest schema, environment
variables, on-disk layout, and the OCI artifact format.

For the developer guide see [DEVELOPMENT.md](DEVELOPMENT.md); for design
rationale see [ARCHITECTURE.md](ARCHITECTURE.md).

---

## CLI

Global: `nexus --version`, `nexus <command> --help`.

### Building and running

#### `nexus init [dir]`
Scaffold a new unit directory containing a `nexus.yaml`.

| Flag | Default | Meaning |
|---|---|---|
| `--model` | `llama3.1:8b` | Model source to scaffold with |

#### `nexus build [dir]`
Package a unit directory into an OCI artifact in the local store.

| Flag | Default | Meaning |
|---|---|---|
| `--seal` | `false` | Embed model weights as layers, for offline/air-gapped use |
| `-o, --output` | — | Also write a portable `.nx` file |

Without `--seal` the artifact records the model *source* and stays under
1 KB; weights are resolved on first run.

#### `nexus run <ref|dir>`
Run a unit on the best available hardware. A directory argument skips
packaging entirely — the fast inner loop while developing.

| Flag | Default | Meaning |
|---|---|---|
| `-p, --prompt` | — | Prompt to send |
| `--device` | auto | Force `npu`, `gpu`, or `cpu` |
| `--backend` | auto | Force `llama.cpp`, `ollama`, or `onnxruntime` |
| `-n, --max-tokens` | `256` | Maximum tokens to generate |
| `--json` | `false` | Emit a JSON result |
| `--no-daemon` | `false` | Ignore a running daemon, execute directly |
| `--no-sandbox` | `false` | Run script units unconfined (trusted units only) |

When `nexus serve` is running, `run` reaches it automatically and skips
model load; it falls back to direct execution after a 300 ms dial
timeout, so the daemon is pure upside.

#### `nexus compose <unit> <unit> [unit...]`
Chain units into a pipeline, piping each output into the next prompt.
Sequential only — there is no DAG or conditional branching.

| Flag | Default | Meaning |
|---|---|---|
| `-i, --input` | — | Input for the first unit |
| `-n, --max-tokens` | `256` | Max tokens per stage |
| `--stages` | `false` | Print every intermediate output |

### Distribution

#### `nexus push <ref> <registry-ref>`
Push to any OCI registry — ghcr.io, Docker Hub, ECR, Harbor, zot. No
NexusRun-specific server support is needed. Credentials come from
`NEXUSRUN_REGISTRY_USER` / `NEXUSRUN_REGISTRY_PASSWORD`.

#### `nexus pull <registry-ref>`
Pull a unit into the local store.

#### `nexus export <ref> <file.nx>` / `nexus import <file.nx>`
Write and read a portable OCI image-layout tarball, for sneakernet and
air-gapped transfer. `.nx` is a container for the same OCI content, not a
competing format.

#### `nexus list` / `nexus inspect <ref>`
Show locally stored units; show one unit's manifest and OCI layers.

### Diagnostics

#### `nexus doctor`
Report detected hardware and which backends can actually use it — the two
lists separately, plus hardware present but unusable and why. `--json`
available.

#### `nexus bench`
Measure real throughput on every usable device. Median of N runs.

| Flag | Default | Meaning |
|---|---|---|
| `-m, --model` | — | Model to benchmark (path or `ollama:<name>`) |
| `-p, --prompt` | — | Prompt to use |
| `--runs` | `3` | Repetitions per device (median reported) |
| `-n, --max-tokens` | `64` | Tokens per run |
| `--json` | `false` | Emit JSON |

Backends with `AcceptsModelPath: false` (Ollama) are excluded — they
choose their own device, so a per-device sweep would be meaningless.

#### `nexus models`
List models available locally, including ones Ollama already pulled.

#### `nexus logs [run-id]`
Show run history, or one run's captured output. `--json` available.

### Serving

#### `nexus serve`
Start the web console and warm-model daemon.

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `127.0.0.1:7717` | Listen address |
| `--idle-timeout` | `10m` | Release a model after this long unused |
| `--no-warm` | `false` | Console only; do not keep models resident |

---

## HTTP API

Served by `nexus serve`. Read-only except `POST /api/run`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/` | Web console |
| `GET` | `/api/hardware` | Detection report + backend capabilities |
| `GET` | `/api/units` | Locally stored units |
| `GET` | `/api/runs` | Run history |
| `GET` | `/api/runs/{id}` | One run's record and output |
| `GET` | `/api/pool` | Warm-model pool status (only when warming is on) |
| `POST` | `/api/run` | Execute a unit against a warm model |

**It binds to loopback by default. Do not expose it publicly without
authentication in front of it — `POST /api/run` executes units by
design.**

---

## Manifest schema

`nexus.yaml`, API version `nexusrun.dev/v1`.

```yaml
apiVersion: nexusrun.dev/v1        # required, must match exactly
name: research-agent               # required, lowercase alphanumeric + hyphens
version: 0.1.0                     # required
description: Summarizes documents locally
author: you
license: MIT

models:                            # required for chat units
  - id: main                       # default: "main"
    source: ollama:llama3.1:8b     # required
    sha256: ""                     # optional integrity pin
    format: gguf                   # gguf (default) | onnx
    context: 8192                  # context window, tokens
    temperature: 0.7               # sampling temperature

entrypoint:
  type: chat                       # chat (default) | script
  script: ./run.py                 # script units only
  system_prompt: |
    You are a careful research assistant.

tools: []                          # accepted but not yet consumed
capabilities: [network]            # sandbox grants; see below
hardware:
  prefer: [npu, gpu, cpu]          # ordered; first *usable* one wins
  min_memory_gb: 8
```

**Unknown fields are rejected**, so a typo fails the build instead of
silently doing nothing. The corollary is that a unit using a newly added
field will not parse on older `nexus` binaries.

### Model sources

| Scheme | Behaviour |
|---|---|
| `ollama:<name>[:<tag>]` | Reuse weights Ollama already pulled — no copy, no re-download |
| `hf:<org>/<repo>/<file>` | Hugging Face resolve endpoint, cached by digest |
| `https://…` | Direct download into the content-addressed store |
| `./path` or `/path` | Local file, used in place |

Downloads are verified against `sha256` when the manifest pins one, and
deduplicated by digest — the same model shared by ten units costs one
copy on disk.

### Capabilities

Anything not declared is denied, enforced by the kernel via Landlock.

| Capability | Grants |
|---|---|
| *(none declared)* | Read/write only the unit's own working directory |
| `network` | Outbound **TCP**. Landlock does not govern UDP, so DNS is not restricted — treat this as "cannot open TCP connections", not a complete network jail |
| `storage` | Read/write the real user home directory |

Linux only. See [SANDBOXING.md](SANDBOXING.md) for enforcement details
and limits before trusting it with genuinely hostile code.

### Hardware preference

`prefer` is an ordered list of `npu`, `gpu`, `cpu`. Selection intersects
this with detected hardware **and** probed backend capability, so a
device nothing can drive is never chosen. If nothing in the list is
usable, the first available backend's most capable device is used — and
`nexus doctor` will tell you why your preference was not honoured.

---

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `NEXUSRUN_HOME` | `~/.nexusrun` | Store root — blobs, units, logs |
| `NEXUSRUN_ADDR` | `127.0.0.1:7717` | Daemon address `run` probes for |
| `NEXUSRUN_LLAMA_CLI` | *(PATH lookup)* | Path to `llama-cli` |
| `NEXUSRUN_LLAMA_SERVER` | *(PATH lookup)* | Path to `llama-server`, used by the daemon |
| `NEXUSRUN_ONNXRUNTIME_LIB` | *(standard dirs)* | Path to the ONNX Runtime shared library |
| `NEXUSRUN_REGISTRY_USER` | falls back to `REGISTRY_USER` | Registry username |
| `NEXUSRUN_REGISTRY_PASSWORD` | falls back to `REGISTRY_PASSWORD`, then `GITHUB_TOKEN` | Registry password or token |
| `NEXUSRUN_REGISTRY_INSECURE` | unset | Set to exactly `1` to allow plain HTTP to a non-local registry |
| `OLLAMA_HOST` | `http://127.0.0.1:11434` | Ollama server to use |
| `OLLAMA_MODELS` | *(standard dirs)* | Ollama model directory to borrow from |

`llama-cli` is otherwise looked up on `PATH` as `llama-cli` or `llama`,
then in `~/.local/bin`, `/usr/local/bin`, and `/opt/homebrew/bin`.

Registry credentials come from the environment only — **Docker config
file support is deliberately not implemented**, because env vars behave
identically in CI and on a Raspberry Pi. If no username and password are
found, requests are made anonymously, which is enough for public pulls.

Registries on `localhost`, `127.0.0.1`, or `::1` automatically use plain
HTTP (local zot, `registry:2`, CI fixtures). Every other host requires
TLS unless `NEXUSRUN_REGISTRY_INSECURE=1`.

---

## On-disk layout

```
$NEXUSRUN_HOME/            default ~/.nexusrun
  blobs/sha256/<hex>       content-addressed model weights and layers
  units/                   OCI image layout for packaged units
  logs/<run-id>.json       run records (RunRecord)
  logs/<run-id>.log        captured stdout/stderr
```

Models resolved from Ollama are **not** copied here — they are executed
in place from Ollama's own blob store.

---

## OCI artifact format

A unit is a standard OCI artifact, which is why it pushes to any registry
with no server-side support.

| Constant | Value |
|---|---|
| Artifact type | `application/vnd.nexusrun.unit.v1` |
| Config | `application/vnd.nexusrun.unit.config.v1+json` |
| Source layer | `application/vnd.nexusrun.unit.source.v1.tar+gzip` |
| Model layer (sealed) | `application/vnd.nexusrun.model.v1.gguf` |

| Annotation | Meaning |
|---|---|
| `dev.nexusrun.model.source` | Where a linked (non-sealed) model came from, so a puller can fetch it |
| `dev.nexusrun.model.id` | Which manifest model a layer corresponds to |
| `dev.nexusrun.sealed` | Whether weights are embedded |

An unsealed unit has one source layer and no model layers. `--seal` adds
one model layer per model and sets the sealed annotation.

---

## Exit codes

`0` on success, `1` on any error, with the message on stderr prefixed by
`nexus:`. There are no distinguished exit codes yet — scripts should
parse `--json` output rather than branch on exit status beyond
success/failure.

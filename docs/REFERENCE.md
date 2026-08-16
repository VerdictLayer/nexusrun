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
| `--auto-model` | `false` | Require the unit to select its model by measurement |
| `--refresh-bench` | `false` | Re-measure candidates instead of trusting the cache |
| `--cache-ttl` | `168h` | How long a cached benchmark stays trusted |
| `-s, --session` | — | Continue a named conversation across runs |
| `--restore` | — | Resume from a checkpoint file |
| `--trim` | `0` | Keep only the most recent N messages of the session |
| `--max-turns` | `8` | Stop after this many tool-calling turns |
| `--mcp-debug` | `false` | Log every MCP frame in both directions |

When `nexus serve` is running, `run` reaches it automatically and skips
model load; it falls back to direct execution after a 300 ms dial
timeout, so the daemon is pure upside.

#### `nexus compose <unit> <unit> [unit...]`
Chain units into a pipeline, piping each output into the next prompt.
Sequential, no file needed.

| Flag | Default | Meaning |
|---|---|---|
| `-i, --input` | — | Input for the first unit |
| `-n, --max-tokens` | `256` | Max tokens per stage |
| `--stages` | `false` | Print every intermediate output |

#### `nexus compose up` and friends
Run a declared workflow: a dependency graph of agents with routing
conditions, payload transforms, and a shared state bus, from a
`nexus-compose.yaml`. See **[COMPOSE.md](COMPOSE.md)**.

| Subcommand | Purpose |
|---|---|
| `init [dir]` | Scaffold a `nexus-compose.yaml` |
| `validate [file]` | Check the file, run nothing |
| `up` | Run the workflow |
| `down [name]` | Stop a detached run |
| `ls` | List detached runs |
| `logs [name]` | Read the state bus |
| `build` | Package as an OCI artifact |
| `push` / `pull` | Move it through a registry |

`nexus compose up` flags:

| Flag | Default | Meaning |
|---|---|---|
| `-f, --file` | `nexus-compose.yaml` | Workflow file, directory, or stored reference |
| `-i, --input` | — | Input delivered to every source agent |
| `--input-file` | — | Read the input from a file |
| `-n, --max-tokens` | `256` | Default for agents declaring none |
| `-d, --detach` | `false` | Run in the background |
| `--only` | — | Run just these agents and their dependencies (repeatable) |
| `--stages` | `false` | Print every agent's output |
| `--json` | `false` | Emit the full result as JSON |

Agents execute one at a time in topological order. A workflow is its own
OCI artifact type, so `nexus run` refuses one by type rather than
half-executing it.

### Sessions, state, secrets, and tools

#### `nexus session list|show|remove`
An agent's continuing conversation, kept across runs. See
**[CHECKPOINT.md](CHECKPOINT.md)**.

#### `nexus checkpoint save|list|inspect|remove`
Move a session between machines as a portable `.state.nx`.

| Flag | Applies to | Meaning |
|---|---|---|
| `-o, --output` | save | Write here instead of the store (`-` for stdout) |
| `--encrypt` | save | Seal the archive under `NEXUS_STATE_KEY` |
| `--seal` | save | Embed the model weights (gigabytes) |
| `--transcript` | inspect | Also print the conversation |

The KV cache is deliberately not captured; see CHECKPOINT.md for why.

#### `nexus secret set|rotate|list|remove|check|export|import|audit`
Credentials kept out of the artifact, encrypted per machine and optionally
scoped per device. See **[SECRETS.md](SECRETS.md)**.

| Flag | Applies to | Meaning |
|---|---|---|
| `--device` | set, rotate, remove, check | Scope to one machine |
| `--file` / `--stdin` | set, rotate | Read the value from a file or stdin |
| `--expires-in` | set | Expire after a duration |

#### `nexus tools install|update|list|check`
The MCP servers a unit depends on. See **[MCP.md](MCP.md)**.

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

#### `nexus bench [unit]`
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

Given a unit whose model entry declares candidates, `bench` measures each
against the unit's own eval suite and reports which one this machine would
select — the same decision `nexus run` makes, without running the agent.
Results are cached per machine fingerprint. See **[AUTOMODEL.md](AUTOMODEL.md)**.

#### `nexus bench cache show` / `clear`, `nexus bench export`
Inspect, discard, or export this machine's benchmark cache.
`export --format` takes `json` (default) or `csv`.

#### `nexus eval <ref|dir>`
Score a unit against an eval suite and record the score against the
conditions that produced it — unit digest, model weights, backend, device,
host. See **[EVAL.md](EVAL.md)** for the suite format and the reasoning.

| Flag | Default | Meaning |
|---|---|---|
| `--suite` | the unit's `evals/` | Suite file to run |
| `--model` | the unit's own | Also score the suite against this model; repeatable. Each is recorded as an override so its score is never read as the unit's own |
| `--device` | — | Restrict to one device: `npu`, `gpu`, `cpu` |
| `--backend` | — | Restrict to one backend |
| `--repeats` | `1` | Runs per case; above 1, cases that don't pass every run are reported flaky |
| `--all-devices` | `false` | Evaluate every usable backend/device pair, not just the one the unit would pick |
| `--fail-under` | `0` | Exit non-zero if the best pass rate is below this percentage |
| `--compare` | — | Diff against a saved evaluation ID; bare flag means the latest for this unit |
| `--json` | `false` | Emit JSON (`{report, diff}`) |
| `--no-save` | `false` | Do not save the report |

Unlike `nexus bench`, Ollama **is** included: it addresses models by name,
so it is eligible whenever the unit's own model source names it
(`ollama:…`). It is reported as device `AUTO`, because it chooses its own
placement and NexusRun cannot claim otherwise. For the same reason it is
excluded when `--device` is given.

`llama-server` is preferred over `llama-cli` here, the reverse of one-shot
`nexus run`: a suite is many prompts against one model, so a warm server
pays the weight load once instead of per case.

#### `nexus eval list`
List saved evaluations, newest first. `--json` available.

#### `nexus eval diff <before-id> <after-id>`
Compare two saved evaluations case by case. States when the two are not
comparable — different suite, unit build, weights, or temperature.
`--json` available.

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

The `nexusrun.dev` prefix is the project's own domain, used as a namespace
the way Kubernetes and OCI do. It is not fetched at runtime — nothing here
touches the network to validate a manifest — it just guarantees the schema
name cannot collide with anyone else's.

```yaml
apiVersion: nexusrun.dev/v1        # required, must match exactly
name: research-agent               # required, lowercase alphanumeric + hyphens
version: 0.1.0                     # required
description: Summarizes documents locally
author: you
license: MIT

models:                            # required for chat units
  - id: main                       # default: "main"
    source: ollama:llama3.1:8b     # required, unless candidates are given
    sha256: ""                     # optional integrity pin
    format: gguf                   # gguf (default) | onnx
    context: 8192                  # context window, tokens
    temperature: 0.7               # sampling temperature

entrypoint:
  type: chat                       # chat (default) | script
  script: ./run.py                 # script units only
  system_prompt: |
    You are a careful research assistant.

tools:                             # offered to the model; see below
  - name: search_notes             # required, [A-Za-z0-9_-]{1,64}
    description: Search my notes   # required — what the model selects on
    parameters:                    # optional JSON Schema object
      type: object
      properties:
        query: {type: string}
      required: [query]
    exec:
      type: script                 # required; script is the only kind
      command: ["python3", "tools/search.py"]
    capabilities: [storage]        # subset of the unit's capabilities

secrets:                           # declared, never carried; see SECRETS.md
  - name: OPENAI_API_KEY
    required: true
  - name: SSL_CERT
    required: false
    env: CERT_PATH                 # variable to inject as; default is name
    mount_path: /etc/certs/ssl.pem # write to a file instead of a variable

config:                            # non-secret settings with defaults
  - name: MAX_RETRIES
    default: "3"
    env: MAX_RETRIES

mcp_servers:                       # external tool servers; see MCP.md
  filesystem:
    source: github:org/repo#<sha>/src/filesystem
    command: ["node", "dist/index.js", "/projects"]
    env: {ALLOWED_EXTENSIONS: ".go,.md"}
    tools: [read_file]             # narrow what is offered to the model
    timeout: 30s
    sandbox:
      allowed_paths: ["/projects"] # absolute
      network: false               # cannot exceed the unit's capabilities

capabilities: [network]            # sandbox grants; see below
hardware:
  prefer: [npu, gpu, cpu]          # ordered; first *usable* one wins
  min_memory_gb: 8
```

**Unknown fields are rejected**, so a typo fails the build instead of
silently doing nothing. The corollary is that a unit using a newly added
field will not parse on older `nexus` binaries.

### Auto-selected models

A model entry may declare *what kind* of model it needs instead of naming
one. It must be one shape or the other, never both.

```yaml
models:
  - id: main
    profile: default               # cache key; default "default"
    context: 8192
    requirements:
      min_context: 8192
      tool_calling: required       # required | preferred | none
      max_size_mb: 3000
      min_quality_score: 11        # eval cases that must pass
    candidates:
      - source: ollama:phi3:3.8b
        weight: 1.0                # tiebreaker preference, not a scale weight
        context: 4096              # optional per-candidate override
        sha256: ""
      - source: ollama:llama3.1:8b
        weight: 0.9
    selection_strategy: best_eval_score
```

`selection_strategy` is `best_eval_score` (default), `fastest_passing`, or
`smallest_passing`. The latter two require `min_quality_score` — without a
bar every candidate passes and the choice would ignore quality entirely.

See **[AUTOMODEL.md](AUTOMODEL.md)**.

---

## Workflow schema

`nexus-compose.yaml`, API version `nexusrun.dev/v1`, kind `Workflow`.
Documented in full in **[COMPOSE.md](COMPOSE.md)**.

```yaml
apiVersion: nexusrun.dev/v1
kind: Workflow
name: content-pipeline
version: 1.0.0

agents:
  researcher:
    unit: ghcr.io/acme/researcher:v1.2.0
    model: ollama:llama3.1:8b      # override the unit's own
    hardware: {prefer: [gpu, cpu]}
    depends_on: []
    env: {MAX_DEPTH: "3"}
    max_tokens: 400
    restart: 2
    max_memory_mb: 4096
    max_cpu_percent: 80

routing:
  - from: researcher
    to: writer
    condition: "len(researcher.output) > 200"
    transform: "{{ summarize 2000 .researcher.output }}"

shared_state:
  backend: file                    # memory (default) | file
  path: ./.nexus/state.jsonl
  encryption: aes256-gcm           # optional; needs NEXUS_STATE_KEY

network:
  isolation: process               # process (default) | namespace | none
  allow_loopback: true
```

`backend: sqlite` is rejected with an explanation: every pure-Go driver is
larger than this binary and the cgo ones break static cross-compilation to
32-bit ARM.

### Model sources

| Scheme | Behaviour |
|---|---|
| `ollama:<name>[:<tag>]` | Reuse weights Ollama already pulled — no copy, no re-download |
| `hf:<org>/<repo>/<file>` | Hugging Face, revision `main`. `<file>` may contain slashes |
| `hf:<org>/<repo>@<rev>/<file>` | Hugging Face at a pinned revision, branch, or tag |
| `https://…` | Direct download into the content-addressed store |
| `./path` or `/path` | Local file, used in place |

Downloads are verified against `sha256` when the manifest pins one, and
deduplicated by digest — the same model shared by ten units costs one
copy on disk. Even without a pin, a downloaded source (`hf:` or `https:`) is
remembered by digest, so it is fetched once and reused on later runs rather
than re-downloaded.

Gated or private Hugging Face repos are reachable by setting `HF_TOKEN`
(or `HUGGING_FACE_HUB_TOKEN`) in the environment.

### Capabilities

Anything not declared is denied, enforced by the kernel via Landlock.

| Capability | Grants |
|---|---|
| *(none declared)* | Read/write only the unit's own working directory |
| `network` | Outbound **TCP**. Landlock does not govern UDP, so DNS is not restricted — treat this as "cannot open TCP connections", not a complete network jail |
| `storage` | Read/write the real user home directory |

Linux only. See [SANDBOXING.md](SANDBOXING.md) for enforcement details
and limits before trusting it with genuinely hostile code.

Unknown capability names are rejected rather than ignored, so a typo cannot
read as a grant while granting nothing.

### Tools

A tool's `capabilities` must be a subset of the unit's, so the unit's
top-level list stays the complete account of what the artifact can reach.

Tool calling requires a backend with a chat-completions endpoint —
`llama-server` or Ollama, reported under "Tool calling" by `nexus doctor`.
A unit declaring tools is scheduled over that smaller candidate set, and
fails with the reason if nothing on the host qualifies.

The runtime drives the full loop: generate, execute what the model asked
for, feed the results back, generate again, bounded by `--max-turns`
(default 8). A tool that runs and refuses is returned to the model, which
usually recovers; a sandbox denial or a dead MCP server aborts the run.

Each invocation is its own process under its own Landlock policy, so a
tool's capability narrowing is enforced at execution and not merely checked
at build time.

Tools reach the model from two places it cannot distinguish: the unit's own
`tools:`, and whatever its `mcp_servers:` offer. See **[TOOLS.md](TOOLS.md)**
and **[MCP.md](MCP.md)**, including the `--jinja` requirement and why the
warm daemon refuses tool units.

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
| `NEXUS_STATE_KEY` | — | Passphrase for an encrypted workflow state bus and for `checkpoint --encrypt` |
| `NEXUS_MASTER_KEY` | *(generated file)* | Overrides the on-disk secret-store master key |
| `NEXUS_BACKUP_KEY` | — | Passphrase for `secret export` / `secret import` |
| `NEXUS_DEVICE_ID` | — | Names this machine, selecting device-scoped secrets |
| `NEXUSRUN_REGISTRY_USER` | falls back to `REGISTRY_USER` | Registry username |
| `NEXUSRUN_REGISTRY_PASSWORD` | falls back to `REGISTRY_PASSWORD`, then `GITHUB_TOKEN` | Registry password or token |
| `NEXUSRUN_REGISTRY_INSECURE` | unset | Set to exactly `1` to allow plain HTTP to a non-local registry |
| `OLLAMA_HOST` | `http://127.0.0.1:11434` | Ollama server to use |
| `OLLAMA_MODELS` | *(standard dirs)* | Ollama model directory to borrow from |
| `HF_TOKEN` | — | Hugging Face token for gated/private repos (also `HUGGING_FACE_HUB_TOKEN`) |

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
  evals/<eval-id>.json     saved evaluation reports (eval.Report)
  cache/benchmarks.json    auto-model benchmark cache, keyed by machine
  workflows/<name>.json    detached workflow sessions (pid, file, state path)
  sessions/<name>.json     agent conversations (0600)
  checkpoints/*.state.nx   saved portable state
  secrets.json             encrypted secret store (0600)
  master.key               secret-store master key (0600)
  audit.log                secret access log, by key name (0600)
  mcp/<kind>/…             fetched MCP servers, shared across units
  restored/                weights extracted from a sealed checkpoint
```

A workflow's own state bus is **not** here: it lives beside the workflow
file, at `shared_state.path`, because it belongs to that workflow rather
than to the machine.

Evaluations live beside runs rather than inside a unit because a score
belongs to a (unit, model, host) triple, not to the unit alone: the same
artifact scores differently on different machines, and both numbers are
worth keeping.

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
one model layer per model and sets the sealed annotation. Sealing a unit
whose model entry declares candidates embeds *every* candidate — the
target machine has to measure them to choose, and an air-gapped device
cannot fetch what is missing.

A workflow is a **separate** artifact type sharing the same store:

| Constant | Value |
|---|---|
| Artifact type | `application/vnd.nexusrun.workflow.v1` |
| Config | `application/vnd.nexusrun.workflow.config.v1+json` |
| Compose layer | `application/vnd.nexusrun.workflow.compose.v1+yaml` |
| `dev.nexusrun.workflow.agents` | Agent count, so a listing need not fetch the config |

Its config decodes into a unit manifest well enough to look plausible —
same `name`, `version`, and `description` keys — so `nexus run` refuses it
by artifact type rather than executing it as a unit with no models. Only
the workflow file is packed; the units it names resolve from the registry
at run time.

---

## Exit codes

`0` on success, `1` on any error, with the message on stderr prefixed by
`nexus:`. There are no distinguished exit codes yet — scripts should
parse `--json` output rather than branch on exit status beyond
success/failure.

# NexusRun

**Portable AI units — build once, run on whatever hardware you land on.**

[nexusrun.dev](https://nexusrun.dev) · [Quickstart](docs/QUICKSTART.md) · [Reference](docs/REFERENCE.md)

NexusRun packages an AI agent (its model, prompts, tools, and config) into a
standard **OCI artifact**, then runs it on the fastest accelerator the host can
actually use. No containers, no Python environment, no daemon. One static
binary, about 11 MB.

```bash
nexus init my-agent              # scaffold a unit
nexus build my-agent             # package it as an OCI artifact
nexus run my-agent:0.1.0 -p "hi" # run on the best available device
nexus push my-agent:0.1.0 ghcr.io/you/my-agent:0.1.0
```

---

## Why this exists

Ollama and llama.cpp solved *running a model*. They deliberately stop below the
agent layer: a model plus its system prompt, its tools, its context settings,
and its hardware requirements is not a shareable thing today. People paste
README instructions at each other instead.

Docker's `cagent` packages agents as OCI artifacts, which is the right idea, but
it assumes Docker — a non-starter on a Raspberry Pi, a phone, a locked-down
enterprise laptop, or an air-gapped box.

NexusRun takes the OCI packaging idea and removes the container requirement.

### Four things it does that existing tools don't

**1. Reuses models you already have.** A unit references a model rather than
embedding it. If you already pulled it with Ollama, NexusRun runs that exact
blob from Ollama's store — no second download, no second copy on disk.

```yaml
models:
  - source: ollama:llama3.1:8b   # already on disk? just use it
```

A typical unit is **under 1 KB** because it is metadata, not weights.

**2. Sealed units for air-gapped machines.** When you do need self-containment,
`nexus build --seal` embeds the weights as OCI layers. For a clean supported
host, `nexus bundle` adds the NexusRun executable and a pinned, verified
llama.cpp runtime so no separate engine installation or network access is
needed. A sealed `.nx` by itself remains weights plus agent content.

**3. It tells you the truth about your hardware.** Detecting a GPU is not the
same as being able to use it. See below — this is the interesting part.

**4. It scores the agent, per backend and device.** A local agent's answers
change with the quantization of its weights and the backend that runs them,
not just with its prompt. `nexus eval` runs a suite of cases and reports the
pass rate keyed to the unit digest, the model digest, and the target it ran
on — because on mixed hardware that is the only form of the number that
means anything.

### How it compares

|  | NexusRun | Ollama | Docker `cagent` | llama.cpp |
|---|---|---|---|---|
| Packages the **agent**, not just the model | ✅ | ❌ model only | ✅ | ❌ |
| Runs **without a container runtime** | ✅ | ✅ | ❌ needs Docker | ✅ |
| Ships to **any OCI registry** | ✅ | partial (own registry) | ✅ | ❌ |
| **Reuses** models already on disk | ✅ Ollama's blobs | n/a | ❌ | manual |
| **Air-gapped** single-file transfer | ✅ `.nx` export | ❌ | ❌ | manual |
| Reports **backend capability**, not just detection | ✅ | ❌ | ❌ | ❌ |
| **Scores agent quality** per backend and device | ✅ | ❌ | ❌ | ❌ |
| Runs the model itself | ❌ delegates | ✅ | ❌ delegates | ✅ |

NexusRun is a layer *above* the inference engine, not a replacement for one.
It delegates generation to llama.cpp or Ollama and concerns itself with
packaging, distribution, and honest hardware scheduling.

### Complete offline deployments

A sealed `.nx` contains the agent and model weights, but intentionally remains
separate from platform-specific execution code. For a clean supported host,
NexusRun can now package both layers into one deployment bundle:

```bash
nexus runtime import ./llama-linux-amd64.runtime.tar.gz
nexus bundle my-agent:0.1.0 \
  --target linux/amd64 \
  --runtime 'llama.cpp/<version>/linux/amd64/<digest>' \
  --nexus ./dist/nexus-linux-amd64 \
  -o my-agent-linux-amd64.tar.gz
```

The resulting archive contains the NexusRun executable, exact verified
llama.cpp runtime, sealed `.nx` agent, licenses, bundle manifest, and a
relocatable `run` launcher. The launcher verifies the runtime before execution,
uses a bundle-local writable state directory, imports the agent idempotently,
and runs inference through the managed loopback-only llama-server lifecycle.
It does not require root, modify PATH, or download an engine or model.

Runtime imports are bounded, staged, integrity-checked, installed atomically,
and addressed by engine version, target, and payload digest. Local import is an
explicit trust decision: SHA-256 proves the bytes did not change, not who
published them. See [Managed runtimes and offline bundles](docs/RUNTIME_PACKAGES.md)
for the package schema, build procedure, supported first profile, and clean-host
acceptance test.

### Should you use it?

**Good fit if you** want to hand someone a complete agent rather than setup
instructions; need to run on an air-gapped box or a locked-down Linux machine
where Docker is not an option; are deploying the same agent
across mixed hardware and need to know what each host can actually do; or want
untrusted agent scripts confined by the kernel.

**Poor fit if you** just want to chat with a model locally (use Ollama — this
sits on top of it and adds nothing for that), or need NPU execution (NPUs are
detected and reported; see [docs/NPU.md](docs/NPU.md)).

---

## Hardware: detection is not capability

Most tools check whether a GPU or NPU exists and assume it works. In practice
it often doesn't: llama.cpp may have been compiled CPU-only, a driver may be
half-upgraded, or an NPU may have no execution provider installed.

`nexus doctor` reports these separately. Real output from the development
machine, which has a healthy Quadro RTX 5000 in it:

```
Detected hardware
  linux/amd64 · Intel(R) Xeon(R) W-10885M CPU @ 2.40GHz (16 cores, avx2/avx/sse42) · 125.6 GB RAM
    [GPU] nvidia Quadro RTX 5000 (16.0 GB) → llama.cpp/cuda
    [CPU] intel Intel(R) Xeon(R) W-10885M CPU @ 2.40GHz → llama.cpp/cpu

Backend capability
  llama.cpp        ready        CPU-only build (no GPU backend compiled in)
                                version 5688 (860a9e4e)
  llama.cpp/server unavailable  llama-server not found (set NEXUSRUN_LLAMA_SERVER to enable warm models)
  ollama           ready        server reachable; runs ollama: models warm, picks its own device
                                version 0.10.1
  onnxruntime      unavailable  onnxruntime shared library not found (needed for NPU execution)

Sandboxing:     Landlock ABI v8 (filesystem + network)
Usable devices: GPU, CPU
```

That GPU is real, the driver works, and `nvidia-smi` reports it happily — and
llama.cpp **still cannot touch it**, because this build was compiled without
CUDA. `llama-cli --list-devices` returns an empty list, so llama.cpp is pinned
to CPU no matter what the hardware report says.

This is the entire argument in one screen. A tool that checked `lspci` or
`nvidia-smi` and stopped there would have promised GPU acceleration and then
silently delivered CPU speeds. GPU appears under "usable" here *only* because
Ollama is running and does its own device placement — a different backend with
a different answer, reported separately rather than blended into one
optimistic number.

Scheduling uses the **intersection** of detected hardware and probed backend
capability, per backend. A device nothing can drive is never selected.

### Measuring instead of guessing

`nexus bench` runs the same prompt on every usable device and reports median
throughput, so "is the NPU actually faster here?" is an empirical question:

```
  DEVICE   BACKEND        EVAL tok/s PROMPT t/s   LOAD ms
  ────────────────────────────────────────────────────────
  CPU      llama.cpp           14.43      52.97      1039

Fastest: llama.cpp/cpu
```

Accelerators are not automatically faster. For small models the transfer
overhead can exceed the compute win, and NPUs are usually optimized for
sustained low power rather than peak throughput. Measure per host.

See **[docs/NPU.md](docs/NPU.md)** for how NPU execution works, why it needs a
different stack than GPUs, and how to test it.

### The same argument, applied to quality

Throughput is the easy half. The harder question is whether the agent still
*works* on a given host, and it has the same shape: one number hides the
answer. Same unit, same model digest, one machine, two backends:

```
Suite:    code-reviewer-basics (13 cases × 1 repeat(s))
Unit:     code-reviewer:0.1.0
Model:    ollama:phi3:latest · Q4_0 · 633fc5be925f
Sampling: temperature 0.00 · max 128 tokens

  DEVICE   BACKEND              PASS     RATE   FLAKY       tok/s
  ───────────────────────────────────────────────────────────────
  CPU      llama.cpp/server    12/13    92.3%       0       13.57
  AUTO     ollama              10/13    76.9%       0       86.99

Best: llama.cpp/server/cpu at 92.3%
```

The faster path is the worse one — **6.4× the throughput, 15 points below** on
quality, with identical weights and temperature 0 on both. Picking a backend
on `nexus bench` alone would have picked the one that answers worse. The
failures say why: Ollama misses the same bug llama-server does, then adds two
of its own by writing paragraphs about correct code where the unit was told to
answer in one line.

### And the same question about the model

`--model` scores one suite against several models, which answers "how much
model does this job actually need?" Same host, temperature 0, four cases:

```
  MODEL                          PARAMS  QUANT         SIZE     PASS    RATE     tok/s
  ────────────────────────────────────────────────────────────────────────────────────
  ollama:llama3.1:8b             8.0B    Q4_K_M      4.6 GB    13/13    100%       6.3
  ollama:phi3:latest *           3.8B    Q4_0        2.0 GB    12/13     92%      14.0
  ollama:codeqwen:latest         7.3B    Q4_0        3.9 GB    12/13     92%       7.7
  ollama:deepseek-coder:latest   1B      Q4_0        740 MB    10/13     77%      38.1
  ollama:starcoder2:3b           3B      Q4_0        1.6 GB     3/13     23%      16.4
```

**2.0 GB scores what 3.9 GB scores, 1.8× faster.** And `starcoder2:3b` is more
than twice the size of the 740 MB model while scoring a third as well, because
it is a base model that was never taught to follow a system prompt: **size is
not the axis, training is**. No leaderboard tells you that about *your* agent.

The suite has 13 cases for a reason. An earlier 4-case version of it reported
the 740 MB model tying `phi3` at 75% each — a finding that did not survive
nine more cases, where they separate to 77% and 92%. A suite too small to
distinguish two models will happily tell you they are equivalent, which is
worth knowing before you cite one.

Suites live in `evals/` inside the unit, so they are packed into the artifact
and travel with it: whoever pulls the unit can rerun the evaluation instead
of trusting a published number. `--compare` diffs against the last run, and
`--fail-under` turns a suite into a release gate.

Details, the suite format, and what the numbers do *not* cover:
**[docs/EVAL.md](docs/EVAL.md)**.

## Running untrusted units

Script units execute arbitrary code, so they run under Landlock with only the
capabilities they declare. A unit that declares nothing cannot write outside its
own directory or open a TCP connection — enforced by the kernel:

```yaml
capabilities: [network]   # this and nothing else
```

Details and limits in **[docs/SANDBOXING.md](docs/SANDBOXING.md)**.

---

## Install

New to local AI? Follow **[Your first local AI helper](docs/QUICKSTART.md)**
for Ollama installation, a small model download, a complete `nexus.yaml`, and
your first answer. The [offline walkthrough](docs/OFFLINE_DEMO.md) takes the
same helper to a Linux computer in a complete deployment bundle.

Download a binary from releases, or build from source:

```bash
git clone https://github.com/verdictlayer/nexusrun
cd nexusrun
./scripts/build.sh          # cross-compiles all platforms into dist/
go build -o nexus ./cmd/nexus   # or just build for this machine
```

Prebuilt for linux (amd64/arm64/arm), macOS (Intel/Apple Silicon),
Windows (x64/arm64), and FreeBSD. Static, CGO-free, no dependencies.

**To actually run models** you also need a GGUF runtime. Either:
- [llama.cpp](https://github.com/ggml-org/llama.cpp) — `llama-cli` on your PATH, or
- [Ollama](https://ollama.com/download) — running locally; use `--backend ollama`
  with an `ollama:` model source. It also provides model storage NexusRun can borrow from.

---

## Quickstart

For the step-by-step installation guide, start with
**[Your first local AI helper](docs/QUICKSTART.md)**. Already have Ollama running?
From this repository after `make build`, try the included small example:

```bash
ollama pull qwen2.5:0.5b
./bin/nexus run ./examples/plain-helper --backend ollama -p "What is a backup?"
./bin/nexus build ./examples/plain-helper
./bin/nexus run plain-helper:0.1.0 --backend ollama -p "What is a backup?"
```

The remaining examples use `nexus` installed on PATH and a model already
available locally:

```bash
# See what your machine can do
nexus doctor

# What models are already available (including Ollama's)?
nexus models

# Create and run a unit
nexus init my-agent --model llama3.1:8b
nexus build my-agent
nexus run my-agent:0.1.0 -p "Explain OCI artifacts in two sentences."

# Share it — any OCI registry works
export NEXUSRUN_REGISTRY_USER=you NEXUSRUN_REGISTRY_PASSWORD=$GITHUB_TOKEN
nexus push my-agent:0.1.0 ghcr.io/you/my-agent:0.1.0

# Or use the hosted registry: sign in once, from any terminal
nexus login
nexus push my-agent:0.1.0 registry.nexusrun.dev/you/my-agent:0.1.0

# Or carry it to an air-gapped machine
nexus build my-agent --seal -o my-agent.nx
nexus import my-agent.nx

# Keep models warm + watch it in a browser
nexus serve     # → http://127.0.0.1:7717
                # while running, `nexus run` skips model load entirely

# Chain units together
nexus compose extractor:0.1.0 summarizer:0.1.0 --input "$(cat report.txt)"

# Or declare a real workflow: a graph, with conditions and transforms
nexus compose init && nexus compose validate
nexus compose up -i "the Voyager 1 spacecraft" --stages

# Let each machine pick its own model, by measuring them
nexus bench my-agent:0.1.0        # what would this device choose, and why?

# Does it still work? On every backend this host can use?
nexus eval my-agent:0.1.0 --all-devices
nexus eval my-agent:0.1.0 --compare --fail-under 80
```

## The unit format

```yaml
apiVersion: nexusrun.dev/v1
name: research-agent
version: 0.1.0
description: Summarizes documents locally
license: MIT

models:
  - id: main
    source: ollama:llama3.1:8b     # or hf:org/repo/file.gguf, https://…, ./local.gguf
    sha256: ""                     # optional integrity pin
    context: 8192

entrypoint:
  type: chat
  system_prompt: |
    You are a careful research assistant.

hardware:
  prefer: [npu, gpu, cpu]          # ordered; first usable one wins
```

Unknown fields are rejected rather than ignored, so a typo fails the build
instead of silently doing nothing.

## Commands

| Command | What it does |
|---|---|
| `nexus init [dir]` | Scaffold a new unit |
| `nexus build [dir]` | Package as an OCI artifact (`--seal` to embed weights) |
| `nexus install <ref\|dir>` | Guided model, runtime, hardware, tool, secret, and acceptance-test setup |
| `nexus run <ref\|dir>` | Run on the best device (`--device`, `--backend` to force) |
| `nexus compose <a> <b> …` | Chain units, piping each output into the next |
| `nexus compose up` | Run a declared workflow — a graph with conditions, transforms, and a state bus |
| `nexus list` / `inspect` | Show local units, layers, and manifests |
| `nexus login` / `logout` / `whoami` | Sign in to NexusRun Cloud from any terminal (device flow) |
| `nexus usage` | Your Cloud plan, storage and monthly bandwidth |
| `nexus push` / `pull` | Any OCI registry — ghcr.io, Docker Hub, ECR, Harbor, zot |
| `nexus export` / `import` | Portable `.nx` file for sneakernet and air-gap |
| `nexus doctor` | Hardware vs. backend capability |
| `nexus bench [unit]` | Measure real tok/s per device, or pick a unit's model by measuring candidates |
| `nexus session` / `checkpoint` | Conversations an agent remembers, and moving them between machines |
| `nexus secret` | Credentials kept out of the artifact, encrypted per machine |
| `nexus tools` | Install and health-check a unit's MCP servers |
| `nexus eval` | Score a unit against a suite, per backend and device |
| `nexus models` | Local models, including reusable Ollama ones |
| `nexus logs` | Run history and captured output |
| `nexus serve` | Web console + warm-model daemon |

Running a directory directly (`nexus run ./my-agent`) skips packaging — the
fast inner loop while developing.

## Documentation

| Document | What's in it |
|---|---|
| [docs/QUICKSTART.md](docs/QUICKSTART.md) | **Start here.** Install Ollama, download a small model, create the YAML, and run your first helper |
| [docs/OFFLINE_DEMO.md](docs/OFFLINE_DEMO.md) | Seal the same helper and carry a complete Linux deployment bundle to an offline computer |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | Build, repo map, how to add a backend or model source, testing |
| [docs/REFERENCE.md](docs/REFERENCE.md) | Every command and flag, the manifest schema, env vars, on-disk and OCI layout |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Why the system is shaped this way — the design decisions and their trade-offs |
| [docs/COMPOSE.md](docs/COMPOSE.md) | Multi-agent workflows: the graph, routing conditions, transforms, and the shared state bus |
| [docs/AUTOMODEL.md](docs/AUTOMODEL.md) | Declaring what kind of model a unit needs and letting each machine measure which one clears the bar |
| [docs/TOOLS.md](docs/TOOLS.md) | Declaring tools, the agent loop that executes them, and per-tool capability narrowing |
| [docs/MCP.md](docs/MCP.md) | Declaring MCP servers as versioned, sandboxed dependencies of a unit |
| [docs/SECRETS.md](docs/SECRETS.md) | Keeping credentials out of the artifact: the encrypted store, device scoping, rotation, audit |
| [docs/CHECKPOINT.md](docs/CHECKPOINT.md) | Sessions an agent remembers, and moving that state to another machine |
| [docs/EVAL.md](docs/EVAL.md) | Scoring a unit per backend and device, the suite format, and what the numbers don't cover |
| [docs/SANDBOXING.md](docs/SANDBOXING.md) | What Landlock enforces, and the limits to read before trusting it |
| [docs/NPU.md](docs/NPU.md) | Why NPUs need a different stack than GPUs, and how to test one honestly |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Where help is most valuable, and the project's conventions |

## Status

Working and verified end-to-end on Linux:

- Packaging, OCI store, export/import round-trip
- **Push/pull against a real OCI registry** (tested against `registry:2`; the
  artifact is stored with a proper `artifactType` and annotations, and needs no
  NexusRun-specific registry support)
- Ollama model reuse — runs weights Ollama already downloaded, no copy
- Hardware detection and backend capability probing
- CPU execution via llama.cpp, with the model's chat template applied
- **Warm-model daemon** — `nexus serve` keeps weights resident; measured 5.3s
  cold to 2.4s warm on a repeat run of a 2.2 GB unit. `llama-server` is
  discovered next to `llama-cli`, so a normal llama.cpp build needs no config
- **Air-gapped execution** — a `--seal`ed unit exported, imported into an empty
  store, and generated correctly with no Ollama models directory, no reachable
  Ollama server, and a throwaway `HOME`
- **Managed runtime packages and deployment bundles** — safe explicit import,
  per-file and aggregate verification, atomic digest-addressed installation,
  target validation, and a relocatable launcher that carries NexusRun,
  llama.cpp, sealed weights, metadata, and notices. Verified end-to-end on a
  clean Linux x86-64 host: the bundle ran real inference with no engine or
  model installed beforehand.
- **Sandboxed script units** — Landlock-enforced, capability-gated. Verified
  from inside the sandbox: writes outside the unit directory, reads of the user
  home, and outbound TCP are all denied by the kernel, while shell and Python
  units run normally (see [docs/SANDBOXING.md](docs/SANDBOXING.md))
- **Pipeline composition** — `nexus compose a b c`
- **Tool execution** — the full agent loop: the model calls a tool, the
  runtime runs it under the unit's sandbox, feeds the result back, and the
  model continues. Verified end-to-end with llama3.1:8b through
  `llama-server`: a script tool converted 37°C to 98.6°F in one call, and an
  MCP-backed agent chained `list_notes` then `read_note` over three turns to
  answer from a file it was never shown. A tool that refuses goes back to the
  model to recover from; a sandbox denial or dead server aborts the run
  ([docs/TOOLS.md](docs/TOOLS.md))
- **MCP-native units** — servers declared in the unit like models are:
  version-pinned, fetched into a shared cache, started under Landlock with
  only their `allowed_paths` readable, health-checked with a real MCP
  handshake, and namespaced per server. stdio transport only, deliberately
  ([docs/MCP.md](docs/MCP.md))
- **Sessions and checkpoints** — an agent that remembers across runs, and
  `.state.nx` files that carry that memory to another machine. Verified: a
  session recalled a fact on a second run, and an encrypted checkpoint
  restored onto a clean store on which the session had never existed and
  recalled it again. `--seal` produced a self-contained 4.5 GB file
  ([docs/CHECKPOINT.md](docs/CHECKPOINT.md))
- **Secrets** — credentials declared by a unit and kept out of it, AES-256-GCM
  at rest, per-device scoping, rotation with a grace period, and an audit log
  that records key names and never values. Verified: injection into a
  sandboxed script unit including a mounted certificate, device-scoped
  override, and rotation ([docs/SECRETS.md](docs/SECRETS.md))
- **Agent Compose** — a `nexus-compose.yaml` graph with routing conditions,
  `text/template` transforms, per-agent model and hardware overrides, retries,
  and an append-only state bus. Verified end-to-end with two real units on
  llama3.1:8b: the condition gated the second stage, the transform reshaped the
  payload, and the bus recorded every hop with the tokens and device that
  produced it — including with AES-256-GCM encryption on, where the payload is
  opaque on disk and the wrong key fails loudly rather than reading empty.
  Detached runs (`up -d`, `ls`, `logs -f`, `down`) work through a pidfile, with
  no daemon (see [docs/COMPOSE.md](docs/COMPOSE.md))
- **Auto-model selection** — a unit declares the bar (context, size, tool
  calling, eval cases that must pass) and a shortlist; each machine measures
  which candidate clears it. Verified end-to-end: a 940 MB base model scored
  0/3 and was rejected, a 4.7 GB instruct model scored 3/3 and was selected,
  and the cached second run took 40 ms instead of minutes
  (see [docs/AUTOMODEL.md](docs/AUTOMODEL.md))
- **Unit evaluation** — `nexus eval` scores a unit against a suite packed
  inside the artifact, keyed to the unit and model digests, and reports per
  backend, device and model. Verified end-to-end on the example unit: the same
  weights scored 92% through `llama-server` and 77% through Ollama, and a
  2.0 GB model matched a 3.9 GB one — the sort of thing a single number hides
  (see [docs/EVAL.md](docs/EVAL.md))
- **NexusRun Cloud** — `nexus login` signs in with the OAuth device flow (works
  over SSH and on headless machines), stores the login at mode 0600, and
  `push`/`pull` use it. Verified against the live registry, including a
  400 MB sealed unit
- Benchmarking, run logging, web console

Platforms and limits:

- **Offline bundles and CPU inference:** Linux x86-64.
- **Accelerators.** `nexus doctor` reports whether the installed llama.cpp
  build can actually use a detected GPU, and NexusRun never silently falls
  back to CPU when it cannot. NPUs are detected and reported; the ONNX Runtime
  backend returns a clear error rather than falling back (see
  [docs/NPU.md](docs/NPU.md)).
- **Sandboxing is Linux-only.** On macOS and Windows, script units refuse to run
  unless `--no-sandbox` is passed.
- **Workflow resource limits.** `max_memory_mb`, `max_cpu_percent`, and
  `network.isolation` are validated and recorded in the manifest; enforcement
  comes from the process sandbox, with no per-agent cgroups or job objects.
- **Workflow agents run sequentially**, deliberately — two resident models is
  what the OOM killer stops on the hardware this targets — but that means a
  wide graph gains nothing from its width.
- **Eval assertions are mechanical** — substring, regex, JSON shape. There is
  no model-as-judge, deliberately: a judge's own quality varies with the
  quantization and hardware `nexus eval` exists to measure, so it cannot be
  the instrument. Script units cannot be evaluated at all yet.
- **Multi-unit DAGs.** Composition is a sequential pipeline, not a graph.

## License

Apache 2.0 — see [LICENSE](LICENSE).

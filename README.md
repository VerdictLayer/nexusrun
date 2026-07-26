# NexusRun

**Portable AI units — build once, run on whatever hardware you land on.**

[nexusrun.dev](https://nexusrun.dev) · [Quickstart](docs/QUICKSTART.md) · [Reference](docs/REFERENCE.md)

NexusRun packages an AI agent (its model, prompts, tools, and config) into a
standard **OCI artifact**, then runs it on the fastest accelerator the host can
actually use. No containers, no Python environment, no daemon. One static
binary, about 3 MB.

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
`nexus build --seal` embeds the weights as OCI layers. `nexus export` writes a
single file you can carry on a USB stick to a machine with no network.

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

### Should you use it?

**Good fit if you** want to hand someone a complete agent rather than setup
instructions; need to run on a Raspberry Pi, an air-gapped box, or a
locked-down laptop where Docker is not an option; are deploying the same agent
across mixed hardware and need to know what each host can actually do; or want
untrusted agent scripts confined by the kernel.

**Poor fit if you** just want to chat with a model locally (use Ollama — this
sits on top of it and adds nothing for that); need GPU inference verified today
(implemented but unverified, see Status); need NPU execution (not implemented);
need agent graphs with branching (composition is a sequential pipeline); or
need tools that actually run (they are declared, validated, and offered to
the model, but executing one is not implemented yet — see
[docs/TOOLS.md](docs/TOOLS.md)).

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
- [Ollama](https://ollama.com) — for model storage NexusRun can borrow from

---

## Quickstart

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

# Or carry it to an air-gapped machine
nexus build my-agent --seal -o my-agent.nx
nexus import my-agent.nx

# Keep models warm + watch it in a browser
nexus serve     # → http://127.0.0.1:7717
                # while running, `nexus run` skips model load entirely

# Chain units together
nexus compose extractor:0.1.0 summarizer:0.1.0 --input "$(cat report.txt)"

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
| `nexus run <ref\|dir>` | Run on the best device (`--device`, `--backend` to force) |
| `nexus compose <a> <b> …` | Chain units, piping each output into the next |
| `nexus list` / `inspect` | Show local units, layers, and manifests |
| `nexus push` / `pull` | Any OCI registry — ghcr.io, Docker Hub, ECR, Harbor, zot |
| `nexus export` / `import` | Portable `.nx` file for sneakernet and air-gap |
| `nexus doctor` | Hardware vs. backend capability |
| `nexus bench` | Measure real tok/s per device |
| `nexus eval` | Score a unit against a suite, per backend and device |
| `nexus models` | Local models, including reusable Ollama ones |
| `nexus logs` | Run history and captured output |
| `nexus serve` | Web console + warm-model daemon |

Running a directory directly (`nexus run ./my-agent`) skips packaging — the
fast inner loop while developing.

## Documentation

| Document | What's in it |
|---|---|
| [docs/QUICKSTART.md](docs/QUICKSTART.md) | **Start here.** Three steps to build your own agent, three to run someone else's |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | Build, repo map, how to add a backend or model source, testing |
| [docs/REFERENCE.md](docs/REFERENCE.md) | Every command and flag, the manifest schema, env vars, on-disk and OCI layout |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Why the system is shaped this way — the design decisions and their trade-offs |
| [docs/EVAL.md](docs/EVAL.md) | Scoring a unit per backend and device, the suite format, and what the numbers don't cover |
| [docs/TOOLS.md](docs/TOOLS.md) | Declaring tools, which backends can carry them, per-tool capabilities, and what isn't built yet |
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
- **Sandboxed script units** — Landlock-enforced, capability-gated. Verified
  from inside the sandbox: writes outside the unit directory, reads of the user
  home, and outbound TCP are all denied by the kernel, while shell and Python
  units run normally (see [docs/SANDBOXING.md](docs/SANDBOXING.md))
- **Pipeline composition** — `nexus compose a b c`
- **Unit evaluation** — `nexus eval` scores a unit against a suite packed
  inside the artifact, keyed to the unit and model digests, and reports per
  backend, device and model. Verified end-to-end on the example unit: the same
  weights scored 92% through `llama-server` and 77% through Ollama, and a
  2.0 GB model matched a 3.9 GB one — the sort of thing a single number hides
  (see [docs/EVAL.md](docs/EVAL.md))
- Benchmarking, run logging, web console

Not done, and not pretended otherwise:

- **NPU execution.** Detection and scheduling are in place; the ONNX Runtime
  backend returns a clear error instead of silently falling back to CPU. See
  [docs/NPU.md](docs/NPU.md).
- **GPU execution** is implemented but unverified end-to-end. The development
  machine's Quadro RTX 5000 and driver now work — `nexus doctor` detects it —
  but the llama.cpp build there has no CUDA compiled in, so no generation has
  ever actually run on it. Verifying this needs a CUDA-enabled llama.cpp build.
- **Sandboxing is Linux-only.** On macOS and Windows, script units refuse to run
  unless `--no-sandbox` is passed.
- **Tool execution.** Tools are declared in the manifest, validated at build
  time, offered to the model, and the model's request is reported exactly as
  it was made — verified with llama3.1:8b through `llama-server`. Executing
  the tool and feeding its result back is the next piece of work. Until it
  lands, a unit whose model calls a tool **fails** with a non-zero exit
  rather than answering as though the tools were absent
  ([docs/TOOLS.md](docs/TOOLS.md)). No MCP.
- **Eval assertions are mechanical** — substring, regex, JSON shape. There is
  no model-as-judge, deliberately: a judge's own quality varies with the
  quantization and hardware `nexus eval` exists to measure, so it cannot be
  the instrument. Script units cannot be evaluated at all yet.
- **Multi-unit DAGs.** Composition is a sequential pipeline, not a graph.

## License

Apache 2.0 — see [LICENSE](LICENSE).

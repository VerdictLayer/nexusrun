# Quickstart

Two paths, three steps each. Pick the one you need.

- **[Build your own agent](#build-your-own-agent)** — you have an idea and want to package it.
- **[Run someone else's](#run-someone-elses-agent)** — someone handed you a unit.

Every command and every output below was run on Linux; nothing here is
illustrative.

---

## Before you start

You need the `nexus` binary, plus a way to actually execute models:

```bash
git clone https://github.com/verdictlayer/nexusrun
cd nexusrun
make build        # → bin/nexus
```

For generation you also need **either** [llama.cpp](https://github.com/ggml-org/llama.cpp)
(`llama-cli` on your PATH) **or** [Ollama](https://ollama.com) running. If you
already use Ollama, NexusRun will reuse the models you have pulled — no
second download.

Check the machine before anything else:

```bash
nexus doctor
```

It prints your hardware, then which backends can genuinely drive it. If it
reports no usable devices, install llama.cpp before going further — that is a
real answer, not a bug.

---

## Build your own agent

### 1. Create it

```bash
nexus init my-agent --model llama3.1:8b
```

That writes one file, `my-agent/nexus.yaml`. The whole agent lives there:

```yaml
apiVersion: nexusrun.dev/v1
name: my-agent
version: 0.1.0

models:
  - id: main
    source: ollama:llama3.1:8b     # reuses Ollama's copy if you have it
    context: 4096

entrypoint:
  type: chat
  system_prompt: |
    You are a helpful assistant.

hardware:
  prefer: [npu, gpu, cpu]          # first *usable* one wins
```

Edit `system_prompt` to whatever you want the agent to be. That's the part
that makes it yours.

`--model` accepts more than Ollama names: `hf:<org>/<repo>/<file>.gguf`
(add `@<revision>` to pin a branch or tag), an `https://` URL, or a path to a
local `.gguf`. For gated Hugging Face repos, set `HF_TOKEN`.

### 2. Run it straight from the folder

```bash
nexus run ./my-agent -p "What is an OCI artifact?"
```

```
running my-agent:0.1.0 on CPU via llama.cpp
An OCI (Open Container Initiative) artifact is a Docker image or container
specification that adheres to the OCI standards for packaging and running
applications in containers.

— 36 tokens · 14.2 tok/s · llama.cpp on CPU
```

Pointing `run` at a **directory** skips packaging entirely, so this is the
loop to stay in while you tune the prompt: edit `nexus.yaml`, run again.

### 3. Package and share it

```bash
nexus build my-agent
nexus push my-agent:0.1.0 ghcr.io/you/my-agent:0.1.0
```

```
Built my-agent:0.1.0
  digest: sha256:f44913fe54eeda41e5f786a4e59549fe4c51687c4f1260c523f9e8fe60a2ad77
  layers: 1
  sealed: false

Pushed 127.0.0.1:5002/lance/my-agent:0.1.0
```

The artifact is a few hundred bytes, because it references the model rather
than embedding it. Any OCI registry works — ghcr.io, Docker Hub, ECR, Harbor,
a local zot — with no NexusRun-specific support on the server.

Credentials come from the environment:

```bash
export NEXUSRUN_REGISTRY_USER=you
export NEXUSRUN_REGISTRY_PASSWORD=$GITHUB_TOKEN
```

**No network where it's going?** Seal the weights in and carry one file:

```bash
nexus build my-agent --seal -o my-agent.nx
```

---

## Run someone else's agent

### 1. Pull it

```bash
nexus pull ghcr.io/them/their-agent:0.1.0
```

```
Pulled their-agent:0.1.0
  run it: nexus run their-agent:0.1.0 -p "hello"
```

Handed a `.nx` file instead? `nexus import their-agent.nx` does the same job
with no network at all.

### 2. Look at it before you run it

```bash
nexus inspect their-agent:0.1.0
```

```
Unit:        their-agent:0.1.0
Description: A NexusRun unit
Artifact:    application/vnd.nexusrun.unit.v1
Prefer:      npu → gpu → cpu

Models:
  main       ollama:phi3:latest (gguf)

Layers:
  application/vnd.nexusrun.unit.source.v1.tar+gzip     447 B      a2387869794a
```

Worth doing every time. This tells you which model it will pull, how big it
is, and — for units that run code — which capabilities they ask for. A unit
that declares nothing gets nothing: no file access outside its own directory,
no network. See [SANDBOXING.md](SANDBOXING.md).

### 3. Run it

```bash
nexus run their-agent:0.1.0 -p "What is a Raspberry Pi?"
```

```
A Raspberry Pi is a small, affordable, single-board computer that can be used
for a variety of tasks, often for educational purposes and hobby projects

— 34 tokens · 14.1 tok/s · llama.cpp on CPU
```

On first run the model is resolved: reused in place if Ollama already has it,
downloaded once into a shared cache otherwise, or read straight from the
artifact if it was sealed. Later runs skip that.

---

## Making it faster

Keep models loaded between runs:

```bash
nexus serve      # → http://127.0.0.1:7717
```

While that's running, `nexus run` uses it automatically. Measured on a 2.2 GB
model: **5.3s cold, 2.4s warm**. It also serves a local dashboard showing your
units, hardware and run history. If it isn't running, everything still works —
it's pure upside.

Want to know whether your GPU is actually worth using?

```bash
nexus bench -m ollama:llama3.1:8b
```

It runs the same prompt on every usable device and reports real tokens per
second. Accelerators are not automatically faster; for small models the
transfer overhead often exceeds the win.

---

## When something goes wrong

| Symptom | What it means |
|---|---|
| `no execution backend available` | Neither llama.cpp nor Ollama was found. `nexus doctor` lists each one and why it is unavailable. |
| `ollama model "x" not found` | The name doesn't match anything in your Ollama store. `nexus models` lists what is actually there. |
| Runs on CPU when you expected GPU | Your llama.cpp was probably built without GPU support. `nexus doctor` says so explicitly — it will never pretend otherwise. |
| `integrity check failed` | The downloaded weights don't match the manifest's `sha256`. Treat as a real failure, not a flake. |
| Script unit refuses to start | Sandboxing is Linux-only today. On macOS and Windows, pass `--no-sandbox` and only for units you trust. |

Full flag and schema reference: **[REFERENCE.md](REFERENCE.md)**.

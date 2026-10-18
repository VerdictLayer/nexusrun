# First-helper validation record

The walkthroughs in `QUICKSTART.md` and `OFFLINE_DEMO.md` use the runnable
fixture in `examples/plain-helper`. The following checks were performed with
real weights and inference, not a mock model server.

## Environment

- Pop!_OS 22.04 (Ubuntu Jammy base), Linux x86-64, glibc 2.35.
- Intel Xeon W-10885M, 125 GiB system RAM; Quadro RTX 5000 available to Ollama.
- Go 1.26.5; NexusRun built with `CGO_ENABLED=0 make build`.
- Ollama 0.10.1, isolated model directory, model pulled from the Ollama registry.
- Model: `qwen2.5:0.5b`, weights SHA-256
  `c5396e06af294bd101b30dce59131a76d2b773e76950acc870eda801d3ab0515`.
- CPU runtime: llama.cpp `b5688`, upstream revision
  `860a9e4eeff3eb2e7bd1cc38f65787cc6c8177af`, built with
  `scripts/build-managed-runtime.sh` and libc label `glibc>=2.35`.
- Runtime payload digest:
  `6ec809f534a4ae1710fa25f77d2f7b92484499dca5fb9579e63a9d76e6b0a691`.

The 8 GB RAM / 15 GB disk figures in the beginner guide are suggested setup
budgets, not minimums established on this larger development host.

## Results

1. `ollama pull`, `ollama run`: successful real download and answer.
2. Directory execution through `--backend ollama`: successful answer,
   backend `ollama`, device `AUTO`.
3. Linked build: `sealed: false`, one source layer. Running the packaged
   reference through Ollama succeeded with external networking blocked.
4. Sealed build: `sealed: true`, source plus model layers; `.nx` about 380 MiB.
5. Runtime build and import succeeded; complete bundle about 379 MiB.
6. Bundle extracted into a path containing spaces. Its launcher was invoked
   by absolute path from a different directory, twice, starting with no
   bundle-local state. PATH was `/usr/bin:/bin`, excluding installed Ollama
   and llama.cpp. Both questions produced nonempty answers through
   `llama.cpp/server` on CPU.
7. Both offline checks ran in a new user/network namespace created with
   `unshare --user --map-root-user --net`, enabling only the loopback
   interface. There was no external interface or route. The Ollama check
   started its own service inside the namespace against the downloaded model.
8. `make check` passed. `file bin/nexus` confirmed a static executable.

Example bundle output for “What is a backup?”:

```text
running plain-helper:0.1.0 on CPU via llama.cpp/server
A backup is a method of protecting your data from loss or theft by copying it to another location or medium.

— 23 tokens · 67.0 tok/s · llama.cpp/server on CPU
```

This exercise used the host's Ubuntu-based system libraries; the network
namespace is not a new OS image. Apply the clean-host acceptance procedure in
`RUNTIME_PACKAGES.md` to the actual destination baseline before distributing
a runtime for it. Model output and throughput vary between runs.

## Website checks

The renderer generates both standalone website pages from the same Markdown.
Browser checks cover the homepage and both guides at 390, 768, 1024, 1280,
and 1440 px in light/dark themes; guide anchors, exact command/YAML clipboard
contents, and JavaScript errors are checked too. The YAML in the quickstart
matches the runnable fixture byte for byte.

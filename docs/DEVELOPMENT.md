# Development guide

How to build NexusRun, find your way around the code, and extend it.

New to the tool itself? Run through [QUICKSTART.md](QUICKSTART.md) first —
it takes a few minutes and everything below assumes you have built and run
a unit at least once.

For *why* the system is shaped the way it is, read
[ARCHITECTURE.md](ARCHITECTURE.md) first — this document assumes those
decisions and tells you how to work inside them. For the complete CLI,
manifest, and environment reference, see [REFERENCE.md](REFERENCE.md).

---

## Setup

You need **Go 1.26+**. Nothing else is required to build — there are no
codegen steps, no C toolchain, no Node.

```bash
git clone https://github.com/verdictlayer/nexusrun
cd nexusrun
make build        # → bin/nexus (version-stamped)
make test
make vet
```

If `go` is not on your PATH, it is usually at `/usr/local/go/bin`:

```bash
export PATH=$PATH:/usr/local/go/bin
```

To actually execute models you also need a GGUF runtime — either
`llama-cli` (llama.cpp) on your PATH, or Ollama running. Everything
except generation works without them: packaging, push/pull, export,
inspection, and hardware detection are all exercised by
`go test ./...` with no model present.

Verify your environment:

```bash
./bin/nexus doctor
```

If it reports no usable devices, that is a real result, not a bug — it
means no backend on this machine can execute anything. Install llama.cpp
before filing an issue.

---

## The inner loop

Packaging on every change is slow and unnecessary. `nexus run` accepts a
directory, which skips the OCI store entirely:

```bash
make build
./bin/nexus run ./examples/code-reviewer -p "review this: func f() {}"
```

Use a scratch store so experiments never touch your real one:

```bash
export NEXUSRUN_HOME=/tmp/nexus-dev
./bin/nexus build ./examples/code-reviewer
./bin/nexus list
rm -rf /tmp/nexus-dev     # reset to a clean slate
```

Force a specific path rather than letting the scheduler choose, which is
how you test a backend you cannot otherwise reach:

```bash
./bin/nexus run ./my-agent -p hi --device cpu --backend llama.cpp
./bin/nexus run ./my-agent -p hi --no-daemon    # bypass a running serve
```

---

## Repository map

```
cmd/nexus/
  main.go            root command, version stamping, binName constant
  commands.go        every subcommand and its flags (~1000 lines)

internal/
  manifest/          nexus.yaml schema, parsing, validation
  hardware/          CPU/GPU/NPU detection — "what is in this machine"
  engine/            backend abstraction + capability probing — "what can run"
  unit/              OCI packaging, registry push/pull, export/import
  store/             content-addressed blobs, model resolution, run records
  daemon/            warm-model pool backed by llama-server
  server/            local web console + daemon HTTP API
  runner/            script-unit execution + sandbox re-exec helper
  sandbox/           Landlock policy derivation and enforcement
  compose/           sequential unit pipelines
  bench/             cross-device throughput measurement
```

Everything is `internal/`, so there is no public Go API to keep stable
yet. The stable contracts today are the **CLI**, the **nexus.yaml
schema**, and the **OCI artifact format** — treat those as the things you
cannot casually break.

### What each package owns

| Package | Owns | Key types |
|---|---|---|
| `manifest` | The nexus.yaml contract | `Manifest`, `Model`, `Entrypoint`, `Hardware` |
| `hardware` | Host detection, never execution | `Report`, `Device`, `Class*` constants |
| `engine` | Executing a prompt on a device | `Backend`, `Capability`, `Request`, `Result` |
| `unit` | Bytes on disk and on registries | `Build`, `Push`, `Pull`, `Unpack`, media-type constants |
| `store` | `~/.nexusrun`, model resolution | `Store`, `ResolvedModel`, `RunRecord` |
| `sandbox` | Landlock policy | `Policy`, `FromCapabilities`, `Apply` |
| `compose` | Ordering stages, not running them | `Run`, `RunFunc`, `Stage` |

The dependency direction is one-way: `manifest` and `hardware` depend on
nothing internal; `engine` depends on `hardware`; `unit` depends on
`manifest` and `store`; `cmd` depends on everything. Keep it that way —
if a low-level package needs something from a higher one, inject it as a
function instead. `compose.RunFunc` is the existing example: it takes a
"run one unit" closure so the package never imports `engine` or `unit`.

---

## Two invariants

**1. Never silently fall back.** If a GPU or NPU cannot be used, say so
loudly and stop. A quiet fallback to CPU turns every benchmark number and
capability claim into a lie, and honest capability reporting is the whole
credibility of this project. This is the one hard rule.

**2. Detection and capability are different questions.** `hardware`
answers "what is physically in this machine." `engine.Probe()` answers
"what can actually execute right now." Only the *intersection* is safe to
schedule against — see `engine.Select`. Never schedule on detection
alone, and never assume a device is usable because it was detected.

The development machine is the reason both rules exist: it has a working
Quadro RTX 5000 that `nexus doctor` detects correctly, and a llama.cpp
compiled without CUDA that cannot touch it. Detection alone would have
promised GPU acceleration and silently delivered CPU speeds.

---

## Execution flow

What happens on `nexus run my-agent:0.1.0`:

```
cmd/nexus/commands.go  newRunCmd
    │
    ├─ is a daemon listening?              300ms dial to NEXUSRUN_ADDR
    │     └─ yes → POST /api/run, done     internal/server + internal/daemon
    │
    ├─ resolve unit from OCI store         internal/unit  Resolve/Unpack
    ├─ resolve model source → local path   internal/store Resolve
    │     ollama: blob, cached download, or local file
    ├─ detect hardware                     internal/hardware Detect
    ├─ probe backend capability            internal/engine  Probe
    ├─ select (backend, device)            internal/engine  Select — the intersection
    ├─ generate, streaming to stdout       Backend.Generate
    └─ write RunRecord + captured output   internal/store SaveRun
```

Script units branch at the generate step into `internal/runner`, which
derives a Landlock policy from the manifest's `capabilities:` and
re-execs itself through the hidden sandbox helper subcommand.

---

## Extending it

### Add an execution backend

Implement three methods and register the type. Everything else —
scheduling, benchmarking, `doctor` output — follows automatically.

```go
// internal/engine/mybackend.go
type MyBackend struct{}

func (m *MyBackend) Name() string { return "mybackend" }

func (m *MyBackend) Probe() Capability {
    c := Capability{Backend: m.Name()}
    bin := findMyBinary()
    if bin == "" {
        // Say what to do next. "unavailable" alone is a useless message.
        c.Detail = "mybackend not found (install it or set NEXUSRUN_MYBACKEND)"
        return c
    }
    c.Available = true
    c.AcceptsModelPath = true          // false if it only takes model *names*
    c.Devices = []string{hardware.ClassCPU}
    return c
}

func (m *MyBackend) Generate(ctx context.Context, req Request) (*Result, error) { … }
```

Then add it to `All()` (one-shot scheduling) and `ProbeAll()`
(diagnostics) in `engine.go`.

Three things to get right:

- **`Devices` must list what the backend can *drive*, not what exists.**
  `LlamaCPP.Probe` shells out to `llama-cli --list-devices` and reports
  GPU only when a GPU backend was compiled in. Copy that pattern.
- **`AcceptsModelPath: false`** excludes the backend from path-based
  scheduling and from `nexus bench`. Ollama sets this because it
  addresses models by name and cannot execute arbitrary unit weights.
- **Populate `Result` timing fields** (`EvalTPS`, `PromptTPS`, `LoadMS`)
  if the tool reports them. `nexus bench` and the run log are built on
  them; a backend that leaves them zero shows up as a blank row.

`Capability` and `Result` are served over HTTP and printed by
`--json`, so **their struct tags are part of the wire contract** — the
web console reads `Capability`'s lowercase keys directly. Both shipped
untagged once, which silently rendered every backend row in the console
as an "OFF" pill labelled `undefined`. `engine_test.go` guards the keys;
don't rename them casually.

Backends **shell out rather than link**. This keeps the binary CGO-free
and portable and avoids ABI coupling to fast-moving C++ projects. If you
need cgo, that is a design discussion first — a cgo dependency breaks
the eight-target static cross-compile that makes this project work on
edge hardware.

### Add a model source scheme

Model sources are resolved in `store.Resolve`, a single switch on the
prefix. Adding `s3://`, `oci://`, or similar means one case:

```go
case strings.HasPrefix(source, "myscheme:"):
    return s.download(translateToURL(source), wantDigest, source, p)
```

Reuse `s.download` where you can — it handles content-addressed caching,
streaming to a temp file, progress reporting, and `sha256` verification
against the manifest's pin. Return `Shared: true` only when the file is
owned by another tool and must not be moved or deleted, as with Ollama
blobs.

### Add a CLI command

Add a `newXCmd()` constructor in `cmd/nexus/commands.go` and register it
in `main.go`. Conventions in this codebase:

- `--json` on anything a script might consume.
- Errors say what to do next, not just what failed.
- Long-running work reports progress through a callback the package
  accepts (`BuildOptions.Progress`, `store.Progress`), so the package
  never writes to stdout itself.

### Add a manifest field

`manifest.Parse` uses `dec.KnownFields(true)` — unknown fields are a hard
error, so a typo fails the build instead of silently doing nothing.

**This makes new fields a forward-compatibility break.** A unit using a
field you just added will fail to parse on every older `nexus` binary,
with a confusing "field not found" error. So:

1. Add the field to the struct with `omitempty`.
2. Default it in `applyDefaults()` so existing units keep working.
3. Validate it in `Validate()` with a message naming the legal values.
4. Bump `APIVersion` only for a breaking *change*, not an addition —
   but expect old binaries to reject the new field regardless.

---

## Testing

Current coverage is `manifest`, `unit` (packing, including tar path
traversal), `sandbox` (policy derivation), `compose` (pipeline ordering,
via an injected `RunFunc`), and `engine` (JSON wire contract only).
`store`, `daemon`, `server`, and `runner` have no tests, and `engine`
has no coverage of `Select` — that is the largest known gap in the repo.

`Select` is currently untestable without a refactor: it calls `All()`
internally, so there is no seam to inject a stub `Backend`. Making it
take a `[]Backend` parameter would fix that and is worth doing.

```bash
make test
go test ./internal/unit -run TestPack -v
go test ./... -race
```

Two rules worth keeping:

- **Anything parsing untrusted input gets a test.** Archives, manifests,
  registry references. Tar path traversal is already covered; keep it
  that way.
- **Tests must not require a model, a GPU, or a network.** CI runs on
  ubuntu/macos/windows runners with none of the three. Inject
  dependencies rather than reaching for real hardware —
  `compose.RunFunc` exists precisely so pipeline logic is testable
  without an inference engine.

### Testing hardware paths you don't have

You mostly can't, and pretending otherwise is how the honesty rule gets
broken. What you *can* do:

- **Force selection** with `--device` / `--backend` to confirm the error
  path is clear rather than silent.
- **Fake the probe.** `Backend` is an interface; a stub returning
  `Devices: ["npu"]` exercises `Select` without an NPU.
- **Point at a fake binary.** `NEXUSRUN_LLAMA_CLI` accepts any
  executable, so a shell script emitting canned `llama-cli` stderr tests
  the performance-counter regexes in `engine.go` with no model at all.

If you genuinely cannot verify something, say so in the PR and in the
docs. Unverified-but-written is an honest state; it is recorded that way
for GPU offload in the README today.

---

## Sandboxing notes

Landlock work has one trap worth knowing before you touch
`internal/sandbox`:

> `RestrictPaths()` with **no rules** does not test whether Landlock is
> supported — it enforces a deny-everything policy. Support detection
> must query the ABI version instead.

Getting that wrong sandboxes the CLI itself before it can exec anything,
which presents as a baffling permission error far from the cause.

Policies are inherited across `exec` and cannot be dropped, so the policy
is applied in a helper process that then *becomes* the interpreter via
`syscall.Exec` — no unconfined parent survives. The helper is a hidden
subcommand; see `runner.HelperCommandName()`.

Test enforcement is real, not assumed:

```bash
go test ./internal/sandbox -v
```

Sandboxing is Linux-only. macOS (Seatbelt) and Windows (AppContainer) are
unimplemented stubs in `sandbox_other.go` and `helper_windows.go`, and
script units refuse to run there without `--no-sandbox`. See
[SANDBOXING.md](SANDBOXING.md) for the enforcement details and limits.

---

## Releasing

```bash
make dist     # → dist/, all eight targets + checksums.txt
```

`scripts/build.sh` cross-compiles with `CGO_ENABLED=0 -trimpath` and
stamps `main.Version` / `main.Commit` via ldflags, taking them from `git
describe` unless `VERSION` / `COMMIT` are set in the environment.

Targets: linux amd64/arm64/arm (GOARM=7, for 32-bit Raspberry Pi OS),
darwin amd64/arm64, windows amd64/arm64 (Snapdragon X / Copilot+), and
freebsd amd64.

CI configuration is maintained outside this repository, so the
pre-release gate is local and every one of these must pass:

```bash
gofmt -l ./cmd ./internal   # must print nothing
go vet ./...
go test ./...
make dist                   # all eight targets must cross-compile
```

**If `make dist` fails after your change, the usual cause is a new
dependency that pulls in cgo or is platform-specific — fix it with build
tags rather than dropping a target.** The static CGO-free binary is what
makes this run on edge hardware at all.

---

## Conventions

- **Standard library first.** Every dependency earns its place. The
  current set is `oras-go` (OCI), `cobra` (CLI), `yaml.v3`, and
  `go-landlock`. The static CGO-free binary is a feature, not an accident.
- **Comments explain why, not what.** If a comment restates the code,
  delete it. The good comments in this codebase record traps — read
  `sandbox.Supported` and `engine.All` for the house style.
- **Errors say what to do next.** `nexus doctor` exists because "no
  backend available" on its own is useless. Compare
  `engine.Select`, which lists every backend and the specific reason each
  one is unavailable.
- **Platform-specific code goes in `_linux.go` / `_windows.go` / `_other.go`
  files**, not `runtime.GOOS` branches, when the whole implementation
  differs. `internal/sandbox` is the model.

## Renaming the project

The CLI name appears in exactly one place, `binName` in
`cmd/nexus/main.go`. Rebranding is that constant, the directory
`cmd/nexus/`, the module path in `go.mod`, and the `dev.nexusrun.*`
annotation and media-type constants in `internal/unit/pack.go` — the last
of which is an artifact-format break and should happen before anything is
published widely, not after.

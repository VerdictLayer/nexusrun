# Managed runtimes and offline bundles

NexusRun delegates inference to llama.cpp. A managed runtime is executable
code, not part of an agent and not a new inference implementation. Runtime
packages and deployment bundles are platform-specific; `.nx` agent artifacts
remain portable and `nexus build --seal` still means only “embed weights.”

## Runtime package v1

A package is a gzip-compressed tar archive with `runtime.json` at its root and
only regular files/directories. Imports reject absolute paths, `..` traversal,
links, devices, duplicate archive names, more than 4,096 entries, or more than
4 GiB of expanded regular-file data.

`runtime.json` uses schema `nexusrun.dev/runtime/v1` and records:

- exact engine version/upstream revision and build provenance;
- OS, architecture, CPU/accelerator baseline, libc, and host dependencies;
- server/CLI/library paths and the supported protocol/features;
- licenses/notices;
- the size and SHA-256 of every payload file plus an aggregate payload digest.

The aggregate is SHA-256 over each file's bytes in the order listed in
`files`. The package builder sorts that list by path. The metadata file is not
part of its own digest. Hashes prove content integrity, not publisher identity.
P0 local imports are therefore explicit trust decisions. Public connected
distribution requires signed catalog metadata and an established trust root.

Imports stage under `$NEXUSRUN_HOME/runtimes`, verify before activation, then
atomically rename to:

```text
runtimes/<engine>/<version>/<os>/<arch>/<payload-digest>/
```

Commands:

```sh
nexus runtime import ./llama.runtime.tar.gz
nexus runtime list
nexus runtime inspect <runtime-id>
nexus runtime verify <runtime-id>
nexus runtime activate <runtime-id>
```

## Build a CPU runtime

Use an already-reviewed llama.cpp checkout at the exact revision being
packaged. The script refuses a dirty or different revision. It builds without
`-march=native` and packages the executables, static project libraries, and
llama.cpp notices:

```sh
scripts/build-managed-runtime.sh \
  /src/llama.cpp b10695 linux/amd64 \
  'glibc>=2.31' dist/llama-b10695-linux-amd64.runtime.tar.gz
```

Linux arm64 runtimes are built on an arm64 host with `linux/arm64`.

## Full offline deployment

The unit must declare the target and have sealed model layers. Import the
reviewed runtime, build the matching NexusRun binary, then export:

```sh
nexus build examples/offline-chat --seal -o offline-chat.nx
nexus runtime import dist/llama-b10695-linux-amd64.runtime.tar.gz
nexus bundle offline-chat:0.1.0 \
  --target linux/amd64 \
  --runtime 'llama.cpp/b10695/linux/amd64/<payload-digest>' \
  --nexus dist/nexus-linux-amd64 \
  -o dist/offline-chat-linux-amd64.tar.gz
```

Extract the bundle and run `./run -p 'Say hello'`. The launcher resolves every
path relative to itself, uses `state/` as its default writable home, imports
the agent idempotently, disables daemon fallback, and pins the bundled
llama.cpp executables. It does not modify PATH or require root.

`--offline` means NexusRun does not resolve external dependencies. It permits
loopback inference traffic and is not an OS network sandbox. The first bundle
profile accepts sealed chat agents without tools and rejects scripts, tools,
MCP sources, and host interpreters before export. Secrets are never copied.

## Clean-host smoke test

On the declared baseline host or VM, with no llama.cpp/Ollama on PATH and an
empty cache:

1. Block external networking while retaining loopback.
2. Extract into a path containing spaces and `cd` elsewhere.
3. Run the absolute path to `run`; require a nonempty real model response.
4. Run it again and confirm no download and successful cache reuse.
5. Corrupt a runtime file and confirm `nexus runtime verify` fails.
6. Send SIGINT during generation and confirm no inference child remains.
7. Record OS image, libc, CPU features, RAM, model/runtime digests, startup
   time, and result.

Fast tests use synthetic bytes and do not establish real-inference support.
This gate has been run on a clean Linux x86-64 host.

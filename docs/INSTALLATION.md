# Guided installation

`nexus install <ref|dir>` is the first-run setup flow shared by the CLI and
the local web console. It checks dependencies before running the unit and
keeps going after every approved repair:

1. Locate each model locally. For a missing model, show its declared or
   origin-reported size, request approval, verify its digest when declared,
   cache it, and resume.
2. Intersect detected hardware with installed backend capability. When no
   compatible backend exists, offer a platform-specific llama.cpp release
   pinned by version and SHA-256. Managed runtimes live below
   `$NEXUSRUN_HOME/runtimes/`; NexusRun finds them without a PATH edit.
3. Compare model working-memory estimates with accelerator memory and system
   RAM. An unavailable or undersized GPU falls back to the tested CPU
   configuration when it fits; an actually undersized machine is blocked.
4. Install declared MCP dependencies through the same resolver as
   `nexus tools install`, and store entered credentials through the same
   encrypted store as `nexus secret set`.
5. Run the unit's included `evals/` suite and list every passing, failing, or
   errored case. A unit with no suite reports acceptance tests as skipped.

Downloads never begin during planning. In scripts, `--json` returns that plan
without prompting, and `--json --yes` applies it.

## Model size metadata

HTTP and Hugging Face origins are queried with `HEAD`. Registries that do not
publish `Content-Length` can make approval deterministic with `size_bytes`:

```yaml
models:
  - id: main
    source: hf:org/repo@revision/model.gguf
    sha256: sha256:…
    size_bytes: 4920578048
```

Auto-model candidates accept the same field.

## Offline target declaration

An offline export needs an explicit target and a bundled runtime:

```yaml
installation:
  targets:
    - os: linux
      arch: amd64
      device: cpu
  runtime:
    name: llama.cpp
    version: b10695
    path: runtimes/linux-amd64
```

Build with `--seal`, place the pinned runtime at the declared relative path,
then validate and export:

```sh
nexus build --seal .
nexus export --offline --target linux/amd64/cpu my-agent:1.0.0 my-agent.nx
```

Offline validation also rejects externally resolved MCP servers and host
commands. Bundle those executables inside the unit and declare them as
unit-relative inline tools. Credentials are never bundled.

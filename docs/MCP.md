# MCP-native units

MCP is how an agent reaches tools it did not ship with — a filesystem
server, a Git server, a company's internal one.

NexusRun treats those servers the way it treats models: declared in the
unit, version-pinned, resolved into a shared local cache, and started under
the same kernel sandbox as everything else. The arrangement MCP tooling
usually assumes — the operator installs and runs the servers, and the agent
trusts whatever is listening — is exactly what a portable artifact exists
to avoid.

```yaml
apiVersion: nexusrun.dev/v1
name: dev-assistant
version: 1.0.0

models:
  - source: ollama:llama3.1:8b
    context: 8192

entrypoint:
  type: chat
  system_prompt: |
    Use the filesystem tools to answer questions about the project.

mcp_servers:
  filesystem:
    source: github:modelcontextprotocol/servers#0123456789abcdef0123456789abcdef01234567/src/filesystem
    command: ["node", "dist/index.js", "/home/user/projects"]
    env:
      ALLOWED_EXTENSIONS: ".go,.md,.yaml"
    sandbox:
      allowed_paths: ["/home/user/projects"]
    tools: [read_file, list_directory]
    timeout: 30s
```

```bash
nexus tools install ./dev-assistant   # fetch what it declares
nexus tools check ./dev-assistant     # start each one, list its tools
nexus run ./dev-assistant -p "…"      # servers come up and down with the run
```

---

## Sources

| Form | Resolution |
|---|---|
| `github:org/repo#ref/path` | Shallow clone at `ref` into the cache |
| `file:///absolute/path` | Used in place |
| `npm:package` | `npm install` into the cache |
| `exec:command args…` | A program already on this machine |
| `ghcr.io/user/img:tag` | **Not implemented** — use one of the above |

Everything except `file://` and `exec:` is cached under
`$NEXUSRUN_HOME/mcp/` and shared across units.

### Pinning

A source that names a branch is a **floating ref**: the server's code can
change under a unit whose digest did not, which is the property unit
digests exist to provide. NexusRun warns at install and at run.

Pinned means a 40-character commit SHA for GitHub, a version for npm, or a
local path (which is whatever is on that machine, by definition).

### `command`

Most sources need it. A cloned repository is not self-describing about
which file to run, and guessing wrong produces a startup failure that reads
like a protocol error. A relative command resolves inside the server's own
directory.

---

## Lifecycle

```
1. Parse the source; warn if it is a floating ref
2. Fetch it if the cache does not have it
3. Start the process, under the sandbox, with the unit's secrets in its env
4. MCP handshake: initialize → notifications/initialized → tools/list
5. Namespace and offer the tools to the model
6. On exit, close stdin (the protocol's shutdown signal), then kill if needed
```

**A server that fails to start aborts the run.** Carrying on with fewer
tools produces an agent that quietly cannot do part of its job and reports
success while doing it.

When a server dies during startup — the common real failure — its stderr is
captured and included in the error. Losing that turns every such failure
into an unexplained timeout:

```
nexus: mcp handshake with filesystem failed: … 
  filesystem said: ROOT_PATH does not exist
```

---

## Tool namespacing

A server's tools are offered as `<server>__<tool>`:

```
notes__list_notes
notes__read_note
```

Two servers routinely offer the same name — a filesystem server and a git
server both have `read_file` — and a model asked to choose between two
identical names picks arbitrarily. The separator is a double underscore
because the tool-name syntax every backend accepts is `[A-Za-z0-9_-]` and a
single underscore is common inside real tool names.

### Narrowing with `tools:`

Listing tool names restricts what the model is offered. A filesystem server
exposing eleven tools when the unit needs two is nine extra ways for the
model to go wrong. A list that matches nothing is an error, not an empty
tool set — that is a typo, and silently producing an agent with no tools is
the worst possible response to one.

---

## Sandboxing

Servers run under the same Landlock helper as script units: the helper
applies the policy to itself and then becomes the server, so confinement is
inherited and cannot be dropped.

| Field | Effect |
|---|---|
| `sandbox.allowed_paths` | Absolute paths the server may read |
| `sandbox.blocked_paths` | Recorded; **not yet enforced** — Landlock grants, it does not subtract |
| `sandbox.network` | Permit outbound TCP |

A server never gets blanket home-directory access. What it may reach is
exactly its `allowed_paths`.

`sandbox.network` cannot exceed the unit: a server asking for network in a
unit that never declared the `network` capability fails validation. The
unit's top-level capability list stays the complete account of what the
artifact can reach, which is the property that makes it worth reading
before running a stranger's unit.

---

## Transport

**stdio only.** It is the simplest transport and the only one that keeps a
server inside the sandbox. An SSE server is a network endpoint someone else
controls — a different trust decision, which deserves to be made explicitly
rather than inherited from a URL in a YAML file.

The client speaks JSON-RPC 2.0, one message per line, negotiating protocol
version `2024-11-05`. Responses are correlated by ID rather than by arrival
order, since a server may interleave notifications freely. Unparseable
stdout lines are skipped: real servers print banners before speaking
JSON-RPC, and treating the first one as a protocol failure is needlessly
brittle.

---

## Runtimes

NexusRun bundles no interpreter, which is the whole reason the binary is
one static file. A `npm:` or `node`-launched server therefore needs Node on
the machine, and a Python one needs Python. The error says so rather than
failing obscurely.

An `exec:` source pointed at a compiled server needs nothing at all, and is
the option that keeps the zero-dependency property intact.

---

## Commands

| Command | Purpose |
|---|---|
| `nexus tools install <ref\|dir>` | Fetch what a unit declares |
| `nexus tools update <ref\|dir>` | Re-fetch, discarding the cache |
| `nexus tools list [ref\|dir]` | Cached servers, or what a unit declares |
| `nexus tools check <ref\|dir>` | Full lifecycle: start each, list its tools, shut down |

| Flag | Applies to | Meaning |
|---|---|---|
| `--force` | install | Re-fetch even if cached |
| `--mcp-debug` | check, run | Log every MCP frame in both directions |
| `--no-sandbox` | check, run | Start servers unconfined (trusted units only) |

`nexus tools check` injects the unit's secrets exactly as a run would. A
check that skipped them would pass where a real run fails.

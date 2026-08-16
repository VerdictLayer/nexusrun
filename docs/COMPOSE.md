# Agent Compose

Multi-agent workflows: a dependency graph of units, with routing
conditions, payload transforms, and a shared state bus, declared in one
`nexus-compose.yaml` that can be validated, versioned, and shipped as a
single OCI artifact.

There are two levels, and the smaller one is often enough.

**A pipeline** needs no file at all:

```bash
nexus compose summarizer:0.1.0 translator:0.1.0 --input "$(cat report.txt)"
```

Each unit's output becomes the next one's prompt. That is the whole model.

**A workflow** is for everything a straight line cannot express: a stage
that should only run if the previous one produced enough to work with, two
researchers feeding one writer, a payload that needs reshaping in between.

```bash
nexus compose init          # scaffold nexus-compose.yaml
nexus compose validate      # check it, run nothing
nexus compose up -i "..."   # run it
```

---

## The file

```yaml
apiVersion: nexusrun.dev/v1
kind: Workflow

name: content-pipeline
version: 1.0.0
description: Research a topic, then write it up

agents:
  researcher:
    unit: ghcr.io/acme/researcher:v1.2.0
    model: ollama:llama3.1:8b      # override the unit's own model
    hardware:
      prefer: [gpu, cpu]
    max_tokens: 400
    env:
      MAX_DEPTH: "3"

  writer:
    unit: ghcr.io/acme/writer:v2.0.0
    restart: 2                      # retry twice if it crashes
    env:
      TONE: technical

  editor:
    unit: ghcr.io/acme/editor:v1.0.0

routing:
  - from: researcher
    to: writer
    condition: "len(researcher.output) > 200"
    transform: |
      Write an article from these notes:

      {{ summarize 2000 .researcher.output }}

  - from: writer
    to: editor

shared_state:
  backend: file                     # memory | file
  path: ./.nexus/state.jsonl
  encryption: aes256-gcm            # optional

network:
  isolation: process                # process | namespace | none
  allow_loopback: true
```

### Agents

| Field | Meaning |
|---|---|
| `unit` | Unit reference: a local `name:version`, a registry reference, or a directory |
| `model` | Override the unit's own model for this workflow |
| `hardware.prefer` | Ordered accelerator preference: `npu`, `gpu`, `cpu` |
| `depends_on` | Ordering that is not also data flow |
| `env` | Environment for the agent |
| `max_tokens` | Per-agent generation cap; falls back to `--max-tokens` |
| `restart` | Retries before the workflow gives up on this agent |
| `max_memory_mb`, `max_cpu_percent` | Resource ceilings where the platform can enforce them |

An agent name is also an identifier in conditions and transforms, so it
must start with a letter or underscore and contain only letters, digits,
and underscores.

### Execution order

The dependency graph is `depends_on` **plus** every routing edge — a route
from A to B means B cannot start before A. Most workflows therefore declare
only their routes. Order within the graph is alphabetical, so a workflow
runs in the same order every time, on every machine.

Agents run **one at a time**, even where the graph would allow parallelism.
On the hardware NexusRun targets, two resident models is the difference
between a workflow that runs on a 4 GB Pi and one the OOM killer stops, and
inference already saturates the accelerator.

A cycle is rejected at validate time, with the path:

```
dependency cycle among agents: a → b → c → a
```

### Routing

A route moves one agent's output to another, if its condition holds.

- **Source agents** — those nothing routes into — receive the workflow's
  `--input`.
- **Several inbound routes** concatenate in declaration order, separated by
  a blank line.
- **A route that does not fire** skips the target agent. That is a normal
  outcome, reported as `skipped` rather than as an error, and it propagates:
  an agent whose only upstream was skipped is skipped too.

---

## Conditions

A small expression language — deliberately small. A routing condition needs
comparisons, a few string predicates, and boolean combination; a general
expression evaluator would bring a language, including the parts that turn
a condition into an execution vector for text a model produced.

**Values.** Every agent that has run exposes:

| Field | Type |
|---|---|
| `output` | text |
| `tokens` | number |
| `tok_per_sec` | number |
| `took_ms` | number |
| `device` | text (`gpu`, `cpu`, `npu`) |
| `backend` | text |
| `ok` | boolean |

Plus `input` (the workflow's input), and the constants `always`, `never`,
`true`, `false`.

**Operators.** `==` `!=` `<` `<=` `>` `>=`, `and`/`&&`, `or`/`||`,
`not`/`!`, parentheses. Numbers may use `_` separators (`100_000`).

**Functions.** `len`, `words`, `lines`, `contains`, `matches`, `lower`,
`upper`, `trim`.

```yaml
condition: "always"
condition: "len(researcher.output) > 200"
condition: 'contains(researcher.output, "CVE") or researcher.tokens > 500'
condition: 'matches(writer.output, "^# ") and not contains(writer.output, "TODO")'
condition: "researcher.tok_per_sec > 10 and researcher.device == 'gpu'"
```

`and` short-circuits, so `writer.ok and len(writer.output) > 5` is safe
when `writer` never ran.

Everything is checked by `nexus compose validate` — a name that is not an
agent, a field that does not exist, a function that is not defined, and the
wrong number of arguments all fail before anything runs:

```
routing[0] (a → b): condition: agents have no field "putput"
  (available: output, tokens, tok_per_sec, took_ms, device, backend, ok)
```

---

## Transforms

A transform reshapes the payload before delivery. It is Go
[`text/template`](https://pkg.go.dev/text/template) over the same values,
with a fixed function map and nothing else — no filesystem, no process, no
network.

```yaml
transform: |
  Write an article from these notes:

  {{ summarize 2000 .researcher.output }}
```

Note the leading dot: agent values are template fields, so it is
`.researcher.output`, not `researcher.output`.

| Function | Signature |
|---|---|
| `summarize` | `summarize <n> <text>` — truncate to n characters on a word boundary, marking the cut |
| `trim`, `lower`, `upper` | `<fn> <text>` |
| `head` | `head <n> <text>` — first n lines |
| `indent` | `indent <n> <text>` |
| `replace` | `replace <old> <new> <text>` |
| `default` | `default <fallback> <text>` — fallback when text is blank |
| `quote` | `quote <text>` |

Referencing an agent that never ran is an error, not an empty string.

---

## Shared state

Every delivery is recorded on a bus: the payload, the condition that let it
through, whether it was transformed, and the conditions it was generated
under (tokens, throughput, backend, device).

```json
{
  "id": "msg_f131740c9ff7",
  "from": "researcher",
  "to": "writer",
  "timestamp": "2026-08-16T00:16:25Z",
  "payload": {
    "content": "…",
    "metadata": {"tokens_used": 141, "tokens_per_sec": 6.46,
                 "runtime": "llama.cpp", "device": "cpu"}
  },
  "routing_context": {"condition": "len(researcher.output) > 80",
                      "condition_matched": true, "transform_applied": true}
}
```

| Backend | Behaviour |
|---|---|
| `memory` | Fast, lost when the process exits. The default. |
| `file` | Append-only JSONL, flushed per message, `0600`. Survives a crash. |

Read it back with `nexus compose logs`, which needs the `file` backend —
the memory bus never leaves the process that ran it.

### Encryption

`encryption: aes256-gcm` encrypts every record with a key derived from
`NEXUS_STATE_KEY`. The bus holds whatever the agents said to each other,
which on a shared machine is usually the most sensitive thing a workflow
leaves behind.

```bash
export NEXUS_STATE_KEY="…"
nexus compose up
nexus compose logs
```

Each record is `base64(nonce ‖ ciphertext)` on its own line, so a partial
write costs one message rather than the log. The key is the SHA-256 of the
passphrase — a hash, not a password-hardening KDF. It defends a state file
against a casual reader on the same box; it is not a defence against an
offline attack on a weak passphrase.

Without the variable set, the run refuses to start. With the wrong key,
reading fails loudly rather than returning empty history.

### Why not SQLite

The roadmap named SQLite as the default backend. Every pure-Go SQLite
driver is larger than this entire binary, and the cgo ones break static
cross-compilation to the 32-bit ARM targets NexusRun exists to reach. The
`file` backend provides what SQLite was wanted for — durable, single-file,
survives a crash — at no size cost. `backend: sqlite` fails validation with
that explanation rather than being silently accepted.

---

## Running in the background

```bash
nexus compose up -d              # detach
nexus compose ls                 # what is running
nexus compose logs <name> -f     # follow the bus
nexus compose down <name>        # stop it
```

There is no daemon and no supervisor. `up -d` starts a detached process and
records its pid; `down` reads that record and sends it `SIGTERM`. A record
whose process has exited is pruned as it is read, so `ls` never lists a
workflow that already finished.

A detached run requires `backend: file` — with an in-process bus there
would be nothing for `logs` to read.

---

## Packaging

```bash
nexus compose build                                  # into the local store
nexus compose build -t ghcr.io/acme/pipeline:v1.0.0  # and push
nexus compose pull ghcr.io/acme/pipeline:v1.0.0
nexus compose up -f pipeline:1.0.0
```

Only the workflow file travels. The units it names are resolved from the
registry at run time: they are already content-addressed artifacts, and
embedding copies would freeze a workflow to unit revisions its author never
pinned.

A workflow is its own OCI artifact type
(`application/vnd.nexusrun.workflow.v1`), so `nexus run` refuses one by
type rather than half-executing it:

```
wf:0.1.0 is a workflow, not a unit — run it with `nexus compose up -f wf:0.1.0`
```

---

## Commands

| Command | Purpose |
|---|---|
| `nexus compose <unit> <unit>…` | Sequential pipeline, no file needed |
| `nexus compose init [dir]` | Scaffold a `nexus-compose.yaml` |
| `nexus compose validate [file]` | Check without running |
| `nexus compose up` | Run |
| `nexus compose down [name]` | Stop a detached run |
| `nexus compose ls` | List detached runs |
| `nexus compose logs [name]` | Read the state bus |
| `nexus compose build` | Package as an OCI artifact |
| `nexus compose push <ref> <registry-ref>` | Push |
| `nexus compose pull <registry-ref>` | Pull |

### `nexus compose up`

| Flag | Default | Meaning |
|---|---|---|
| `-f, --file` | `nexus-compose.yaml` | Workflow file, directory, or stored reference |
| `-i, --input` | — | Input delivered to every source agent |
| `--input-file` | — | Read the input from a file |
| `-n, --max-tokens` | `256` | Default for agents that declare none |
| `-d, --detach` | `false` | Run in the background |
| `--only` | — | Run just these agents and their dependencies (repeatable) |
| `--stages` | `false` | Print every agent's output |
| `--json` | `false` | Emit the full result as JSON |

`--only` pulls in dependencies automatically: an agent's prompt comes from
upstream, so running it alone would feed it the workflow input instead.

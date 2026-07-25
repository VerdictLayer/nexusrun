# Tools

A model plus a prompt is a chat. A model plus a prompt plus tools it may
invoke is an agent — and it is the second thing that makes a unit worth
packaging as an artifact, because the model, the instructions, the tool
schemas, and the permissions each tool needs become one versioned,
digest-addressed object instead of four paragraphs of README.

## What works today, and what does not

**Working:** a unit declares tools; they are validated at build time; the
runtime schedules the unit onto a backend that can carry tool calls; the
tools are offered to the model; and when the model asks to call one, the
request is reported exactly as it was made.

```
$ nexus run ./notes-agent -p "Which invoices are mentioned in my notes?"
using existing model at /usr/share/ollama/.ollama/models/blobs/sha256-667b0c1932bc…
running notes-agent:0.1.0 on CPU via llama.cpp/server
offering 1 tool(s): search_notes
nexus: the model asked to call search_notes({"query":"invoice"}), but tool
execution is not implemented yet — see docs/TOOLS.md
```

**Not working yet:** executing the tool and feeding its result back. That
is the next piece of work, and it is deliberately separate — the sandboxing
it needs is the part worth getting right slowly.

Until it lands, a unit that declares tools **fails** when the model calls
one, with a non-zero exit status. It does not quietly answer as though the
tools were not there. That distinction is the whole reason this stage is
shippable: you can already tell whether your schema and description get the
model to ask for the right tool with the right arguments, which is where
most of the iteration actually is.

## Declaring a tool

```yaml
apiVersion: nexusrun.dev/v1
name: notes-agent
version: 0.1.0

models:
  - source: ollama:llama3.1:8b
    context: 4096

entrypoint:
  type: chat
  system_prompt: |
    You answer questions about the user's notes. When a question needs
    information from the notes, call search_notes rather than guessing.

capabilities: [storage]

tools:
  - name: search_notes
    description: Search the user's notes for a phrase and return matching lines
    parameters:
      type: object
      properties:
        query:
          type: string
          description: The phrase to search for
      required: [query]
    exec:
      type: script
      command: ["python3", "tools/search.py"]
    capabilities: [storage]
```

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | 1–64 characters of letters, digits, `_` or `-` — the syntax every tool-calling backend accepts |
| `description` | yes | How the model decides whether to call it |
| `parameters` | no | JSON Schema object, passed to the model untouched; omit for a tool taking no arguments |
| `exec.type` | yes | `script` — the only kind for now |
| `exec.command` | yes | argv, resolved relative to the unit directory |
| `capabilities` | no | What this tool may do; a subset of the unit's own |

The description is required because it is not decoration: it is the only
thing the model selects on. An undescribed tool is simply never called,
which reads as a broken runtime rather than a missing sentence.

### Capabilities narrow, never widen

A tool's `capabilities` must be a subset of the unit's. A unit that
declares `capabilities: [storage]` cannot contain a tool that asks for
`network`.

This keeps a useful property: the unit's top-level capability list is the
complete account of what the artifact can reach. That is the list worth
reading before running a stranger's unit, and it stops being worth reading
the moment something nested can exceed it.

The intended enforcement, when execution lands, is per call: each tool
invocation runs as its own process under its own Landlock policy, so a tool
that never declared `network` cannot open a socket even though the unit as
a whole may. That is the piece no other agent packaging format has, and it
is the reason to build the executor carefully rather than quickly.

## Which backends can carry tool calls

Tool calling needs a chat-completions endpoint. `llama-cli` does not have
one, so it cannot do it at all — and rather than coaxing tool syntax out of
it by asking the model to emit JSON in prose, it refuses:

```
$ nexus doctor
Tool calling
  llama.cpp        unsupported  no tool-call support in this backend
  llama.cpp/server ready        native tool calls
  ollama           ready        native tool calls
```

A unit that declares tools is therefore scheduled over a smaller candidate
set than one that does not. If nothing on the host can carry tool calls,
the run fails with that as the stated reason.

`llama-server` ships in every llama.cpp build, right beside `llama-cli`, and
NexusRun finds it as a sibling — so on most machines this needs no setup.

### The `--jinja` detail

`llama-server` only applies the model's own Jinja chat template when started
with `--jinja`. Without it, it falls back to built-in templates that have no
notion of tool calls, and the tools are silently ignored.

NexusRun passes `--jinja` for tool requests, and keys warm server instances
on it, so a tool-calling request is never answered by a server started
without it — and a plain chat request never has its templating changed
because a tool run happened first.

Whether the *model* was trained for tool calling is a separate question no
server can answer up front. A model without tool training simply never
emits a call; you find out by trying, which is what the stage above is for.

## Limits worth knowing

- **Execution is not implemented.** The model's request is reported and the
  run fails. See the top of this document.
- **The warm daemon cannot carry tools.** `nexus serve` has no tool path, so
  `nexus run` bypasses a running daemon for units that declare tools, and
  the daemon refuses them outright rather than answering without them.
- **Ollama cannot be selected by `nexus run`,** despite supporting tools
  natively: `run` executes weights by path and Ollama addresses models by
  name. Tool units therefore need `llama-server` today. The tool wire format
  for Ollama is implemented and tested — only the scheduler cannot reach it.
- **No MCP.** `exec.type` has one value. An `mcp:` type is the natural third
  after `script`, but a stdio MCP client is a large surface and native
  script tools should prove the loop first.
- **One turn.** The engine can carry a full conversation, including tool
  results, but nothing yet drives more than one turn of it.

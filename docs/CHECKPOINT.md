# Sessions and checkpoints

An agent that remembers, and state you can carry to another machine.

```bash
nexus run my-agent -s work -p "the deploy key lives in vault/prod"
nexus run my-agent -s work -p "where did I say the deploy key was?"

nexus checkpoint save work -o work.state.nx --encrypt
# …on the robot, the laptop, the machine that never sees the internet:
nexus run my-agent --restore work.state.nx -p "carry on"
```

---

## Sessions

Every `nexus run` used to be one shot: prompt in, answer out, nothing kept.
That is still the default. `--session <name>` makes a run part of a
continuing conversation instead.

```bash
nexus run my-agent -s work -p "…"     # start or continue "work"
nexus session ls                       # what exists
nexus session show work                # the full transcript
nexus session rm work
```

A session stores the ordered conversation, the unit and model it belongs
to, where it last ran, and a free-form `memory` map the runtime carries but
never interprets. It is plain JSON at `$NEXUSRUN_HOME/sessions/<name>.json`,
mode `0600` — the most useful thing to have when an agent misbehaves is the
ability to read exactly what it was told.

**The system prompt is not part of the transcript.** It is applied fresh
from the unit on every run, so editing a unit's prompt changes what a
resumed conversation sends. Replaying the prompt a session was *started*
with would make a unit edit have no effect on work already in progress.

**Tool exchanges are kept whole.** An assistant turn that called a tool and
the tool result answering it are both in the transcript, so a resumed
conversation is one a backend will accept.

### Backend requirements

A session is a multi-turn conversation, which needs the chat-completions
endpoint. `llama-cli` can express exactly one system turn and one user
turn, so a session is scheduled onto `llama-server` or Ollama and says so
if neither is available. The same is true of any unit with tools.

### Trimming

`--trim N` keeps only the most recent N messages before generating.

The cut lands on an exchange boundary, not N messages back: severing an
assistant turn from the tool results answering it produces a conversation
most backends reject outright. If trimming would leave nothing, it does
nothing.

---

## Checkpoints

A checkpoint is a session in one portable `.state.nx` file — a gzipped tar
holding:

```
manifest.json        metadata, readable without restoring
conversation.jsonl   one message per line, greppable and diffable
memory.json          the agent's own state
context.json         system prompt, window, token counts
model/               the weights, only with --seal
```

```bash
nexus checkpoint save work                    # into the store
nexus checkpoint save work -o work.state.nx   # to a file
nexus checkpoint save work -o - > work.state.nx
nexus checkpoint list
nexus checkpoint inspect work.state.nx --transcript
nexus run my-agent --restore work.state.nx -p "carry on"
```

Restoring onto a machine that has never seen the session works: the
checkpoint carries everything except the weights, which are resolved
normally. A checkpoint made by a different unit still restores — carrying a
transcript to another agent is legitimate — but never silently, because the
system prompt it was produced under no longer applies.

### `--encrypt`

Seals the **whole archive** under `NEXUS_STATE_KEY`, manifest included. The
manifest names the unit and the model, which is exactly what someone
carrying a checkpoint on a USB stick may not want readable.

```bash
export NEXUS_STATE_KEY='…'
nexus checkpoint save work -o work.state.nx --encrypt
```

The wrong key fails loudly; no key at all says which variable to set,
rather than failing as a corrupt archive.

### `--seal`

Embeds the model weights, making the file self-contained for a machine that
cannot fetch them. This is measured in gigabytes — a llama3.1:8b checkpoint
is 4.5 GB — so it is never the default, and it logs what it is doing.

---

## Why there is no KV cache

The roadmap raised this as an open question. The answer turned out to be no,
for two independent reasons:

1. **It is not portable.** llama.cpp's KV cache does not survive a change of
   version, quantization, or architecture — which is precisely the set of
   things that differ between the machines a checkpoint moves between.
2. **There is no handle to capture.** This runtime drives backends as
   subprocesses and HTTP servers. There is no in-process cache to serialize.

Restoring a conversation instead costs one prompt re-ingest on the first
turn, and is correct everywhere. A cache that silently mismatched would be
fast and wrong, which is the worse failure.

---

## Commands

### `nexus session`

| Command | Purpose |
|---|---|
| `list` | Sessions, most recently used first |
| `show <name>` | Full transcript, including tool exchanges |
| `remove <name>` | Delete |

### `nexus checkpoint`

| Command | Purpose |
|---|---|
| `save <session>` | Write a `.state.nx` |
| `list [session]` | Saved checkpoints |
| `inspect <file>` | Metadata, without restoring |
| `remove <file>` | Delete from the store |

| Flag | Applies to | Meaning |
|---|---|---|
| `-o, --output` | save | Write here instead of the store (`-` for stdout) |
| `--encrypt` | save | Seal under `NEXUS_STATE_KEY` |
| `--seal` | save | Embed the model weights |
| `--transcript` | inspect | Also print the conversation |

### `nexus run`

| Flag | Meaning |
|---|---|
| `-s, --session` | Continue a named conversation |
| `--restore` | Resume from a checkpoint file |
| `--trim N` | Keep only the most recent N messages |

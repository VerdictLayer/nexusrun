# Studio: build a workflow on a canvas

`nexus studio` opens an editor for a workflow folder. You drag agents onto a
canvas, drag a model from your machine onto an agent, and drag from one agent to
another to send its output there. It edits the same `nexus-compose.yaml` that
`nexus compose validate` and `nexus compose up` use, so there is nothing to
export.

```bash
nexus studio my-workflow        # opens your browser on the editor
cd my-workflow
nexus compose up -i "Say hello in five words."
```

## What you do

| You | It writes |
|---|---|
| Click or drag **＋ Agent** | An entry under `agents:` and a chat unit in `agents/<name>/nexus.yaml` |
| Type in **Prompt** | `entrypoint.system_prompt` in that unit |
| Drag a model from **Your models** onto an agent | The unit's `models[0].source` (or the agent's `model:` when the agent uses a unit Studio cannot edit) |
| Drag from an agent's right dot to another agent | A `routing:` entry |
| Select a link and fill in **Send only when** or **Reshape what is sent** | The route's `condition` and `transform` |

Every change is saved to disk a moment later. The **YAML** button shows the file.

## Problems are shown where they are

Studio checks the workflow with the same code as `nexus compose validate`. A
problem is listed at the bottom with its place in the file, such as
`routing[0].condition`, and the agent or link it concerns is outlined in red.
Clicking a problem selects it. The same locations are available to scripts with
`nexus compose validate --json`.

## Your models

The left panel lists the models on this machine with their parameters,
quantization and size, read from each file's header: Ollama's models, models
NexusRun downloaded, and the `.gguf` files under folders you add (or pass with
`--model-dir`). Each shows whether this machine's memory can hold it. The verdict
is an estimate of memory for a 4096-token context and says nothing about speed.
The same list is available as `nexus models --json`.

## Files and what Studio will not do

- Studio reads and writes only `nexus-compose.yaml`, `agents/<name>/nexus.yaml`
  and `.nexusrun/studio.json` (where nodes sit) inside the project folder. It
  refuses paths that go through a symbolic link.
- It rewrites `nexus-compose.yaml` completely, which drops comments. The first
  time it changes a file it copies the original to `nexus-compose.yaml.bak`.
- If `nexus-compose.yaml` cannot be read as a workflow, Studio shows why and
  leaves the file untouched.
- It edits a unit's prompt and model only for chat units with one model. A script
  unit, or one that selects among several models, is shown as read-only; edit it in
  your editor. A unit file with problems is never rewritten.
- It does not run workflows. Run them with `nexus compose up` from the project
  folder.

## Security

The editor is a local web page, and web pages in your browser can reach loopback
addresses, so it is protected like `nexus serve`:

- It listens on a loopback address only, and refuses any other `--addr`.
- Every request needs the access token in the link `nexus studio` prints. The
  token is random for each run, is kept only in memory, and is removed from the
  address bar when the page loads.
- Requests are checked for a loopback `Host` and, for changes, a same-origin
  `Origin` and `application/json`, so another site cannot drive it.
- The page has a Content-Security-Policy that allows only its own script, and it
  shows names from model files and units as text, never as markup.
- There is no endpoint that reads an arbitrary file or runs a command.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `127.0.0.1:7718` | Listen address (loopback only) |
| `--model-dir` | none | Also search this folder for `.gguf` models (repeatable; or set `NEXUSRUN_MODEL_DIRS`) |
| `--no-browser` | `false` | Print the link instead of opening a browser |

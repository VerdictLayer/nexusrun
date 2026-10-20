# Studio: build a workflow on a canvas

`nexus studio` opens an editor for a workflow folder. You drag agents onto a
canvas, drag a model from your machine onto an agent, and drag from one agent to
another to send its output there. Press **Run** to watch each step work. Studio
edits the same `nexus-compose.yaml` that `nexus compose validate` and
`nexus compose up` use, so there is nothing to export.

```bash
nexus studio my-workflow          # opens your browser on the editor
nexus studio my-workflow --app    # or in its own window, without browser toolbars
nexus studio --workspace ~/NexusRun --app   # a list of workflows, and a button to start new ones
```

The first time you open it, a short welcome explains the four steps. Tick
**Don't show this again** (on by default) and it stays away; the **?** button
brings it back. The sun and moon button switches between light and dark, and the
choice is remembered.

## Many workflows: the workspace

With `--workspace <folder>` Studio opens on **Your workflows**: every subfolder of
that folder as a card, and a box to name a new one. Creating one makes the folder
and opens it; the **‹ Workflows** button in the toolbar switches between them, and
reopening Studio returns to the one you had open. Each workflow keeps its own
`nexus-compose.yaml`, `agents/` and layout inside its own folder.

Only direct subfolders of the workspace are workflows. A name is letters, digits,
dots, dashes and underscores, and a symbolic link is never treated as one.

## Start it from your application menu (Linux)

```bash
nexus studio --install-launcher                       # workflows in ~/NexusRun
nexus studio --install-launcher --workspace ~/flows   # or another folder
nexus studio --remove-launcher
```

This adds **NexusRun Studio** to your application menu. It opens the workspace in
its own window, and stops by itself shortly after you close that window. If the
usual port is taken, it uses a free one. On macOS and Windows, run
`nexus studio --app --workspace <folder>`.

## What you do

| You | It writes |
|---|---|
| Click or drag **＋ Add agent** | An entry under `agents:` and a chat unit in `agents/<name>/nexus.yaml` |
| Type in **Instructions** | `entrypoint.system_prompt` in that unit |
| Drag a model from **Your models** onto an agent | The unit's `models[0].source` (or the agent's `model:` when the agent uses a unit Studio cannot edit) |
| Drag from an agent's right dot to another agent | A `routing:` entry |
| Select a link and fill in **Only send when** or **Reshape what is sent** | The route's `condition` and `transform` |

**Start from an example** adds two linked agents with instructions and the
smallest chat model that fits in memory. Every change is saved to disk a moment
later. The **`</>`** button shows the file.

## Run it

**Run** opens a sheet: type what the first agent should work on and press Run.
Each agent lights up on the canvas as it works (working, done, skipped, failed),
the links that carried data turn green, and the sheet lists every agent with its
answer, what it was given, its time, tokens per second and device. **Stop**, or
closing the sheet, ends the run.

The same steps are available to scripts as one JSON object per line from
`nexus compose up --events`.

Agents run one at a time, so each result appears when that agent finishes, not
word by word.

## Test a model before you use it

Every model in **Your models** has **Test this model**. It loads the model the
same way a run does, asks it to reply with one word, and shows what happened:
whether it answered, how many tokens per second, how long it took, and on which
device, or why it failed. Only models in the list can be tested.

## Problems are shown where they are

Studio checks the workflow with the same code as `nexus compose validate`. The
toolbar says **Ready to run** or how many things need fixing; click it for the
list. Each problem names its place in the file, such as `routing[0].condition`,
and the agent or link it concerns is outlined in red. Clicking a problem selects
it. The same locations are available to scripts with
`nexus compose validate --json`.

## Your models

The left panel lists the models on this machine with their parameters,
quantization and size, read from each file's header: Ollama's models, models
NexusRun downloaded, and the `.gguf` files under folders you add (or pass with
`--model-dir`). Each shows whether this machine's memory can hold it. That
verdict is an estimate of memory for a 4096-token context and says nothing about
speed. The same list is available as `nexus models --json`.

## Files and what Studio will not do

- In a workspace, a project is named by a header and looked up as a direct child
  of the workspace folder; there is no way to name a path elsewhere.
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
- Run workflows from a project folder you trust: running one starts its units,
  exactly as `nexus compose up` does.

## Security

The editor is a local web page, and web pages in your browser can reach loopback
addresses, so it is protected like `nexus serve`:

- It listens on a loopback address only, and refuses any other `--addr`. If you
  do not pass `--addr` and port 7718 is busy, it takes a free port instead.
- Every request needs an access token. The link printed in your terminal carries
  it. When Studio opens a browser or an app window for you, that program's command
  line (which other accounts on the machine can read) carries only a one-time
  code, which the page exchanges for the token once and which expires after two
  minutes.
- Requests are checked for a loopback `Host` and, for changes, a same-origin
  `Origin` and `application/json`, so another site cannot drive it.
- The page has a Content-Security-Policy that allows only its own script, and it
  shows names from model files and units as text, never as markup.
- There is no endpoint that reads an arbitrary file. The only processes it starts
  are this same `nexus` program, for a run and for a model test, with fixed
  arguments, in the project folder, one at a time. The text you type goes in a
  private temporary file, not on a command line. A model test only accepts a
  model from the list.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `127.0.0.1:7718` | Listen address (loopback only) |
| `--model-dir` | none | Also search this folder for `.gguf` models (repeatable; or set `NEXUSRUN_MODEL_DIRS`) |
| `--app` | `false` | Open in its own window using Chrome, Edge, Chromium or Brave, if one is installed; otherwise your default browser |
| `--workspace` | none | A folder of workflows, instead of one workflow |
| `--quit-when-closed` | `false` | Stop shortly after the page is closed (for launchers with no terminal) |
| `--install-launcher` | `false` | Add NexusRun Studio to the application menu (Linux) and exit |
| `--remove-launcher` | `false` | Remove that menu entry and exit |
| `--no-browser` | `false` | Print the link instead of opening anything |

# Sandboxing

Script units run arbitrary code. NexusRun confines them with
[Landlock](https://landlock.io), the unprivileged Linux security module, so a
unit gets exactly what it declared and nothing more — enforced by the kernel,
not by convention.

## Declaring capabilities

```yaml
capabilities:
  - network    # allow outbound TCP
  - storage    # allow read/write in the user's home directory
```

Declare nothing and the unit is confined to its own directory with no network.
This is the default, and it is the recommended state for anything you did not
write yourself.

## What is enforced

Measured on Linux 6.17 with Landlock ABI v7:

| Unit declares | Write inside unit dir | Write to `$HOME` | Outbound TCP |
|---|---|---|---|
| *(nothing)* | allowed | **blocked** | **blocked** |
| `network` | allowed | **blocked** | allowed |
| `storage` | allowed | allowed | **blocked** |
| `network, storage` | allowed | allowed | allowed |

Reading system directories (`/usr`, `/lib`, `/etc`) stays permitted, because
otherwise no interpreter could start. Everything else outside the unit is
denied.

## How it works

Landlock restrictions are inherited across `exec` and cannot be dropped once
applied. NexusRun uses that directly:

1. `nexus run` computes the policy from the unit's declared capabilities.
2. It re-execs itself as a hidden helper (`nexus __sandbox-exec`).
3. The helper applies the Landlock ruleset **to itself**, then `exec`s the
   unit's interpreter — replacing the process, so no unconfined parent is left
   holding privileges.
4. The unit inherits an irrevocable policy.

The unit also receives a deliberately minimal environment — `PATH`, `HOME`
(pointed at its own directory), and `NEXUS_*` variables. Credentials sitting in
the parent environment are not passed through.

```
NEXUS_UNIT         name:version of the running unit
NEXUS_PROMPT       the prompt (also on stdin)
NEXUS_MODEL_PATH   resolved weights, if the unit declares a model
NEXUS_DEVICE       selected device
NEXUS_MAX_TOKENS   token budget
NEXUS_SANDBOXED    "true" when confinement is active
```

## Limits — read these before trusting it

- **Linux only.** macOS Seatbelt and Windows AppContainer are not implemented.
  On those platforms `nexus run` *refuses* to run a script unit rather than
  running it unconfined; `--no-sandbox` overrides, and says so.
- **TCP only.** Landlock's network rules cover TCP connect and bind. UDP,
  including DNS, is not restricted. Treat `network` as "cannot open TCP
  connections", not as a complete network jail.
- **No CPU, memory, or process limits.** A unit can still spin, allocate, or
  fork. Use cgroups or a VM if you need resource bounds.
- **No protection against a malicious interpreter.** If a unit can write to a
  directory that is on `PATH`, normal precautions apply.
- **`--no-sandbox` disables all of it.** It exists for debugging and for
  platforms without support. Do not use it on units you did not write.

Chat units (`entrypoint.type: chat`) do not execute unit-supplied code — they
only feed a prompt to a model — so they are not sandboxed.

## Verifying it yourself

Landlock support is reported by:

```bash
nexus doctor    # → Sandboxing: Landlock ABI v7 (filesystem + network)
```

To confirm enforcement rather than trusting the label, write a unit that
attempts a forbidden operation and check that it fails:

```python
# probe.py — entrypoint.script, with capabilities: []
import socket, pathlib, os
try:
    pathlib.Path(os.path.expanduser("~/escape.txt")).write_text("x")
    print("LEAK: wrote outside the unit")
except PermissionError:
    print("blocked, as expected")
```

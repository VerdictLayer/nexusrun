# Secrets and config

A unit is meant to be committed, pushed to a public registry, and read by
whoever is about to run it. An API key inside it is a key in git history
and in every registry that ever mirrored the artifact.

So the unit declares only that it *needs* a credential. The value lives on
the machine that runs the agent, encrypted, scoped to that agent and
optionally to that one device.

```yaml
# nexus.yaml — public, committed, pushed
secrets:
  - name: OPENAI_API_KEY
    required: true

  - name: SSL_CERT
    required: false
    mount_path: /etc/nexus/certs/ssl.pem

config:
  - name: MAX_RETRIES
    default: "3"
  - name: TIMEOUT
    default: "30"
    env: REQUEST_TIMEOUT
```

```bash
printf 'sk-…' | nexus secret set my-agent OPENAI_API_KEY --stdin
nexus secret check ./my-agent      # is everything it needs present?
nexus run ./my-agent -p "…"
```

`nexus inspect` shows the names a unit will reach for. It never shows a
value, or even whether one is stored — presence is itself information about
a machine.

---

## Secrets versus config

They are separated on purpose: a value with a default in the manifest is by
definition not a secret. Both are injected as environment variables and
share one namespace, so a collision between them fails validation rather
than becoming a coin flip at run time.

| | Where the value lives | Encrypted | In the artifact |
|---|---|---|---|
| `secrets` | local store | yes | only the name |
| `config` | the manifest | no | name and default |

A secret's key must be a valid environment variable name, because that is
what it becomes. A key that could not be one would be stored happily and
then silently never reach the agent.

---

## Where values are injected

| Consumer | How it receives them |
|---|---|
| Script units | Environment, on top of the small fixed set the runner builds |
| Script tools | Environment, on every tool invocation |
| MCP servers | Environment, at server start — usually the thing that needs the token |

A **required** secret that is not stored fails before anything starts. An
agent launched without its credentials does not fail cleanly; it fails
somewhere in the middle of a request against a third party, which is much
harder to read. An optional one is reported and the run continues.

### `mount_path`

Some secrets are files — certificates, key files — not tokens. `mount_path`
writes the value to disk for the run and sets the variable to the path.

The declared path is a **hint about the filename, never permission to write
where it says**. A unit declaring `/etc/nexus/certs/ssl.pem` gets a file
named `ssl.pem` in a per-run temporary directory, mode `0600`, and the
variable points at where it really landed. The file is removed when the run
ends, and the sandbox is told to allow reading exactly that path.

---

## Device scoping

A secret with no device applies everywhere. One with a device wins over the
global value on that machine — which is how a single fleet ships per-site
credentials without a per-site unit.

```bash
nexus secret set kiosk-agent API_KEY sk-shared --stdin
nexus secret set kiosk-agent API_KEY sk-store-a --stdin --device kiosk-01
```

The device is read from `NEXUS_DEVICE_ID`. Unset means only global secrets
apply.

---

## Rotation

```bash
printf 'sk-new…' | nexus secret rotate my-agent OPENAI_API_KEY --stdin
```

The previous value stays valid for a grace period (5 minutes) and is also
offered to the agent as `OPENAI_API_KEY_PREVIOUS`, so an agent mid-flight
can accept either instead of failing on whichever half it did not get.

A plain `set` over an existing key is **not** a rotation: there is no grace
period and the old value is gone immediately, which is what someone fixing
a typo means.

> The roadmap wanted hot reload of a running agent. The runtime has no
> long-lived agent process to signal — a run resolves secrets at start and
> exits — so rotation is what actually delivers the property that motivated
> it: a value can change without a coordinated restart, because the next
> run picks up the new one and anything still in flight keeps working.

---

## Encryption and the master key

Values are AES-256-GCM encrypted under a master key at
`$NEXUSRUN_HOME/master.key` (mode `0600`), generated on first use, or taken
from `NEXUS_MASTER_KEY` when set — which lets a fleet inject a key it never
writes to disk.

Generating on first use rather than demanding setup is deliberate: a store
nobody can write to is a store people work around by putting the key back
in the YAML.

The store file itself is `0600` too. **Key names are readable in it**; only
values are protected. That is the intended trade: the names are what make
the file auditable, and they are already public in the unit.

If the master key does not match, reads fail loudly. Silently returning
nothing would look like the secret was never set.

---

## Backups

```bash
export NEXUS_BACKUP_KEY='…'
nexus secret export my-agent -o secrets.backup.nx
nexus secret import secrets.backup.nx
```

A backup is re-encrypted under a **passphrase** rather than the master key,
so it is portable to a machine that does not have this machine's key.
Import re-encrypts under the local master key. Device scoping survives the
round trip.

---

## Audit log

Every operation is appended to `$NEXUSRUN_HOME/audit.log`: timestamp,
operation, agent, device, and key **name**. Never a value.

```bash
nexus secret audit my-agent
```

```
WHEN                 OP       AGENT          DEVICE     KEY
2026-08-15 17:56:50  inject   needs-secrets  kiosk-01   API_KEY
2026-08-15 17:56:50  set      needs-secrets  kiosk-01   API_KEY
2026-08-15 17:56:50  rotate   needs-secrets  —          API_KEY
```

A failure to write the log never fails the operation. Losing an audit line
is bad; refusing to start an agent because the disk is full is worse.

---

## What this is not

The master key sits on the same machine as the ciphertext. This protects a
secret store against a casual reader, a backup that leaks, and a unit file
in a public registry. It is **not** a defence against someone who already
has root on the box — they can read the key.

For that threat model you want the values to come from somewhere the
machine has to authenticate to. The roadmap sketches `nexus secret sync`
against AWS Secrets Manager, 1Password, and Vault; that is not implemented.

---

## Commands

| Command | Purpose |
|---|---|
| `nexus secret set <agent> <KEY> [value]` | Store or replace |
| `nexus secret rotate <agent> <KEY> [value]` | Replace with a grace period |
| `nexus secret list [agent]` | Names, scopes, versions — never values |
| `nexus secret remove <agent> <KEY>` | Delete |
| `nexus secret check <ref\|dir>` | What a unit needs versus what is stored |
| `nexus secret export [agent]` | Passphrase-encrypted backup |
| `nexus secret import <file>` | Restore a backup |
| `nexus secret audit [agent]` | Access log |

`set` and `rotate` take the value as an argument, `--file`, or `--stdin`.
Prefer the latter two: an argument lands in shell history.

| Flag | Applies to | Meaning |
|---|---|---|
| `--device` | set, rotate, remove, check | Scope to one machine |
| `--file` | set, rotate | Read the value from a file |
| `--stdin` | set, rotate | Read the value from stdin |
| `--expires-in` | set | Expire after a duration (e.g. `720h`) |

An expired secret reads as missing, so a required one fails the run and an
optional one is skipped.

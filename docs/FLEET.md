# Fleet: ship an agent update safely

Fleet connects NexusRun Cloud to local Linux computers. Publish one exact
agent version, let a small group try it first, and keep the working version
when a candidate fails its acceptance tests. Model inference runs on each
computer, not in Cloud.

A **host** is an enrolled computer. A **channel** is an update group, such as
`production`. A **canary** is a host that tests and activates an update before
the rest of its channel receives permission to activate it.

## Before you start

- Use an organization where you are an owner or administrator in NexusRun Cloud.
  Your plan's host limit applies to enrollment.
- Use Linux x86-64 hosts with a compatible CPU runtime and system libraries.
  This guide uses CPU inference; a GPU is not required.
- Install the NexusRun binary on the preparation computer and each host. Follow
  [the first-helper guide](QUICKSTART.md) for Go, Ollama, the small
  `qwen2.5:0.5b` download, and building NexusRun. Only the preparation computer
  needs Go and Ollama; enrolled hosts use the sealed weights directly.
- Prepare a reviewed managed CPU runtime. Follow **Build the CPU runtime**
  and **Import it** in [the offline walkthrough](OFFLINE_DEMO.md). Copy that
  same runtime archive to each host. Match the destination's CPU features and
  system libraries, not just its `x86_64` architecture.
- Keep an internet connection for enrollment, artifact downloads, and updates.
  An activated release can run while disconnected.

Fleet accepts sealed, single-model chat agents with an included acceptance
suite. An agent may use script tools and secrets (see
[Tools and secrets](#tools-and-secrets)); MCP servers and model
auto-selection are excluded. Test prompts and expected answers in the suite
travel inside the published artifact: use synthetic, non-private examples.

Run the commands below from the `nexusrun` source folder. Replace `YOUR_ORG`
with your organization slug and `PASTE_RUNTIME_ID_HERE` with the complete
identifier printed by `runtime list`. Do not paste a filesystem path there.

## 1. Package and upload the demo

On your preparation computer:

```sh
./bin/nexus build examples/fleet-demo --seal
./bin/nexus inspect fleet-demo:0.1.0
./bin/nexus login
./bin/nexus push fleet-demo:0.1.0 registry.nexusrun.dev/YOUR_ORG/fleet-demo:0.1.0
```

Keep the full `sha256:...` artifact digest shown by the build. A digest
identifies exact bytes; a version tag is a human-friendly label. Rollouts use
the digest, so changing a tag does not change an existing release.

The example's `nexus.yaml` selects the small model, CPU execution, and a Linux
target. Its `evals/suite.yaml` includes two simple checks: arithmetic and a
short greeting. Both must pass on a host before that host activates it. These
checks demonstrate delivery gates; they are not a general quality or safety
assessment of the model.

## 2. Enroll the hosts

On the first host, import the runtime archive you prepared:

```sh
./bin/nexus runtime import ./llama-demo.runtime.tar.gz
./bin/nexus runtime list
./bin/nexus runtime verify 'PASTE_RUNTIME_ID_HERE'
./bin/nexus login
./bin/nexus fleet join registry.nexusrun.dev/YOUR_ORG/fleet-demo --name host-01 --channel production
```

Repeat on the other hosts, choosing labels such as `host-02` and `host-03`.
Labels must be unique in the organization. Avoid names containing customer
information. One NexusRun home enrolls for one repository and channel; use a
separate private `NEXUSRUN_HOME` when intentionally maintaining another
independent installation on the same computer.

Enrollment writes a private, repository-scoped host credential locally. It
can check in and pull from its assigned repository; it cannot publish releases.
Enrollment also creates the host's own signing key and prints its fingerprint;
the private half never leaves the host (see
[Signed release records](#signed-release-records)).
Keep the NexusRun home private and do not copy it between hosts. After
enrollment, polling uses this credential rather than your administrator login.

## 3. Start a one-host canary rollout

On your preparation computer, replace both placeholders:

```sh
./bin/nexus fleet release registry.nexusrun.dev/YOUR_ORG/fleet-demo \
  --digest 'PASTE_SHA256_DIGEST_HERE' \
  --runtime 'PASTE_RUNTIME_ID_HERE' \
  --channel production --canaries 1
```

The first enrolled host in that repository/channel is the canary. With
`--canaries 2`, the first two enrolled hosts are selected. Selection is fixed
for the release; removing a selected canary pauses its rollout rather than
silently reducing the required approvals.

On **each host**, run:

```sh
./bin/nexus fleet sync
./bin/nexus fleet status
```

The canary downloads the exact artifact, verifies its bytes and pinned runtime,
and runs its tests through the real local CPU backend. It asks Cloud for
permission again after testing, then atomically changes its active release.
All selected canaries must report activation before other hosts are allowed
to activate. Every remaining host also runs the acceptance tests itself.

If a non-canary reports `canary`, run `sync` there again after the canary
finishes. A rejected candidate pauses further activation and preserves that
host's previous active release. Hosts that already activated are not
automatically moved backward: use a tested rollback when needed.

## 4. Ask the active agent a question

On an activated host:

```sh
./bin/nexus fleet run -p "What is 2 + 2? Answer with just the digit."
```

Expect `4` and a CPU backend summary. `fleet run` uses the activated artifact
and its exact runtime without contacting Cloud. `fleet status` also reads only
local state. Keep loopback networking enabled for the local model server.

For recurring update checks, leave this running in a separate terminal:

```sh
./bin/nexus fleet watch --interval 30s
```

Interrupt with Ctrl+C. A network outage leaves the local active release in
place; the watcher retries on its next interval. Only one Fleet operation uses
a given home at a time. A watcher encountering an active invocation retries
later, so update evaluation does not compete with it for model memory.

Updates affect subsequent `fleet run` invocations. Running an ordinary
`nexus run` reference does not select the Fleet active release.

## 5. Serve local applications

`fleet serve` does what `watch` does and also answers applications on the same
computer through an OpenAI-compatible endpoint:

```sh
./bin/nexus fleet serve
./bin/nexus fleet key
```

`serve` listens on `127.0.0.1:7720` (change it with `--addr`; only loopback
addresses are accepted). `fleet key` prints the API key, which stays the same
across restarts. Point any OpenAI client at `http://127.0.0.1:7720/v1` with
that key:

```sh
curl -s http://127.0.0.1:7720/v1/chat/completions \
  -H "Authorization: Bearer $(./bin/nexus fleet key)" \
  -H "Content-Type: application/json" \
  -d '{"model":"fleet-demo","messages":[{"role":"user","content":"What is 2 + 2?"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:7720/v1", api_key=FLEET_KEY)
client.chat.completions.create(model="fleet-demo", messages=[{"role": "user", "content": "Hello"}])
```

- Every answer comes from the active release: its system prompt, sampling
  settings, model and tools. A request's own system messages are added after
  the release's system prompt. The `model` field is not used to choose
  anything; `GET /v1/models` lists the active unit.
- Each response names the release that produced it in the `X-Nexus-Release`
  and `X-Nexus-Digest` headers, and the digest in `system_fingerprint`, so an
  application can log exactly which tested version answered.
- Requests that try to add their own `tools` are refused. Text content only.
- `stream: true` returns standard server-sent event chunks; the answer arrives
  once generation finishes, as one content chunk.
- The model stays loaded between requests. When an update arrives, `serve`
  unloads the current release, tests the candidate, and answers requests in
  the meantime with `503` and `Retry-After: 30`. The first request after
  activation loads the new release.
- `serve` is the one process using this NexusRun home while it runs; `fleet
  sync`, `watch` and `run` report that another fleet operation is in progress.
- The endpoint has the same protections as `nexus serve`: requests must carry
  the key, and web pages in a browser cannot reach it.

Fleet does not supervise your application process or restart in-flight work.

## 6. See the fleet in Cloud

Open your organization in the Cloud dashboard and select **Fleet**. It shows
host assignments, last check-in, reported hardware, active artifact/runtime,
acceptance counts, and release history. Use **Refresh status** to reload it.
Members can view status; owners and administrators can publish, pause, roll
back, and remove hosts. Enrollment is done on the host using the CLI.

The CLI view is:

```sh
./bin/nexus fleet list registry.nexusrun.dev/YOUR_ORG
```

Last check-in means the last report received, not continuous application
health. A disconnected host continues using its local snapshot and Cloud
retains its last reported state.

## 7. Update, pause, or roll back

Change `version` in `examples/fleet-demo/nexus.yaml` to `0.2.0`, adjust the
instructions or tests, rebuild with `--seal`, and push the new version. Create
a new `fleet release` with its new artifact digest and the runtime ID.

To pause further activation, copy the release UUID from the dashboard or
`fleet list`:

```sh
./bin/nexus fleet pause registry.nexusrun.dev/YOUR_ORG PASTE_RELEASE_ID_HERE
```

To roll back:

```sh
./bin/nexus fleet rollback registry.nexusrun.dev/YOUR_ORG/fleet-demo --channel production --canaries 1
```

Rollback creates a new canary rollout of the earlier active artifact and
runtime. Each host tests it again; rollback is not an unconditional bypass of
the gate. Keep the earlier runtime installed. A rejected release requires a
new rollout rather than overwriting its failure with a retry.

To revoke a host, use **Remove** in the dashboard or:

```sh
./bin/nexus fleet remove registry.nexusrun.dev/YOUR_ORG PASTE_HOST_ID_HERE
```

Removal blocks future check-ins and repository access, including already
issued host bearer tokens. It does not remotely delete or disable an offline
copy. Manage a lost computer through your normal device-security controls.

## Tools and secrets

A fleet agent can call script tools shipped in the unit and use secrets
stored on each host. [examples/fleet-tools](../examples/fleet-tools) is an
agent whose tool answers order-status questions and needs an access token.
Its acceptance tests ask about orders whose status appears only in the
unit's data file, so they pass only when the tool really ran.

Set the agent's secrets on every host before you release it:

```sh
./bin/nexus secret set order-desk ORDERS_TOKEN
```

Secret values stay on the host: they are never part of the artifact, a
check-in, or a release record. A host that lacks a required secret rejects
the release with the code `secrets` and prints which secret to set. Like any
rejection, this pauses the rollout, so set secrets everywhere first.

Tools run as they do with `nexus run`: each call in the unit's sandbox, with
only the capabilities the unit and tool declare, from a fresh private copy of
the release's files. A tool may use a program the host provides, such as
`/bin/sh` or `python3`. Each host runs the acceptance tests itself, so a host
without that program fails them and keeps its working release.

## Signed release records

Each host signs every result it reports with its own key: what it tested
(the artifact digest, runtime, and a digest of the acceptance suite), which
cases passed, and whether it activated or rejected the release. Cloud keeps
those signed statements and refuses a report that is unsigned, signed by a
different key, or says something different from what the host claims.

Download a release's record from the dashboard (**Signed record** on the
release) or the CLI, and verify it anywhere, offline:

```sh
./bin/nexus fleet record registry.nexusrun.dev/YOUR_ORG PASTE_RELEASE_ID_HERE -o record.json
./bin/nexus fleet verify record.json
```

`verify` checks every signature against the host's recorded key and checks
that each signed statement matches the release and the result shown. It
exits non-zero if any report fails. A record contains test case names and
pass or fail, never prompts, answers, secrets, or host paths.

The host keys in a record come from Cloud. To confirm a key is really a
host's own, compare the fingerprint `verify` prints with the `hostKey` in
`nexus fleet status` on that host (also printed by `fleet join`).

## Offline operation and storage

Fleet's control plane uses outbound HTTPS; it adds no inbound listener on a
host. An active release, its weights, and its managed runtime remain local.
Check-ins contain hardware totals, release IDs, acceptance counts and failure
codes, and the host's signed statement of the same with test case names—not
inference prompts, generated answers, local paths, or credentials.

Allow space for downloaded model layers, extraction during testing, and
retained releases. Each release has an isolated store; shared model weights
are not deduplicated across releases. Preserve active and previous stores
and their runtimes. Disk budgets increase with the number and size of updates.

For a permanently air-gapped destination, use a complete deployment bundle
and physical transfer as described in [the offline guide](OFFLINE_DEMO.md).
Fleet polling requires a connection to its control plane; sealing an artifact
does not create a transport into a disconnected network.

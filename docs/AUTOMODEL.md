# Auto-model selection

A unit can declare *what kind* of model it needs instead of naming one, and
let each machine measure which candidate actually clears the bar there.

The problem is the one that appears the moment the same agent ships to more
than one device. `llama3.1:8b` is the obvious choice on a workstation and
unusable on a Pi; `phi3` is the reverse. Nobody authoring the unit knows
which machine it will land on, and a hardcoded model is wrong on most of
them.

```yaml
models:
  - id: main
    profile: default
    context: 8192
    requirements:
      min_context: 8192
      tool_calling: required
      max_size_mb: 3000
      min_quality_score: 11
    candidates:
      - source: ollama:phi3:3.8b
        weight: 1.0
      - source: ollama:llama3.1:8b
        weight: 0.9
      - source: ollama:qwen2.5-coder:7b
        weight: 0.8
    selection_strategy: best_eval_score
```

```bash
nexus bench code-reviewer      # measure and report, without running the agent
nexus run code-reviewer -p "…" # selects, then runs
```

A model entry says either `source` (name a model) or `candidates` (select
one). Declaring both is rejected — picking one silently is the wrong kind
of helpful.

---

## What "quality" means

The unit's own eval suite, in `evals/`. Nothing else. That is what makes
this measurement rather than marketing: the suite travels inside the
artifact, so whoever pulls the unit runs the same cases the author did, on
their own hardware, against their own weights.

`min_quality_score` is a count of cases that must pass, out of that suite —
the roadmap's "11 out of 13". A count rather than a percentage because it
is the number a suite author actually reasons about, and the denominator is
not ambiguous when the suite ships with the unit.

A unit with **no** suite still selects, on throughput alone. That is a
legitimate profile — *any of these work, give me the fastest* — which is
why a missing suite is not an error. A suite that exists and does not
parse **is** an error: degrading to a throughput ranking there would answer
a different question than the requirements asked and report it as the same.

See [EVAL.md](EVAL.md) for writing suites.

---

## Requirements

Checked in two passes. The cheap ones first, because rejecting a candidate
on its size is free and evaluating it is minutes.

| Field | Checked | Meaning |
|---|---|---|
| `min_context` | before download | Candidate's context window (its own `context`, else the profile's) |
| `max_size_mb` | after resolve | Weights on disk |
| `tool_calling` | before measuring | `required` restricts measurement to tool-capable backends |
| `min_quality_score` | after evaluating | Cases that must pass |

`tool_calling: required` matters more than it looks: a model measured on a
backend that cannot carry tool calls would score well and then fail in
production. Run `nexus doctor` to see which installed backends can.

A candidate that cannot be fetched is skipped, not fatal — the point of a
candidate list is that some of them are absent on any given machine.

---

## Strategies

| `selection_strategy` | Picks |
|---|---|
| `best_eval_score` (default) | Highest pass rate; ties broken by `weight`, then throughput |
| `fastest_passing` | Fastest model clearing `min_quality_score` |
| `smallest_passing` | Smallest model clearing `min_quality_score` |

`fastest_passing` is what an interactive agent on a kiosk wants;
`smallest_passing` is for devices where resident memory is the binding
constraint. Both require `min_quality_score` — without a bar every
candidate qualifies and the choice would ignore quality entirely, so
declaring one without it fails validation.

`weight` is an author's prior: *these are equivalent on paper, but I trust
this one*. It breaks ties between equal scores. It cannot promote a
candidate over one that genuinely scored better — it is a preference, not a
thumb on the scale.

---

## The decision is shown, always

```
Auto-model (profile "default", strategy fastest_passing)

    MODEL                               SCORE      TOK/S      SIZE  NOTE
    ollama:qwen2.5-coder:1.5b-base        0/3       30.3    940 MB  rejected: scored 0/3, below min_quality_score 2
  → ollama:llama3.1:8b                    3/3       12.9   4693 MB  cached

Selected ollama:llama3.1:8b — fastest of 1 candidate(s) meeting the bar (12.9 tok/s, 3/3)
```

Every candidate appears, including the rejected ones and why. A selection
the user cannot audit is one they will replace with a hardcoded model the
first time it surprises them.

When nothing clears the bar, the error says what each candidate actually
scored, so the choice between lowering the bar and changing the shortlist
is an informed one.

---

## The benchmark cache

Measuring is expensive; the first run on a new device pays for it and every
run after is a cache read. In practice: minutes, then 40 ms.

Results live in `$NEXUSRUN_HOME/cache/benchmarks.json`, keyed by a
**machine fingerprint** — a hash of OS, architecture, CPU model and
features, RAM rounded to the nearest GB, and the detected accelerators.
It identifies a hardware profile, not a person: no serial numbers, no MAC
addresses, safe to publish beside a benchmark.

A cache belonging to a different machine is discarded rather than merged.
Home directories get copied, and a throughput number from a workstation
applied to a Pi is worse than no number at all. RAM is rounded because
reported `MemTotal` moves between boots and would otherwise invalidate
everything each reboot.

Each entry is keyed on unit, version, profile, and model, and pinned to the
unit digest it was measured against — a rebuilt unit with a changed prompt
is a different unit, and inheriting its predecessor's score would report a
measurement never taken.

Entries are trusted for 7 days by default. Long enough that repeat runs are
instant, short enough to notice a driver update, a rebuilt llama.cpp, or a
machine that has started thermal-throttling.

```bash
nexus bench cache show           # what this machine has measured
nexus bench cache clear
nexus bench export --format csv  # or json
nexus run <unit> --refresh-bench # ignore the cache and re-measure
nexus run <unit> --cache-ttl 24h
```

> The roadmap wrote `~/.nexus/cache/benchmarks.json`. The store actually
> roots at `~/.nexusrun` (or `$NEXUSRUN_HOME`), and splitting state across
> two home directories would break the first time someone set that
> variable.

---

## Interaction with the rest of the runtime

**The warm daemon is bypassed.** `nexus serve` has no selection path and no
single model to be warm with, so a unit with candidates always executes
directly.

**`--auto-model`** is an explicit opt-in. A unit declaring candidates
selects anyway — it has no `source` to fall back on — so the flag's real
job is to fail loudly when pointed at a unit that names its model outright,
rather than let you believe a selection happened.

**`--seal`** on an auto-selecting unit embeds *every* candidate. The target
machine has to measure them to choose, and an air-gapped device cannot
fetch what is missing. That is expensive and deliberate, and it is logged
per candidate.

**In a workflow**, give the agent an explicit `model:`. A workflow stage
running a unit that selects its own model would re-select per run;
`nexus compose up` says so rather than guessing.

---

## Commands

### `nexus bench [unit]`

With no argument, benchmarks one model's throughput across every usable
device (`--model` required). With a unit that declares candidates, measures
each against the unit's suite and reports the selection this machine would
make — the same decision `nexus run` makes, without running the agent.
Results are cached either way.

| Flag | Default | Meaning |
|---|---|---|
| `-m, --model` | — | Model to benchmark (no-unit form) |
| `-p, --prompt` | built-in | Prompt to use |
| `--runs` | `3` | Repetitions per device; median reported |
| `-n, --max-tokens` | `64` | Tokens per run |
| `--json` | `false` | Emit JSON |

### `nexus run` flags

| Flag | Default | Meaning |
|---|---|---|
| `--auto-model` | `false` | Require selection; error if the unit names its model |
| `--refresh-bench` | `false` | Re-measure instead of trusting the cache |
| `--cache-ttl` | `168h` | How long a cached benchmark stays trusted |

# Evaluating units

`nexus eval` scores a unit against a suite of cases and records the score
against the exact conditions that produced it: unit digest, model weights,
backend, device, host, and sampling settings.

That keying is the entire point, and it is what makes this different from
prompt-testing tools. Those assume a cloud API endpoint, where the only
variable is the prompt. A local agent has three more — which quantization
of the weights, which backend build, which device — and they change the
answers, not just the speed.

## The number a bare percentage hides

Same unit. Same model digest. One machine. Two backends:

```
Suite:    code-reviewer-basics (13 cases × 1 repeat(s))
Unit:     code-reviewer:0.1.0
Model:    ollama:phi3:latest · Q4_0 · 633fc5be925f
Host:     linux/amd64 · Intel(R) Xeon(R) W-10885M CPU @ 2.40GHz
Sampling: temperature 0.00 · max 128 tokens

  DEVICE   BACKEND              PASS     RATE   FLAKY       tok/s
  ───────────────────────────────────────────────────────────────
  CPU      llama.cpp/server    12/13    92.3%       0       13.57
  AUTO     ollama              10/13    76.9%       0       86.99

ollama/auto — 3 not passing:
  fail  finds-nil-map-write
      expected one_of "nil map | nil-map | not initialized | …"
  fail  stays-quiet-on-correct-code
      expected max_words "<= 60"
  fail  stays-quiet-on-correct-loop
      expected max_words "<= 60"

Best: llama.cpp/server/cpu at 92.3%
```

The faster path is the worse one. Ollama is **6.4× the throughput and 15
points below** on quality, with identical weights and temperature 0 on both
— the difference is in how each backend templates the prompt and handles
stop tokens. Neither number is wrong; a single number would have been.

The failing cases say what went wrong rather than only how much. Both
backends miss the same nil-map bug; Ollama adds two of its own by writing
paragraphs about code that is fine, where the unit's prompt asks for one
line.

This is the same argument `nexus doctor` makes about hardware, extended to
output: measure on the host, per backend, and report the parts separately
rather than blending them into one optimistic figure.

`--all-devices` is what produces the comparison. Without it, eval runs
where the unit would actually run.

## Is a cheaper model good enough?

`--model` is repeatable, and scores the same suite against each one. This is
the question people actually have about a local agent — not "which model is
best" in the abstract, but "how much model does *this job* need".

```bash
nexus eval code-reviewer:0.1.0 \
  --model ollama:deepseek-coder:latest \
  --model ollama:codeqwen:latest \
  --model ollama:llama3.1:8b \
  --model ollama:starcoder2:3b
```

Real output, same host, temperature 0, thirteen cases:

```
  MODEL                          PARAMS  QUANT         SIZE     PASS    RATE     tok/s
  ────────────────────────────────────────────────────────────────────────────────────
  ollama:llama3.1:8b             8.0B    Q4_K_M      4.6 GB    13/13    100%       6.3
  ollama:phi3:latest *           3.8B    Q4_0        2.0 GB    12/13     92%      14.0
  ollama:codeqwen:latest         7.3B    Q4_0        3.9 GB    12/13     92%       7.7
  ollama:deepseek-coder:latest   1B      Q4_0        740 MB    10/13     77%      38.1
  ollama:starcoder2:3b           3B      Q4_0        1.6 GB     3/13     23%      16.4

  * the unit's own model; the rest were supplied with --model
```

Three things worth taking from that table:

**A 2.0 GB model scores what a 3.9 GB model scores, 1.8× faster.** For this
agent, `codeqwen` is paying nearly twice the disk and nearly twice the wait
for nothing — and the unit's existing default is the one that wins.

**Size is not the axis — training is.** `starcoder2:3b` is more than twice the
size of the 740 MB model and scored a third as well. It is a base model, never
instruction-tuned, and a base model cannot follow a system prompt that says
"report only bugs and misleading names". No parameter count fixes that, and
no benchmark chart tells you so for *your* prompt.

**A suite too small will invent a finding.** An earlier version of this suite
had four cases, and on it the 740 MB model tied `phi3` at 75% each — an
attractive result, and false. Nine more cases separated them to 77% and 92%.
Four cases could not tell apart two models three sizes apart, so it reported
them as equivalent. Thirteen cases is not many, and it was already enough to
overturn the conclusion.

Every row is a full, separately saved report, so any of them can be diffed
later with `nexus eval diff`. Sizes are the weights layer only, which is why
they read a little smaller than `ollama list`.

## Suites live in the unit

A suite is a YAML file under `evals/` in the unit directory. Everything in
that directory is packed into the artifact's source layer, so suites travel
with the unit: whoever pulls it can rerun the evaluation instead of
trusting a number someone published.

```yaml
apiVersion: nexusrun.dev/v1
kind: EvalSuite
name: code-reviewer-basics
description: Does this unit still find real bugs and stay quiet about correct code?

max_tokens: 128           # per case; low is good, suites are slow

cases:
  - name: finds-index-out-of-range
    prompt: |
      Review this Go function:

      func last(xs []int) int {
          return xs[len(xs)]
      }
    expect:
      one_of: ["out of range", "index", "bounds", "panic"]
      not_contains: ["I cannot", "as an AI"]
```

| Suite field | Default | Meaning |
|---|---|---|
| `apiVersion` | — | `nexusrun.dev/v1` |
| `kind` | — | `EvalSuite`, so a suite and a unit manifest can't be confused |
| `name` | — | Identifies the suite in reports |
| `temperature` | `0` | See [Determinism](#determinism) |
| `max_tokens` | `256` | Per case; a case may override it |
| `case_sensitive` | `false` | String assertions fold case by default |
| `system` | the unit's | Overrides the unit's system prompt for every case |
| `cases` | — | At least one |

| Case field | Meaning |
|---|---|
| `name` | Unique; it is the key a diff between two runs joins on |
| `prompt` | The user turn |
| `system` | Overrides the suite's and the unit's system prompt |
| `max_tokens` | Overrides the suite's cap |
| `expect` | At least one assertion |

Unknown fields are rejected, as in `nexus.yaml`. A mistyped assertion key
would otherwise become a case with nothing to check — which passes,
silently inflating every score it appears in.

Resolution is deliberately narrow: `evals/suite.yaml` if present, otherwise
the only `*.yaml` in `evals/`. Anything ambiguous is an error naming the
candidates, because which suite produced a score must not be a guess.

## Assertions

Every populated assertion must hold for the case to pass.

| Assertion | Passes when |
|---|---|
| `contains: [a, b]` | Every listed substring appears |
| `not_contains: [a, b]` | None of them appear |
| `regex: [expr]` | Every pattern matches somewhere |
| `equals: text` | The whole output, trimmed, matches |
| `one_of: [a, b]` | At least one appears |
| `json: true` | Output parses as JSON (a wrapping ``` fence is stripped first) |
| `max_words: n` | Output is at most n words |

Patterns are compiled during validation, not during the run — the
alternative is discovering a typo after twenty minutes of generation.

Write assertions **tolerant about wording and strict about behaviour**. A
review that describes the right bug in unexpected words is a pass;
tightening the wording measures the phrasing of one model rather than the
usefulness of the unit. `not_contains: ["I cannot", "as an AI"]` is the
highest-value assertion in most suites: refusal and hedging is how a
smaller quantization usually fails, and it is invisible to a `contains`
check.

There is **no model-as-judge**, and that is a decision rather than an
omission. A judge needs a second model whose own quality varies with the
same quantization and hardware this command exists to measure, so it cannot
be the instrument. Mechanical checks on a well-chosen prompt measure less,
but they measure it honestly.

## Determinism

Temperature defaults to **0**. Sampling noise turns a suite into a coin
flip — cases pass on Tuesday and fail on Wednesday with nothing changed,
which trains people to ignore the result.

Raise it only to deliberately measure stability, and pair it with
`--repeats`:

```bash
nexus eval ./my-agent --repeats 5
```

Above one repeat, a case that does not pass **every** repeat is reported
`flaky`, and flaky does not count as passed. A case that succeeds three
times in five tells you the unit is unreliable; averaging that away is how
flaky agents reach production looking fine.

## Comparing runs

Every run is saved. The useful question is rarely "what is the score" but
"did my change make it worse":

```bash
nexus eval ./my-agent --compare          # against the latest saved run for this unit
nexus eval list
nexus eval diff <before-id> <after-id>
```

```
llama.cpp/server/cpu  75.0% → 50.0% (-25.0 pts)
  - finds-nil-map-write          pass → fail
  + flags-misleading-name        fail → pass
```

Only cases that moved are shown. Leaving `pass` is a regression —
**including becoming flaky**, since a case that now passes only sometimes
has stopped being evidence that the unit works.

A diff states when the two runs are not comparable: a different suite, a
different unit build, different model weights, or a different temperature.
A rebuilt unit keeps its `name:version` while its contents change, so the
digest is the only thing that catches "same ref, different unit".
Comparing two measurements of different things is how misleading numbers
get published, so the mismatch is printed rather than hidden.

## As a gate

```bash
nexus eval my-agent:0.1.0 --fail-under 80
```

Exits non-zero when the best target scores below the threshold, which makes
a suite usable in CI or a pre-release check. Note that it gates on the
*best* target: use `--device` or `--backend` to gate on a specific one, or
`--all-devices` and read the JSON if every target must clear the bar.

## Limits worth knowing before trusting the number

- **Chat units only.** Script units run their own program rather than
  generating from a prompt; `nexus eval` refuses them rather than
  pretending.
- **Deterministic assertions only** — see the note on judges above. Suites
  measure whether a unit does mechanically checkable things, not whether
  its prose is good.
- **All cases count equally.** There is no weighting, so a suite of
  fifteen easy cases and one critical one reports 94% when the critical
  one breaks. Split suites instead.
- **A small suite is a misleading one.** With four cases each is worth 25
  points, so one lucky answer reads as a real difference. The example suite
  overturned its own headline finding when it grew from four cases to
  thirteen. Write more cases than feels necessary before quoting a number.
- **Quantization is a filename hint.** Weights borrowed from Ollama are
  stored under their digest with no filename to read, so the quant column
  says `unknown` there. The digest still pins the exact weights.
- **Targets run sequentially.** A three-target sweep takes three times as
  long; nothing is parallelised.
- **GPU quality is unmeasured here.** The development machine's llama.cpp
  build has no CUDA compiled in, so every number in this document is CPU
  or Ollama-placed. The mechanism is device-agnostic; the GPU column has
  simply never been filled in honestly. Same caveat as the rest of the
  project — see the Status section of the README.

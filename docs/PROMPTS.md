# Prompt management — two modes

Every LLM instruction in the app is declared once in `internal/prompt` as a `Def`,
and resolved at use time rather than referenced as a Go constant. That indirection
buys one thing: the active text can come from somewhere other than the binary.

```
-prompts=code      compiled-in defaults          (default; offline; what validate.sh gates)
-prompts=langfuse  Langfuse Prompt Management    (fetched once at boot)
```

---

## The five prompts

| Langfuse name | Handle | Variables |
|---|---|---|
| `voicesurvey/classify-turn` | `llm.ClassifyPrompt` | `question`, `reply` |
| `voicesurvey/question-gen` | `llm.SurveyPrompt` | `product`, `goalLine` |
| `voicesurvey/closing-line` | `ws.ClosingPrompt` | `product`, `transcript` |
| `voicesurvey/greeting-reply` | `ws.GreetingReplyPrompt` | `name`, `about`, `countClause`, `purposeClause`, `reply`, `timeOfDay` |
| `voicesurvey/insight-score` | `insight.ScorePrompt` | `product`, `endReasonLine`, `transcript` |

All five are **chat** prompts (system block, then any few-shot exchanges, then the
templated final user turn) and use Langfuse's `{{variable}}` syntax.

---

## Why code is still the default

`ModeCode` is not a fallback — it's the mode the project's guarantees are written
against.

- **Offline.** No credentials, no network, no behavior change. `./scripts/validate.sh`
  and both evals run exactly as before.
- **Reproducible.** A prompt that can change between two runs of the same commit
  makes an eval number meaningless.
- **No live-path network.** A voice turn has a latency budget measured in hundreds
  of milliseconds; a prompt fetch has no business inside it.

`ModeLangfuse` exists for the loop that code mode is bad at: **edit the wording in
the UI, restart, re-run the eval, compare.** No rebuild, no commit, and the two runs
sit side by side in Langfuse split by prompt version.

---

## The workflow

```bash
# 1. Register the binary's prompts as the "production" versions (once).
go run ./cmd/prompts push

# 2. Confirm what Langfuse serves vs. what's compiled in.
go run ./cmd/prompts list
#   PROMPT                             REMOTE   CODE FP    STATE
#   voicesurvey/classify-turn          v1       3f2a91c40e in sync
#   voicesurvey/closing-line           v2       800a15685b DIFFERS from code

# 3. Edit a prompt in the Langfuse UI, then run against it.
go run ./cmd/server -prompts=langfuse

# 4. Score the edit. The evals resolve through the same handles, so this measures
#    the Langfuse text, not the compiled-in one.
go run ./cmd/evalclosing
go run ./cmd/eval
```

`go run ./cmd/prompts show <name>` prints one compiled-in prompt verbatim — useful
for pasting into the UI or eyeballing a diff without a round-trip.

To promote a Langfuse edit back into the repo, copy it into the `Def` and re-push;
`push` then reports `unchanged` and the two agree again.

---

## Guarantees the implementation makes

**Push never spams versions.** `POST /api/public/v2/prompts` mints a new version on
*every* call — unlike the datasets endpoint, it does not upsert. So `EnsurePrompt`
reads the label's current version first and posts only on real drift. Whitespace-only
differences don't count: the UI is a text area, and a trailing newline is not a prompt
change worth a version.

**Fetch is once, at boot.** `obs.LoadPrompts` runs before the server accepts a
connection. Nothing re-fetches, so a prompt edit can never add latency mid-call or
change behavior halfway through a conversation.

**A load failure is fatal, by design.** Missing prompt, unreachable API, or an edit
that dropped a variable all stop the boot. There is deliberately no fallback to the
compiled-in default:

> Silently serving the old prompt while you believe you're testing the new one is
> the one outcome that makes the whole experiment worthless.

This is the opposite of how *tracing* degrades (a log line, never fatal) — and the
difference is the point. Losing a trace costs you a data point; loading the wrong
prompt invalidates the run.

**A dropped variable is rejected, not warned about.** Each `Def` declares the
`{{variables}}` the code supplies, and `prompt.Install` rejects an override missing
any of them. An edit that deletes `{{transcript}}` from the closing prompt would
otherwise ask the model to reference something specific the respondent said with
nothing to reference — and the output would read perfectly plausibly.

**Branching stays in Go.** `"two questions"` vs. `"a few quick questions"` is a
grammar decision, not a prompt-authoring one; a spoken agent can't recover from
getting it wrong. So the code computes those phrases and the prompt interpolates
them (see `greetingReplyVars`). Same for the insight transcript's numbering, which
is what keys the model's per-answer scores back onto the right question.

---

## How a prompt reaches a trace

Two identities land on every generation span, and they answer different questions.

- **Fingerprint** (`langfuse.trace.metadata.prompt_version`) — a 12-char content
  hash, always present. It's the only identity that exists in code mode, and being
  content-addressed it can't drift out of sync the way a hand-bumped number would.
- **Native link** (`langfuse.observation.prompt.name` + `.version`) — set only when
  the prompt came from Langfuse. This is what attaches the generation to the managed
  prompt in the UI and gives per-version metrics for free.

Prompt linking only works on **generation**-type observations, which all five call
sites are. `langfuse.trace.metadata.prompt_source` records which mode produced the
span, so a mixed history stays readable.

---

## Adding a prompt

1. `prompt.Register(prompt.Def{...})` in the package that owns it, with `Vars`
   listing every `{{placeholder}}` the code supplies.
2. Resolve it at the call site — `Handle.Resolve().Compile(vars)` — never capture
   the messages at init, or a Langfuse override installed at boot is invisible.
3. Stamp it on the trace with `obs.WithPrompt(ctx, resolved)`.
4. Import the owning package in `cmd/prompts` and bump the `-expect` default, or
   `push`/`list` will quietly under-report it. `TestRegistryCountMatchesDefaultExpect`
   fails if you forget.

`cmd/prompts` is the only test binary that imports every prompt-declaring package,
so whole-registry assertions live there: declared-vars-are-used, no-undeclared-
placeholders, names-are-namespaced.

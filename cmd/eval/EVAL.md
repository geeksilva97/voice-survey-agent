# Evaluating the turn classifier (`cmd/eval`)

This is the offline harness that scores the **per-turn intent classifier** — the
component that, on every respondent reply, decides whether the agent should
**advance** to the next question, **re-read** the current one, ask a **follow-up**,
or **end** the call.

That one decision is what makes a voice survey feel human or broken. Get it wrong
and the agent re-asks a question you already answered, bails the whole survey
because you said "nothing comes to mind," or says *"Got it —"* and advances on a
cough. This harness exists to catch exactly those regressions before they ship.

If you only remember one thing: **the classifier is the conversation's brain, and
this eval is its regression suite.**

---

## What the classifier produces

Each turn, `llm.ClassifyTurn(question, reply)` returns a `Turn` with three fields.
The eval scores all three, but treats them very differently.

**1. Intent** — one of six labels, and the primary thing we grade:

| Intent | Meaning | Agent behavior |
|--------|---------|----------------|
| `answer` | they answered the question | capture it, advance |
| `wants_stop` | they want to end the whole survey | thank + end (`bailed`) |
| `repeat` | didn't *hear* it, wants it read again | re-read verbatim |
| `needs_help` | heard it, but unsure *how* to answer | reassure + hint + re-pose |
| `off_topic` | genuinely unrelated aside | warm steer-back |
| `unintelligible` | noise / garbled STT / a cough | re-pose, never advance |

**2. Clarity** — `clear` vs `unclear`, only meaningful on `answer` turns. An
`answer`+`unclear` (a calque, heavy ESL, an ambiguous reference) triggers **one**
confirmation turn ("you said …, did I get that right?") before recording, so we
never silently store a misheard answer.

**3. Ack** — the short spoken lead-in ("Lavender, lovely —") the agent says before
its next line so it sounds like a person, not a form. Quality, not correctness.

---

## The metrics — what gates, what doesn't

Only **two** metrics can fail the build. Everything else is reported for insight
but never blocks.

### Gated (these set the exit code)

- **Overall intent accuracy** — fraction of all cases where the predicted intent
  matches the label. Threshold: **≥ 90%** (`-min-acc`).
- **Valid-answer acceptance** — of the cases that genuinely ARE answers, how many
  were classified `answer` **and** `sufficient`. Threshold: **≥ 95%**
  (`-min-answer`). This is the direct proxy for *"the agent doesn't re-ask a
  question I already answered."* We hold it deliberately high because a false
  follow-up is the most infuriating failure mode.

### Ungated (reported, never fails)

- **Clarity accuracy** — how often the clear/unclear axis is right (drives
  over/under-confirming). Tracks model strength; weak models rarely flag `unclear`.
- **Ack quality** — an LLM-judged score (see below). Purely a feel metric.

Why asymmetric? Because **the dangerous errors are one-directional.** Reading an
answer as something else loses real data; reading a weak ack is cosmetic. The gate
guards the former and merely watches the latter.

---

## The gate model vs. the comparison matrix

You can run several models at once. The harness prints a per-model detail block
(confusion matrix + failures + bad acks) and a final side-by-side matrix.

- The **first** model in `-models` is the **gate model** — its pass/fail alone
  sets the process exit code.
- Every other model is **comparison-only**: it shows up in the matrix but can
  never fail the build. This is what keeps CI green when a cloud model is down.

`scripts/validate.sh` pins the gate to the **local `qwen2.5:3b`** so the gate stays
fast, offline, and free. The stronger models (`glm-5.2:cloud`,
`gemma4:31b-cloud`, `claude-sonnet-5`) are run manually when we want to see the
ceiling — production classification runs on a strong model, but the gate proves
the floor holds even on a tiny local one.

> **Model routing** is by name: anything containing `claude`/`sonnet`/`opus`/
> `haiku` goes to the Anthropic API (key from `$ANTHROPIC_API_KEY`, falling back
> to pepita's `.env`); everything else goes through the local Ollama daemon (cloud
> models like `glm-5.2:cloud` are proxied by Ollama).

---

## The dataset (`dataset.go`)

A broad, **hand-labeled** corpus of `(question, reply, want, clarity)` cases. It is
not sampled from traffic — it is *engineered* to stress the exact confusions that
break real conversations. The strategy behind it:

- **Cover both axes across many product types** (candles, coffee, SaaS, apparel,
  restaurant) so nothing overfits one domain.

- **Phrasing variation is the point.** Every intent has brief, vague, uncertain,
  quirky, negative, and rambling variants — because respondents don't speak in
  clean sentences.

- **Regression anchors from real bugs.** Several clusters exist *because* we saw
  the failure live:
  - *"Nothing comes to mind" / "nope, all good"* → must be `answer`, not
    `wants_stop`. (The screenshot bug where the survey bailed after Q1.)
  - *A cough `(coughing)` / `(clears throat)`* → `unintelligible`, never an
    answer. (The *"Got it —"* advance-on-a-cough bug.)
  - *"Do you expect some score or something?"* → `needs_help`, not ignored.
  - World-Cup chit-chat as a **statement** → `off_topic`, not `answer`.

- **A dedicated calque / ESL block** (Brazilian-Portuguese literal translations:
  "banana vitamin" = smoothie, "price is salty" = expensive, "I pretend to come
  back" = intend to). These MUST stay `answer` but `unclear`, so the agent does a
  natural repair instead of silently recording nonsense.

- **`needs_help` is kept narrow on purpose.** The dataset contrasts it against
  vague/rambling/negative answers that look similar, because the expensive mistake
  is reading a real answer as "needs help" and losing it. When unsure, the safe
  direction is `answer`.

Clarity is only labeled (and scored) on `answer` cases; everything else uses `na`
so we don't grade an axis that has no meaning there.

---

## The acknowledgment judge (LLM-as-judge)

Ack quality can't be checked with string equality, so a **single fixed judge
model** (`-judge`, default `claude-sonnet-5`) grades each produced ack against the
ground-truth expectation: is it short, specific, spoken, non-repetitive, and — for
off-topic/needs-help — does it steer/reassure without engaging the tangent or just
re-reading the question?

Three deliberate properties:

- **One judge across all evaluated models**, so the ack score is comparable
  between them.
- **Ungated and best-effort** — if the judge can't be built (no key, offline) we
  just skip ack scoring; a judge glitch never counts against a model.
- **Judged on the expected case, not the model's own call** — it measures "does
  this model give good acks where we want them," independent of whether its
  intent/clarity was right.

---

## Running it

```bash
# Full comparison matrix — all defaultModels (needs Ollama + Anthropic key)
go run ./cmd/eval

# Just the local gate model, no ack judge — fast/offline (what validate.sh runs)
go run ./cmd/eval -models qwen2.5:3b -judge ""

# Compare a specific pair; the FIRST is the gate
go run ./cmd/eval -models claude-sonnet-5,qwen2.5:3b

# Tune thresholds / concurrency
go run ./cmd/eval -min-acc 0.92 -min-answer 0.96 -c 8
```

Exit code is `0` only if the **gate** model clears both thresholds. Reading the
output: start with the matrix (`acc` / `ans✓` are the gated columns; per-intent
columns are recall), then drop into a model's `failures:` block to see the exact
misclassified replies.

---

## Publishing a run to Langfuse (optional)

Everything above is a snapshot: the terminal prints the numbers and they scroll
away. With `LANGFUSE_PUBLIC_KEY` + `LANGFUSE_SECRET_KEY` set, the same run also
publishes itself as a **Langfuse dataset experiment**, which is what turns the
eval from a pass/fail gate into *history*:

- **This corpus becomes a dataset** (`-langfuse-dataset`, default
  `turn-classifier`). Items upsert by a content hash of `(question, reply)`, so
  re-pushing on every run is free and editing `dataset.go` only touches the items
  that actually changed.
- **Each model's pass becomes a run**, named `<model>@<run>` where `-run` defaults
  to `<prompt-version>-<unix>`. Models are separate runs over identical items,
  which is what Langfuse compares side by side.
- **Each case becomes a run item** linked to the trace that classification
  produced, carrying an `intent_correct` score — so you can filter a run down to
  just the misses and read the actual replies.
- **The headline metrics become run-level scores**: `intent_accuracy`,
  `answer_acceptance`, `clarity_accuracy`, `ack_quality`.

```bash
# name the run after what you changed, so the comparison reads clearly later
LANGFUSE_PUBLIC_KEY=… LANGFUSE_SECRET_KEY=… go run ./cmd/eval -run before-cough-guard
```

Every classifier span is tagged with `llm.ClassifyPromptVersion()` — a content
hash of the system prompt *and* the few-shot anchors. That's the link that makes
"did this score move because I edited the prompt?" answerable rather than
guessed: group runs by prompt version and the answer is in the chart.

The export is strictly additive — no credentials means no-op, and a Langfuse
failure logs and continues, so it can never change this eval's exit code.
Implementation and the API constraints it works around are in `internal/obs`.

## Extending it

- **Add a case:** append an `evalCase` to `dataset.go` under the matching section.
  Set `clarity` only for `answer` cases (`clear`/`unclear`); use `na` otherwise.

- **Adding a case is how you lock in a bug fix.** When a real conversation
  misbehaves, add the offending `(question, reply)` with its correct label *first*
  — it should fail, then pass once the prompt/guard is fixed.

- **Never reuse a dataset sentence as a few-shot** in the classifier prompt — that
  leaks the test set and inflates the score. **This is now enforced, not advisory**
  (`leak.go`): `TestNoLeakInCompiledPrompt` fails the gate, and the eval itself exits
  before spending a model call if the *active* prompt overlaps the corpus. The check
  runs against whichever prompt is installed, so it also covers an anchor authored in
  the Langfuse UI, which never passes through code review on its way in.

  The rule used to say the anchors were "intentionally novel sentences". They weren't:
  when the check was written it found **11 of 15 anchors repeating a corpus sentence
  verbatim**. The cause is benign and worth knowing, because it will happen again —
  fixing the cough bug meant adding `(coughing)` to the prompt (to teach the model)
  *and* to the corpus (to lock the fix). Same sentence, both places.

  Measured before the fix: leaked cases scored **11/11 (100%)** against **67/71
  (94.4%)** on the rest, so the reported 95.1% was carrying 11 gimmes. The anchors
  were rewritten as novel sentences that teach the same lesson (prompt says
  `(sneezes)`, corpus keeps `(coughing)`) and the corpus was left untouched, since
  those cases are regression anchors for real bugs.

- **If you raise a threshold**, confirm the local gate model still clears it —
  otherwise every CI run fails offline.

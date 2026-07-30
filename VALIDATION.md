# Validation runbook

**Policy: every change is revalidated. Run the automated gate after any change;
run the browser E2E after any client/voice change. If you add a step, document it
here in the same commit.**

Two layers:

1. **Automated gate** — `./scripts/validate.sh`. Everything checkable without a
   browser. Run on every change.
2. **Browser E2E** — a real Chrome run with a fake microphone. Run after any
   change to `web/` or the conversation protocol.

---

## 1. Automated gate — `./scripts/validate.sh`

```bash
cd poc
./scripts/validate.sh          # exit 0 = pass
```

Prerequisites: `ollama serve` running with `qwen2.5:3b`, and models present
(`./scripts/fetch-models.sh`). Steps skip (not fail) if Ollama/models are absent.

| # | Step | What it proves | Expected |
|---|------|----------------|----------|
| 1 | `go build ./...` | compiles | ok |
| 2 | `go vet ./...` | no vet issues | ok |
| 3 | `go test ./internal/survey/ ./internal/ws/` | state machine + ending logic; repair helpers (`isAffirmation`, `repairPrompt`) | ok |
| 4 | `go test ./internal/llm/ -run 'TestClassifyTurn\|TestClassifyQuirkyAnswer'` | "I have to go" → `wants_stop`; clear reply → `answer/sufficient`; quirky-but-on-topic reply → `answer` (not off_topic/unintelligible) | ok |
| 5 | `cmd/eval` | full labeled-corpus eval vs the live LLM (see below) | `EVAL PASSED` |
| 6 | `cmd/probe -mode happy` | full STT→LLM→state→TTS loop over WebSocket | `reason=completed` |
| 6 | `cmd/probe -mode silent` | silence backstop | `reason=silence` |

The gate starts a throwaway server on `:8099` and tears it down.

### Intent-classification eval — `go run ./cmd/eval`

The turn classifier (`answer` / `wants_stop` / `repeat` / `needs_help` /
`off_topic` / `unintelligible`) decides whether the agent advances, re-reads,
helps, follows up, or ends — so a misclassification is what makes a conversation
feel wrong (e.g. re-asking an already-answered question). `cmd/eval` scores it
against a broad hand-labeled corpus (`cmd/eval/dataset.go`, ~82 cases across candles/coffee/
restaurant/SaaS/apparel) using **live** models. Phrasings include brief, vague,
uncertain, quirky, negative, rambling, noise, and a deliberate block of
**broken/ESL/calque English** ("defiant" cases) that trip small models — e.g.
`"a banana vitamin would be awesome"` (a calque of PT "vitamina de banana", a
smoothie), `"the price is a little salty"` (salgado = expensive), `"I pretend to
come back next week"` (pretendo = intend).

**Multi-model by default** — it runs the whole `defaultModels` set and prints a
side-by-side comparison matrix. The **first** model is the *gate* model; its
pass/fail sets the exit code (comparison models never fail the gate, so a cloud
outage doesn't break CI).

```bash
go run ./cmd/eval                                   # all models → comparison matrix
go run ./cmd/eval -models qwen2.5:3b                # just the local gate model
go run ./cmd/eval -models glm-5.2:cloud,claude-sonnet-5   # a subset
```

Routing is by name: anything containing `claude`/`sonnet`/`opus`/`haiku` hits the
**Anthropic API** (key from `$ANTHROPIC_API_KEY`, else parsed from
`~/projects/pepita/.env` — the value is never printed); everything else goes
through the **local Ollama daemon** (cloud models like `glm-5.2:cloud` are
proxied by Ollama). Override the pepita path with `-pepita-env`.

**Two axes.** The classifier returns `intent` (the communicative act) *and*
`clarity` (did we understand the content precisely?). They're orthogonal: a
calque like "a banana vitamin" is `intent=answer` + `clarity=unclear`. Intent
drives control flow; clarity drives the agent's **repair** turn (see below). The
dataset labels clarity on answer-type cases only (`clear`/`unclear`); non-answer
cases are `na` (not scored).

Per model it prints a **confusion matrix** and **failures** (including
clarity-only misses); the matrix shows per-intent recall plus three headline
metrics:

- **Overall intent accuracy** (`acc`) — all six intents. **Gate: ≥90%.**
- **Valid-answer acceptance** (`ans✓`) — of replies that *are* answers, how many
  were `answer` **and** `sufficient`. Maps to "the agent doesn't re-ask answered
  questions". **Gate: ≥95%.**
- **Clarity accuracy** (`clar`) — did the model get the clarity axis right?
  *Informational, not gated* (it's fuzzier, and models err toward `clear` =
  under-confirm = safe). This is the axis that separates models.
- **Ack quality** (`ack`) — an **LLM-as-judge** score (ungated) of the
  acknowledgment the classifier produced (see "Acknowledgment layer" below).
  Generated text can't be graded by exact match, so a fixed judge model
  (`-judge`, default `claude-sonnet-5`) rates each ack on the cases where the
  agent would actually speak one — a *clear* answer (reflect-back) or an
  *off_topic* reply (warm steer-back). Good = short, specific to what they said,
  and (for off-topic) steers back without engaging the tangent. `validate.sh`
  passes `-judge ""` so the gate stays offline; the full `go run ./cmd/eval`
  turns it on. Like clarity, it's the strong models that score well and the 3B
  that mostly emits nothing (so the layer stays inert locally — safe).

Local Ollama models run at **temperature 0** (stable/repeatable labels);
Anthropic omits temperature (newer models reject it). Add any new
misclassification QA turns up to `dataset.go` in the same commit.

**Last run (2026-07-21, 73 cases; ack judged by `claude-sonnet-5`):**

| model | acc | ans✓ | clar | ack | notes |
|-------|-----|------|------|-----|-------|
| `qwen2.5:3b` (local, **gate**) | 98.6% | 100% | 67.4% | 10.3% | under-detects `unclear` **and** rarely emits an ack → repair + ack layers both stay inert (safe); 1 unintel→answer miss |
| `glm-5.2:cloud` | 100% | 100% | 84.8% | 76.9% | — |
| `gemma4:31b-cloud` | 100% | 100% | 97.8% | 82.1% | best at clarity **and** ack |
| `claude-sonnet-5` | 100% | 100% | 95.7% | 71.8% | strong; a few off-topic acks the (sonnet) judge dinged for referencing the tangent |

All models clear intent on the "defiant" calque set, accept 100% of valid
answers (incl. "nothing comes to mind"), and get every off-topic case (incl. the
World Cup tangents) right. Clarity and ack are where they diverge — the 3B is
weakest on both, so on the local model the repair AND acknowledgment turns mostly
stay inert (it just asks the plain question — the pre-feature behavior). Both are
ungated for exactly this reason (models err toward under-doing = safe).

### Conversational repair (understood-but-unclear answers)

When a reply is `answer` + `clarity=unclear`, the agent does **one** natural
confirmation before advancing — echoing the respondent's own transcribed words
("Sorry, I want to make sure I got that — you said '…'. Did I understand, or
could you say it another way?"). It is **capped at one per question** and
**fail-open**: the next reply is captured and the survey advances no matter what.
A bare "yes/right/exactly" keeps the original answer; anything substantive is
recorded as the correction (`isAffirmation` in `internal/ws/ws.go`, unit-tested).

Why echo their words instead of decoding the calque? It works on any model
(no need to guess "banana vitamin" = smoothie) and invites a correction if STT
misheard. Note: because the local 3B rarely flags `unclear`, this turn is best
observed on gemma/sonnet; on qwen it seldom triggers.

**Running the conversation on a stronger classifier.** The per-turn classifier
is separate from question generation, so you can keep generation local and run
classification on a bigger model (where the repair turn actually fires):

```bash
# question-gen stays local (qwen); each turn classified by sonnet
go run ./cmd/server -classify-model claude-sonnet-5
go run ./cmd/server -classify-model gemma4:31b-cloud    # or an Ollama cloud model
```

Anthropic models read the key from `$ANTHROPIC_API_KEY`, else `-anthropic-env`
(defaults to pepita's `.env`). Ollama/`:cloud` models need no key. Every turn
now costs a round-trip to that model, so expect a little more latency per reply.

### Greeting pre-layer (a named agent eases in like a person)

Sessions open the way a person eases into a call — **hello, listen, then get to
business** — as three beats, not one:

1. **Hello only.** A short, time-of-day-aware greeting with the agent's name
   (default **Ava**, `-agent-name`) and a "how are you" — and *nothing else*. No
   agenda, no product up front: *"Good afternoon! My name's Ava. How's your day
   treating you?"* `greetingLine` picks from curated templates at random (name +
   `timeOfDay` filled in), so the opener varies with zero latency. Templates are
   hand-written on purpose — the offline 3B was too weak to self-introduce
   reliably. Unit tested (`TestGreetingLine`, `TestTimeOfDay`).

2. **Reply + framing + consent.** The small-talk reply is first run through the
   turn classifier once (catch an early bail). If they didn't bail, Ava's spoken
   reply is **LLM-authored** by the Closer completer (`composeGreetingLead` →
   `greetingReplySystem`) so she genuinely engages: reacts to what they actually
   said (reciprocates *"how about you?"*, notices a busy day), then in ONE tight
   line — single transition, capped ~35 words to avoid a wall of text — frames the
   survey with the **question count and purpose**, and **ends by asking if they're
   ready** (*"...three questions about your candles, to see what's landing —
   sound good?"*). She stops there and waits; she does NOT ask the first question
   yet. A deterministic `fixedFraming` fallback mirrors this (count + product +
   purpose + "Sound good?") when no completer is wired or the model misbehaves.
   Both the prompt and fallback are unit tested (`TestGreetingReplySystem`,
   `TestFixedFraming`, `sanitizeSpoken` via `TestSanitizeSpoken`).

3. **Go-ahead → first question.** The reply to "ready?" is read by `handleStart`:
   a bail ends the call gracefully; anything else is the go-ahead, and the survey
   opens with a brief warm lead + Q1.

Each of beats 2 and 3 is a SINGLE exchange — never loops, so the survey starts
promptly. Silence or a cough during the greeting falls through the normal silence
backstop / non-speech guard. LLM-authored quality tracks model strength; on a
weak local model the fixed framing/fallbacks keep it correct. Toggle the whole
layer with `-greeting` (default on); off restores the LLM-authored intro opening.

```bash
go run ./cmd/server                        # greeting on (default), agent "Ava"
go run ./cmd/server -agent-name=Nova       # rename the agent
go run ./cmd/server -greeting=false        # straight into the intro + first question
```

### Survey purpose (steers questions AND the spoken framing)

The setup page has an optional **"What's this survey for?"** field. The purpose is
threaded through `createPoll → Store.Create → Poll.Purpose` and used in two
places:

- **Question generation** — `GenerateSurvey(product, purpose)` feeds the goal into
  the prompt, which *requires* the questions to focus on it. So "new dishes from
  the northeast region" yields questions about those dishes, not generic
  restaurant filler.
- **Spoken framing** — Ava states the goal (compressed) in the consent beat above.

The setup **presets are scenario-based** (Launching a product, Testing a feature,
New menu, Customer satisfaction, Churn / cancellation, Post-event feedback); each
chip fills both the product and a matching purpose so the generated survey targets
the real use case. Verify by creating a poll with a purpose and inspecting the
stored questions:

```bash
curl -s localhost:8090/api/polls -H 'content-type: application/json' \
  -d '{"product":"a Brazilian restaurant","purpose":"opinions on new northeast-region dishes"}' \
  | python3 -m json.tool   # questions should center on the new dishes
```

### Two-beat pacing (connect turns like a person)

Each forward transition is delivered as TWO beats — a short acknowledgment, a
~400ms pause, then the question — instead of ack+question in one breath, so the
agent connects to the next question the way a person would. Applies to the
answer→next-question advance (`askNextOrFinish`), the opening question after the
consent gate (`startSurvey`), and the greeting reply itself (reaction beat →
framing + "ready?" beat, split by `splitGreetingBeats`). Toggle `-pacing`
(default on). Full design + sources in `docs/PACING-RESEARCH.md`.

Mechanics (the important part): both beats are ONE turn — a single trailing
`tts_end` — so the mic re-arms only after both drain, never mid-turn (the
multi-segment hazard that makes an agent talk over itself). The pause is a
silent-PCM buffer from `speech.Silence` (Kokoro has no SSML), played inside the
same continuous stream; a new `agent_add` control message appends the second
transcript bubble + progress without resetting the turn. An empty ack — or
`-pacing=false` — collapses to a single beat with no pause, so weak-model/no-ack
turns behave exactly as before. `speakTwoBeats`/`splitGreetingBeats` are unit
tested (`TestSplitGreetingBeats`).

Last validated (2026-07-22, browser E2E, `-classify-model claude-sonnet-5`,
candles): every question arrived as its own bubble preceded by a specific ack
("A few evenings a week, noted." → the burn-time question); the greeting reply
split into "Glad it's going well despite the busy morning!" + "So I've got two
questions … Ready to jump in?"; both runs reached `end_reason=completed` with all
slots answered — proving the mic re-armed correctly after every paced turn.

### "Needs help" — when the respondent doesn't know how to answer

Some replies aren't answers, refusals, or off-topic asides — they're the person
asking *us* how to answer: *"Do you expect some score or something?"*, *"what do
you mean?"*, *"what are you looking for?"*. That's the `needs_help` intent. The
agent reassures them and hints HOW to answer (the classifier's `ack` carries a
question-specific reassurance — *"No need for a score — just your honest gut
feeling."*), then **re-poses the same question** without advancing, so they get a
real second shot. It shares the re-ask budget (`maxReasks`) so a confused
respondent can't loop; after that the slot is skipped honestly.

`needs_help` is deliberately NARROW: a vague, rambling, negative, or ESL/broken
on-topic reply is still an `answer` (the dangerous confusion is reading an answer
as needs-help and losing it), and "didn't hear it" is still `repeat`. The eval's
`help` recall column tracks it; on the 3B it under-fires a bit (weaker models
lean toward `answer`, which is the safe direction). `helpPrompt` is unit tested
(`TestHelpPrompt`).

### Non-speech guard (a cough is not an answer)

STT annotates non-speech sounds as bracketed tokens — `(coughing)`, `(laughs)`,
`[inaudible]`, `(background noise)`. Weak models see the word inside and treat it
as an answer (the agent says *"Got it —"* and advances on a cough). A
**deterministic** guard, `llm.IsNonSpeechArtifact`, runs BEFORE the model in both
`ClassifyTurn` backends: if a reply is entirely bracketed annotation with no real
spoken words, it's forced to `unintelligible` (so the agent re-poses, never
advances). Model-independent — it holds even on the local 3B, which the prompt
rule alone did not. Unit tested (`TestIsNonSpeechArtifact`); dataset covers
`(coughing)`, `(clears throat)`, `(laughs)`.

### Acknowledgment layer (making it feel like a conversation, not a form)

Every turn, the classifier also returns a short **`ack`** — a warm, specific
spoken lead-in the agent says right before the next question (folded into the
same classify call, so no extra round-trip). Two jobs:

- **Normal answer** → reflect their point back, then advance:
  *"Warm and calming — love that. Would you consider…"*. It must be SPECIFIC to
  what they said; a canned "Got it, thanks" every turn reads as a bot, so the
  prompt pushes specificity and variety.
- **Off-topic aside** → the ack becomes a warm steer-back and the agent re-poses
  the question (replacing the old robotic *"let me ask again"*):
  *"Ha, no worries — What's your overall impression…"*. It never engages the
  tangent and never promises to discuss it later.

Off-topic handling also changed on the data side: after one steer-back, if the
reply is still off-topic (or noise), the slot is **skipped** (recorded
`Skipped`, no answer) rather than storing the tangent as a bogus answer — results
stay honest. A thin-but-on-topic answer is still kept.

Like clarity, ack strength tracks model strength: the local 3B mostly emits an
empty ack (so the layer stays inert — the plain question is asked, exactly the
pre-ack behavior), while cloud/hosted models produce rich, specific acks. The
eval's `ack` column (LLM-judge, ungated) quantifies this.

### Opening & closing lines (product-aware intro, personalized sign-off)

The greeting and farewell used to be one hardcoded string each. Both are now
authored by an LLM so every poll opens and closes in its own voice.

- **Intro** — generated once at poll creation, in the SAME call that produces the
  questions (`llm.GenerateSurvey` → `SurveyPlan{Questions, Intro}`), and stored on
  the poll (`Poll.Intro`). It's a warm, product-aware opening spoken before the
  first question. Deterministic at runtime (zero added latency, no per-turn risk).
  Missing/oversized intro (`cleanIntro`) → falls back to the fixed greeting via
  `introLine`. Author model is the question-gen model (always local `qwen2.5:3b`).

- **Closing** — a personalized **callback**: at happy-path completion the agent
  asks a one-shot `Completer` (the `Closer`, wired from the *classify* model — the
  conversation's "brain") for a farewell that references ONE genuine thing the
  respondent actually said. The call runs at end-of-conversation, so latency is a
  non-issue. Safety rails: only **answered** slots feed the prompt
  (`closeTranscript`); if nothing was answered, or the model errors, or the reply
  fails `sanitizeClosing` (empty / multi-line / >240 chars), it falls back to the
  fixed close — a personalized-close path never invents a reference and never
  double-acks (it drops the last-turn `lead`). Deterministic helpers are unit
  tested (`TestIntroLine`, `TestSanitizeClosing`, `TestCloseTranscript`).

Validate live (both fire in the headless happy probe; the fixed-fallback case is
covered whenever no answer is captured):

```bash
# Product-aware intro (inspect the stored poll):
curl -s -XPOST localhost:8090/api/polls -H 'content-type: application/json' \
  -d '{"product":"hand-poured scented soy candles for the home"}'   # note the id
python3 -c "import json;print(json.load(open('data/<id>.json'))['intro'])"

# Intro spoken + personalized closing (strong closer). Feed a REAL answer clip so
# slots actually fill (the default probe clip classifies off-topic → fixed close):
./bin/server -classify-model claude-sonnet-5 &      # key read in-process, never printed
go run ./cmd/probe -mode happy -product "hand-poured scented soy candles for the home" -wav <16kHz-answer.wav>
```

Last live check (2026-07-21, classify/closer = `claude-sonnet-5`): intro
*"Hello there! Just a few quick questions about our hand-poured scented soy
candles in your home. How do you like the scent…"*; closing *"It's great to hear
how relaxing you find the scent and that they last so long—thanks so much for
your time today, take care!"* — referenced the captured answer, ended
`completed`. Fixed-fallback confirmed on a qwen run where the probe clip
classified off-topic (nothing answered → generic close, no fabricated reference).

### Insights / per-response scoring (`/insights/<id>`)

A separate results page scores a completed conversation with an **independent**
LLM call (`internal/insight`, via `llm.NewCompleter` — NOT the per-turn
classifier). Given the product + the transcript (question/answer/status per
slot) it returns product **sentiment**, per-answer **usefulness** (1–5) and
**confidence** (1–5), a short **summary**, and an aggregate. Scoring model is
`-insight-model` (default local `gemma4:latest`, offline). Results are cached on
the poll (`?refresh=1` recomputes). Reachable from `/results/<id>` via "View
scored insights".

```bash
go run ./cmd/server                       # then open /insights/<a completed poll id>
go run ./cmd/server -insight-model claude-sonnet-5   # score with a hosted model
```

### Closing-line eval — `go run ./cmd/evalclosing`

A second, deliberately narrow eval for the **personalized sign-off**. Full
rationale in [cmd/evalclosing/EVAL.md](cmd/evalclosing/EVAL.md).

Built to answer one question with a number: the browser QA run caught the agent
opening its farewell with the respondent's own filler (*"Sure thing, thanks for
sharing!"*). **Deterministic only — no LLM judge**, because the defect is exactly
string-detectable; a judge would add cost and noise over a `strings.HasPrefix`.

Fidelity: it drives `ws.ClosingSystem`, `ws.ClosingUserPrompt`,
`ws.CloseTranscript` and `ws.SanitizeClosing`, and builds a real `survey.Survey`
per case, so the transcript is rendered by production code. Those identifiers were
exported from `internal/ws` for exactly this — re-typing the prompt in a harness
is how an eval silently stops measuring what ships.

```bash
go run ./cmd/evalclosing                                              # print only
./scripts/with-langfuse.sh go run ./cmd/evalclosing -run baseline-before-fix
```

**Not in `validate.sh`** (needs a live model; the gate stays fast and offline).
The scorer unit tests are, via `go test ./cmd/evalclosing/`.

**Baseline (2026-07-29, `qwen2.5:3b`, closing prompt `a2844dfca273`):**

| Metric | Result |
|---|---|
| clean opener | **9/13 — 69.2%** |
| clean opener, **tic cases** | **4/8 — 50.0%** |
| within 35 words | 13/13 |
| no question mark | 13/13 |
| mean copied span | 2.2 words |

Langfuse run `qwen2.5:3b@baseline-before-fix` on dataset `closing-line`; verified
via the API that 13 items, 13 run items and the aggregates landed, with
`clean_opener_rate: 0.6923` matching the terminal. **All control cases passed**, so
the defect is specific to tic input rather than a general register problem. The
worst case, `okay-so`, failed both checks at once — opened with the filler *and*
lifted nine consecutive words.

**After the fix — closing prompt `99bfe50db4e1`** (prompt rule +
`ws.StripFillerOpener`, 15 cases):

| Metric | Before | After |
|---|---|---|
| MODEL clean opener (prompt quality) | 69.2% | **86.7%** |
| MODEL clean opener, **tic cases** | **50.0%** | **75.0%** |
| FINAL clean opener (post-guard) | 69.2% | **100%** |
| guard fired | — | 2/15 |

The guard lives in `internal/ws/closing.go` and shares its filler list with the
eval's scorer — one definition, so the measurement and the repair cannot disagree
about what counts as filler. It **repairs rather than rejects** (the rest of the
line is fine) and **fails open** (a guard that can empty the agent's last words is
worse than the tic). Browser QA re-run: poll `94243ba6d5`, `end_reason=completed`,
2/2 slots, screenshot `qa-screenshots/closing-fix-qa-enthusiast.png`.

> ⚠️ **Still open — verbatim parroting.** The same browser QA produced a closing
> that copied **16 consecutive words** from the respondent's last answer. The
> offline corpus had missed the entire class because every case had short answers;
> a long, fluent, quotable final answer is what tempts wholesale repetition. Two
> `quotable-*` cases were added from that live transcript and now reproduce it
> deterministically. **Not fixed** — it needs another prompt iteration, and the
> lavender incident below is the reason not to attempt one blind.

> **Three corrections this harness produced, all worth keeping.**
> 0. **A prompt example became a template.** The first version of the fix ended the
>    new rule with a worked example ("Lavender for the bedroom — lovely."). The 3B
>    model copied it into **all 13 cases**, inventing lavender for a respondent who
>    said only "Vanilla." — while every deterministic score read 100%. Negative
>    examples (the filler phrases not to use) were never copied; the positive one
>    was. Removed it; added `UnsupportedWords` as a tripwire for the class.
> 1. The verbatim-reuse claim was called **refuted** on 6 transcripts — and that
>    verdict was **under-powered, not correct**. None of those 6 had a long quotable
>    answer; once such a case existed, a 16-word copy reproduced immediately. The
>    original *"coastal and tropical blends"* reading was still wrong (it is a real
>    paraphrase, pinned in `TestLongestCopiedSpan`), but "refuted" was too strong a
>    word for what the corpus could support. A negative result is only as strong as
>    the input shapes present.
> 2. The **first** baseline run was discarded: it scored 76.9% because
>    `FillerOpener` missed stacked fillers (`"Okay so,"`). Fixed and pinned by
>    `TestFillerOpenerStacked` before the baseline was recorded — a scorer bug
>    found after the baseline would have invalidated the comparison.

### Langfuse observability + experiment tracking (optional, `internal/obs`)

Two independent exports to [Langfuse](https://langfuse.com), both off unless
credentials are present. Langfuse ships **no Go SDK**, so each half uses its
officially supported non-SDK path:

| Half | Transport | What it covers |
|---|---|---|
| **Traces** | OpenTelemetry → OTLP/HTTP → `/api/public/otel/v1/traces` | every `ClassifyTurn`, in production and in the eval |
| **Datasets / experiments / scores** | hand-written REST client (`internal/obs/langfuse.go`) | the labeled corpus, per-model runs, gated + ungated metrics |

The only third-party dependency added is the **official OpenTelemetry Go SDK**
(`go.opentelemetry.io/otel`); the REST half is raw `net/http`, matching how
`internal/llm` talks to Anthropic.

**Env-driven, off by default.** Enabled only when `LANGFUSE_PUBLIC_KEY` and
`LANGFUSE_SECRET_KEY` are set (`LANGFUSE_HOST` optional, defaults to Langfuse
Cloud). Without them every entry point is a no-op, so the gate and the PoC stay
fully offline. **Keys are never logged** (same rule as the Anthropic key) — the
secret exists only inside the encoded Basic header.

**Never fatal.** A broken/unreachable Langfuse degrades with a log line; it never
stops the server taking calls or the eval from running (and therefore can never
change the gate's verdict).

**But never silent, either.** `obs.Init` logs when tracing is **disabled**, because a
silently untraced run is indistinguishable from a healthy traced one until you're
looking at an empty dashboard — and by then the conversation is gone, since traces
cannot be reconstructed after the fact. This cost real runs before the warning
existed: a poll driven through `:8090` and a whole `npm test` persona suite, both
started without the keys, both invisible.

```
langfuse: tracing DISABLED — LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY not set,
  nothing will be exported (run under ./scripts/with-langfuse.sh to enable)
```

Exactly one key set gets a *different* message naming the empty one and calling it a
misconfiguration — no keys is the normal offline case, one key never is. Both messages
are asserted by `TestInitWarnsWhenKeysAreMissing` and
`TestInitWarnsDifferentlyOnHalfConfigured`, including that the first one names the
wrapper rather than just the symptom.

The wrapper requirement is now documented where it's actually needed:
`docs/BROWSER-QA.md` and `scripts/browser-e2e/playwright/README.md` — neither
previously mentioned that a plain `npm test` exports nothing (its `webServer` block
passes no `env`, so it inherits a shell that sets no `LANGFUSE_*`).

#### Span hierarchy — one TURN is one trace

Every span used to start from `context.Background()`, which carries no active span.
A span with no parent **is** a root span, and a root span is a new trace — so a
finished call produced ~21 separate single-observation traces. A session was a flat
list where the classifier appeared as its own entry between two pieces of spoken
text, dumping `{question, reply}` / `{intent, sufficient, clarity, ack}` tables into
the middle of the dialogue. Measured on a real persona run:
`21 traces, observations per trace {1}`.

The fix is **one span per turn**, in `handleUtterance` — not a single
conversation-level root. A conversation root was tried first and rejected: it
collapses the whole call into one card and loses the per-turn separation that makes a
session skimmable. The unit that reads well is the turn.

Each turn span carries what the respondent said as input and what the agent said back
as output (`streamTTS` is the single funnel every spoken segment passes through, so it
is the one place that can reconstruct the latter).

**Trace names are the conversational PHASE, not an index.** The name is the field
Langfuse aggregates and filters by, so it has to be a small closed set of stable
values: `turn 1`…`turn N` fragments it into one name per position and per-phase
latency/cost can never group. The ordinal lives in `turn_index` metadata instead —
anything that varies per occurrence belongs there (`Operation.Meta`).

The names come from `cv.phaseName()`, which reads the same state-machine flags the
ending logic reasons about, so the trace list and the state machine cannot drift apart:

| name | when |
|---|---|
| `greeting` | the agent's opening hello (scoped, so it is one trace and not three bare `tts`) |
| `greeting_turn` | `inGreeting` — their reply to "how's your day" plus the framing (**not** `greeting_reply`: that name belongs to the LLM generation nested inside, and a trace sharing a name with its own child is unreadable in a filter) |
| `ready_check` | `awaitingStart` — the consent beat before Q1 |
| `survey_turn` | a question/answer exchange |
| `repair` | `awaitingConfirm` — the clarify-once turn |
| `reprompt` | a silence nudge — named so the backstop the ending thesis rests on is countable |

Verified on the real local stack (enthusiast persona, completed):

```
total: 5 | names: {greeting: 1, greeting_turn: 1, ready_check: 1, survey_turn: 2}
  greeting        idx=-   obs=4
  greeting_turn   idx=1   obs=6
  ready_check     idx=2   obs=5
  survey_turn     idx=3   obs=5
  survey_turn     idx=4   obs=6
```

`stt`, `classify_turn`, `greeting_reply`, `closing_line` and the `tts` calls all nest
inside the phase they belong to, so the classifier's JSON is one expand away instead of
interrupting the transcript.

Not yet seen in a live trace: `repair` and `reprompt`. They share the same code path as
the names above, but the enthusiast persona triggers neither (`repair` needs an
`unclear` answer, which the `confused` persona produces — the flaky one). There is also
no `closing` name: the closing line is generated *inside* the last `survey_turn`, so
`closing_line` is a nested observation rather than its own trace.

Span creation funnels through one `startSpan` helper because of a trap worth
recording: `langfuse.trace.name` and `langfuse.session.id` are **trace-level**
attributes, applied to the whole trace no matter which span carries them. Setting the
name on every span was correct when each span was its own trace; once they nest, a
child would rename the entire conversation after whichever leaf wrote last. So only a
span that actually begins a new trace claims the name
(`!trace.SpanContextFromContext(ctx).IsValid()`).

Tradeoff: a turn's span exports only once the turn finishes, so the card for the turn
in flight is not there yet. Far milder than the conversation-root version, which showed
nothing at all until the whole call ended.

**The `confused` persona test is marginal, and this hierarchy is what proved it.**
While validating the change, `confused triggers needs_help, then completes` failed on
the 220 s `waitForSelector` budget. Reading the new trace:

| | |
|---|---|
| server work per turn | **~4–5 s** (stt ~0.3 s, classify ~2 s, tts ~1 s ×2) |
| gap *between* turns | **15–20 s** — the harness: persona LLM → TTS → fake mic → VAD |
| turns before the cap | **9+, still going** |

So ~22 s of wall clock per turn against a 220 s budget leaves room for only ~10
turns, and the confused persona sometimes needs more. The agent was never stuck — it
was answering in ~4 s each time.

Not a regression: re-running the same test on `main` (which lacks this change) also
fails the same way — **main passed 2 of 3 runs**. The suite's `retries: 1` exists for
exactly this ("personas are LLM-driven, so an occasional off-character generation is
possible"), but with only ~10 turns of headroom one retry is not always enough. Worth
fixing separately by raising that test's budget or capping the persona's confusion;
the other three personas pass consistently.

#### What is traced

Instrumentation is applied by **wrapping interfaces at construction** in
`cmd/server/main.go` and `cmd/eval/main.go` — no span is created in a handler or
in `internal/llm`, so the rest of the app is unaware tracing exists.

| Span | Source | Notes |
|---|---|---|
| `turn N` | `obs.StartScope` in `internal/ws` | one per utterance; parents that turn's stt/classify/tts, and carries respondent-said as input, agent-said as output |
| `classify_turn` | `llm.Classifier` (wrapped once) | covers all 3 `ws` call sites + the eval |
| `greeting_reply`, `closing_line` | the Closer `llm.Completer` | one shared wrapper; told apart by `obs.WithLabel` |
| `insight_scoring` | the insight `llm.Completer` | one-shot results pass |
| `stt`, `tts` | `speech.Engine` via `obs.StartOp` | latency + payload bytes; these take no `context`, so they're traced at the call site |
| prompt versions | `obs.WithPrompt` / `prompt.Resolved` | content fingerprint always stamped, plus the native `langfuse.observation.prompt.*` link when the prompt came from Langfuse — so a score movement is attributable to a prompt edit rather than guessed at |
| `qa_persona_reply` | the dev-only `-qa` endpoint | the simulated respondent, not the agent |

Still untraced: question generation (`GenerateSurvey`). Anything driving the
conversation through a different interface than `llm.Classifier` would also need
its own wrapper — the decorators above cover these two interfaces only.

#### Production traces (`cmd/server`)

- `obs.TraceClassifier` is a **pure pass-through decorator** over any
  `llm.Classifier` (Ollama or Anthropic) — it never alters the `Turn` or the
  error, so conversation behavior is identical with tracing on or off.
- Each span carries question/reply as input, the full `Turn` as output,
  intent/clarity/sufficient, the model, and the **classifier prompt version**
  (`llm.ClassifyPromptVersion` — a content hash of the system prompt + few-shots,
  so a score shift can be attributed to a prompt edit).
- Each WebSocket conversation tags its context with a per-run session id
  (`<pollID>-<unix>` → `langfuse.session.id`), so one conversation reads as one
  session.
- The `x-langfuse-ingestion-version: 4` header is set, which is what makes
  Langfuse's **server-side LLM-as-judge evaluators** able to run on these
  OTel-ingested traces (configured in the Langfuse UI, no Go code).

```bash
LANGFUSE_PUBLIC_KEY=pk-lf-… LANGFUSE_SECRET_KEY=sk-lf-… go run ./cmd/server
# logs: "langfuse tracing enabled -> https://cloud.langfuse.com"
```

#### Eval runs as experiments (`cmd/eval`)

With credentials set, an eval run also publishes itself as a Langfuse dataset
experiment, turning terminal output that scrolls away into comparable history:

- the hand-labeled corpus becomes one **dataset** (`-langfuse-dataset`, default
  `turn-classifier`); items upsert by a content-hash id, so re-pushing every run
  creates no duplicates;
- each model's pass becomes one **run** named `<model>@<run>` (`-run`, default
  `<prompt-version>-<unix>`);
- each case contributes a **run item** linking the dataset item to the trace that
  classification produced, plus a per-case `intent_correct` BOOLEAN score;
- the aggregates land as **run-level scores**: `intent_accuracy`,
  `answer_acceptance`, `clarity_accuracy`, and `ack_quality` when judged.

```bash
LANGFUSE_PUBLIC_KEY=… LANGFUSE_SECRET_KEY=… go run ./cmd/eval -run before-prompt-fix
```

Two API constraints the client is built around (verified against the live
OpenAPI spec): datasets upsert by **name** and dataset items by **id**, but
**dataset run items are not idempotent** — the server mints their ids, so they
are posted once and only ever retried on a `404`, which means the trace hasn't
been ingested yet (traces are force-flushed via `obs.Flush` before linking).

**Validated (2026-07-29):**

1. **Unit** — 10 tests in `internal/obs`: the disabled no-op path, that `Init`
   actually **succeeds** with credentials (see the regression note below), trace
   id capture, decorator pass-through incl. error propagation, and the REST
   client driven against an `httptest` fake Langfuse — endpoint paths, required
   field names, Basic auth on every call, score subject targeting (trace vs run),
   `BOOLEAN` value validation, 404-retry, and no-retry on other errors.
2. **End-to-end against a local fake Langfuse** — a full
   `go run ./cmd/eval -models qwen2.5:3b -judge ""` produced exactly:
   1 dataset, 82 dataset items, **82/82 cases linked** to run items, 85 scores
   (82 per-case + 3 aggregates; `ack_quality` correctly skipped with no judge),
   and 4 OTLP batches — auth present on every call.
3. **Production path** — server started with tracing on
   (`langfuse tracing enabled`), `cmd/probe -mode happy` reached
   `reason=completed` unchanged, OTLP batches 4 → 12.
4. **Gate** — `./scripts/validate.sh` **7/7** with credentials unset, and the
   eval reports identical numbers (95.1% / 95.7%) traced and untraced.

> ⚠️ **Regression note worth keeping.** The first cut of `obs.Init` merged a
> pinned `semconv` schema URL into the default OTel resource, which fails with
> *"conflicting Schema URL"* — meaning tracing was broken on **every**
> credentialed start, and `cmd/server` would have called `log.Fatalf` and refused
> to boot. Unit tests missed it because they only exercised the
> credentials-absent path; the fake-Langfuse smoke run caught it immediately. Fix:
> `sdkresource.NewSchemaless`, plus `TestInitEnabledWithKeys` so the credentialed
> path is now covered, plus degrade-not-abort in `cmd/server`.

5. **Against a REAL Langfuse** (self-hosted locally, v3.224.3 — see below):
   - eval run `qwen2.5:3b@first-real-run` → **82 traces** ingested, each tagged
     `eval`, carrying `prompt_version=79bec7b42725`, `expected_intent`,
     `intent_correct`, the question/reply as input and the full `Turn` as output;
   - **85 scores**: 82 per-case `intent_correct` + 3 run-level aggregates whose
     values match the terminal exactly — `intent_accuracy` 0.9512,
     `answer_acceptance` 0.9565, `clarity_accuracy` 0.6739;
   - **idempotency proven**: after three runs pushing the same corpus the dataset
     holds **82 items, not 246**, with 2 distinct runs recorded;
   - **production path**: a `cmd/probe -mode happy` conversation produced 12
     traces all grouped under one session (`8b910e2a93-…`), untagged so they stay
     separable from eval traces, and still ended `reason=completed`.

6. **Browser E2E with tracing on** (2026-07-29, Chrome DevTools MCP, fake mic,
   `enthusiast` persona, candles, poll `dfa9e6d7fb`, real local Langfuse):
   `end_reason=completed`, **3/3 slots answered** with verbatim STT text, full
   greeting flow (hello → reply → framing + "ready?" → go-ahead → Q1-3) and a
   personalized close referencing the respondent's own words ("coastal and
   tropical blends"). Screenshot:
   `qa-screenshots/langfuse-qa-enthusiast-completed.png`.

   The run produced **24 traces in one session** (`dfa9e6d7fb-1785330618`) —
   `tts` ×12, `classify_turn` ×5, `stt` ×5, `greeting_reply` ×1, `closing_line`
   ×1 — proving session grouping and every new span type. The insight pass added
   `insight_scoring`; the 5 `qa_persona_reply` spans are the simulated
   respondent and correctly carry **no** session id (they're not conversation
   turns).

   **First actionable finding from the new spans — TTS dominates the audio path:**

   | operation | n | p50 | max | total |
   |---|---|---|---|---|
   | `tts` | 12 | 0.79s | 2.14s | **11.15s** |
   | `stt` | 5 | 0.33s | 0.37s | 1.61s |
   | `classify_turn` | 5 | 0.90s | 1.21s | — |
   | `insight_scoring` | 1 | 14.59s | — | (post-call, off the hot path) |

   Synthesis cost ~7× transcription across one 3-question call. That is direct
   evidence for the streaming-TTS upgrade already noted in the README's
   next-steps (`GenerateWithCallback`), and it was invisible before these spans.

Not re-run after the final one-line `qa_persona_reply` label change: it renames a
span on the dev-only QA endpoint and touches no conversational logic.

#### The local Langfuse stack

Self-hosted at `~/projects/langfuse-local` (outside this repo, so nothing secret
lands here):

- upstream `docker-compose.yml` kept **byte-identical** to Langfuse's, so it can
  be re-fetched to upgrade; all local changes live in `docker-compose.override.yml`.
- **Port remaps** (both upstream defaults were already taken on this machine):
  web `3000 → 3001` (a node process holds 3000), postgres `5432 → 5434`
  (`myjourney-db-1` holds 5432). The override uses the `!override` YAML tag —
  Compose *merges* sequences by default, so a plain `ports:` list would have
  **added** 3001 while still trying to bind the taken 3000.
- **Headless provisioning**: the org, project, user and API key pair are created
  on first start from `LANGFUSE_INIT_*` in that stack's `.env` — no browser signup
  was needed. `NEXTAUTH_URL` must match the remapped port or login redirects break.
- `SALT`, `ENCRYPTION_KEY` and `NEXTAUTH_SECRET` are `openssl rand -hex 32`
  values, not the compose file's `CHANGEME` defaults. Telemetry is off.

```bash
cd ~/projects/langfuse-local && docker compose up -d   # UI: http://localhost:3001
```

Run either binary against it with the wrapper, which exports the keys and fails
fast if the stack is down (`LANGFUSE_ENV_FILE` points it at another instance,
e.g. Langfuse Cloud):

```bash
./scripts/with-langfuse.sh go run ./cmd/server
./scripts/with-langfuse.sh go run ./cmd/eval -run before-prompt-fix
```

> Not wired into `validate.sh` on purpose: the gate must stay offline and must not
> depend on a running container. Tracing is always opt-in.

### Prompt management — two sources (`internal/prompt`, `-prompts`)

Every LLM instruction is declared once in `internal/prompt` and resolved at use
time, so the active text can come from the binary **or** from Langfuse Prompt
Management. Full design in [docs/PROMPTS.md](docs/PROMPTS.md).

| Mode | Source | Used by |
|---|---|---|
| `-prompts=code` (default) | compiled-in `Def`s | the gate, both evals, all offline work |
| `-prompts=langfuse` | `GET /api/public/v2/prompts/{name}?label=` at boot | prompt experiments |

Five prompts are registered: `classify-turn`, `question-gen`, `closing-line`,
`greeting-reply`, `insight-score`.

**The refactor is behavior-neutral, and that was measured, not assumed.** Moving
five prompts from Go constants into the registry is exactly the kind of change that
silently alters a prompt and invalidates every baseline in this file. Each prompt was
dumped at runtime on this branch and on `main` and compared byte-for-byte:

| Prompt | How it was captured on both trees | Result |
|---|---|---|
| `classify-turn` | `classifyPrompt(q, r)` — system + all 15 few-shots + the turn | identical (99 lines) |
| `question-gen` | `GenerateSurvey` driven against a recording fake at `OLLAMA_HOST`, capturing the real request body | identical (15 lines) |
| `closing-line` | `ClosingSystem`/`ClosingUserPrompt` vs. `ResolveClosing` | identical (14 lines) |
| `greeting-reply` | `greetingReplySystem(...)`, both the populated and all-blank cases | identical |
| `insight-score` | `system` + `buildPrompt(in)` vs. `ScorePrompt` + `promptVars(in)` | identical (29 lines) |

Corroborated by the gate: the intent eval reports **95.1% / 95.7%** — the same
numbers as the pre-refactor baseline recorded above.

### Few-shot test-set leak guard (`cmd/eval/leak.go`)

The classifier prompt's few-shot anchors and the eval corpus must stay disjoint. An
anchor repeating a corpus sentence hands the model the answer to a case it is about
to be graded on: accuracy rises, the classifier does not improve, and the number
quietly stops meaning anything.

`EVAL.md` had carried this rule since it bit us once, but enforcement was a human
reading a diff. Prompt management removed that backstop — an anchor authored in the
Langfuse UI reaches production without a code review — so the rule is now executable:

- `TestNoLeakInCompiledPrompt` runs under `go test ./...`, so the **gate** catches a
  leak introduced in Go, offline, with no credentials.
- `cmd/eval` re-runs the same check against the **active** prompt after resolving
  `-prompts`, and exits before the first model call. That is the half that covers a
  Langfuse-authored anchor.
- Matching is on normalized replies (lowercased, punctuation stripped, apostrophes
  removed) so casing and punctuation can't disguise a reused sentence. Exact matches
  are reported at any length; containment needs ≥20 normalized characters, because
  short replies ("Yeah.", "Vanilla.") legitimately recur across a corpus about
  opinions and flagging them would train people to ignore the check.

**It found 11 pre-existing leaks on its first run** — 11 of the 15 anchors repeated a
corpus sentence verbatim, so `EVAL.md`'s claim that the anchors were "intentionally
novel sentences" was not true. The cause is benign and will recur: fixing the cough
bug meant adding `(coughing)` to the prompt *to teach the model* and to the corpus
*to lock the fix*.

Measured impact, before the fix:

| Group | Accuracy |
|---|---|
| the 11 leaked cases | **11/11 — 100%** |
| the other 71 cases | **67/71 — 94.4%** |
| all 82 (what was reported) | **78/82 — 95.1%** |

Fixed by rewriting the 11 **anchors** as novel sentences teaching the same lesson
(prompt now says `(sneezes)`, corpus keeps `(coughing)`), leaving the corpus untouched
because those cases are regression anchors for real bugs.

**After the fix the gate reports `acc 95.1%, answer 97.8%`** — accuracy held and the
answer rate rose from 95.7%, which is the reassuring outcome: the anchors were
teaching the intent classes, not memorizing the corpus. Removing the overlap cost
nothing and the number is now honest.

**What the tests cover** (`go test ./...`, no credentials needed):

- `internal/prompt` — `{{var}}` substitution incl. `{{ spaced }}`, unknown
  placeholders left visible rather than blanked, fingerprint stability/length/
  sensitivity, role splitting (`System`/`LastUser`/`Chat`), `Install` replacing and
  `Reset` restoring, and the rejection paths.
- `internal/obs` — a 404 treated as "not yet pushed" rather than an error, text-type
  prompts refused, `EnsurePrompt` skipping identical content, ignoring whitespace-only
  drift, and creating on real drift with the right label + commit message.
- `cmd/prompts` — the only test binary importing all five prompt packages, so it
  holds the whole-registry checks: every declared var is used, no undeclared
  placeholder exists, names are namespaced, descriptions are non-empty, and the
  registry count matches the `-expect` default.
- `cmd/prompts` round-trip against an in-process fake Langfuse: push creates v1 →
  **second push creates nothing** (content-idempotent) → load installs v1 and the
  fingerprint is unchanged → a simulated UI edit to v2 is picked up by
  `ws.ResolveClosing` → an edit that drops `{{transcript}}` is **rejected** and
  leaves the compiled-in default active.

**Fail-fast, unlike tracing.** A trace export failure degrades with a log line; a
prompt load failure is **fatal**. Verified by hand:

```
$ go run ./cmd/server -prompts=s3
-prompts: unknown prompt mode "s3" (want "code" or "langfuse")
$ go run ./cmd/server -prompts=langfuse          # no credentials
-prompts=langfuse needs LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY
$ go run ./cmd/prompts list                      # no credentials
LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY are not set — nothing to talk to
```

Losing a trace costs a data point; loading the wrong prompt invalidates the run.

**Verified against the real local stack** (`~/projects/langfuse-local`, 2026-07-30):

| Step | Result |
|---|---|
| `prompts list` before any push | all five `not in langfuse` |
| `prompts push` | five `NEW VERSION` at v1, label `production` |
| `prompts push` again | five `unchanged` — content-idempotent against the real API |
| `prompts list` | all five `v1` / `in sync` |
| `server -prompts=langfuse` | boots, logs all five installed from label `production` |
| `evalclosing -prompts=langfuse` | 15/15 cases linked; run named `qwen2.5:3b@800a15685b33` |

The eval's run name carries the **same fingerprint as the compiled-in prompt**
(`800a15685b33`), which is the round-trip proof: pushing and re-fetching returned
byte-identical text. And it reproduced the documented post-fix numbers exactly —
MODEL clean opener **86.7%**, tic cases **75.0%**, FINAL **100%** — while running the
prompt served by Langfuse rather than the one compiled in.

`-prompts` is accepted by **all three** binaries (`cmd/server`, `cmd/eval`,
`cmd/evalclosing`) via one shared `obs.SetupPrompts`. That sharing is deliberate: an
eval that silently scored the compiled-in prompt while the server ran the Langfuse
one would produce numbers for a prompt nobody is using.

Still unexercised: an actual **edit made in the Langfuse UI** being picked up. The
mechanism is covered by the fake-Langfuse round-trip above (a simulated v2 edit is
resolved, and one that drops `{{transcript}}` is rejected), but no human-authored
edit has been round-tripped yet.

### Phase-0 backbone spike (run if speech/models change)

```bash
go run ./cmd/spike
```
Expected: Kokoro loads (~0.3s, 24kHz), Whisper loads (~0.25s), the test clip and
the TTS→STT round-trip both transcribe correctly.

---

## 2. Browser E2E — real Chrome + fake microphone

Validates the part the gate can't: in-browser mic capture, **Silero VAD**
endpointing, TTS playback, and the turn loop. It works by overriding
`getUserMedia` with a synthesized-speech stream (so no human and no mic prompt)
and auto-answering whenever the agent starts listening. **Ground truth is the
server's `data/<id>.json` `end_reason`.**

> ⚠️ **Always QA with the `scented candles` product.** The fake-mic answer clips
> (`ans0/1/2.wav`) are candle-themed ("the scent is relaxing…"), so a
> restaurant/coffee poll would get mismatched answers. Create the QA poll with
> product `hand-poured scented soy candles for the home`.

Harness files (committed, reusable):
- `scripts/browser-e2e/fakemic.js` — inject as an **init script** (before page load)
- `scripts/browser-e2e/autoanswer.js` — inject **after clicking Start**

### Persona-driven QA (simulated respondents, on demand)

Beyond the fixed clips, the browser suite can drive **LLM-simulated personas**
whose answers are generated on demand and synthesized in a distinct voice, then
played into the fake mic — so each run exercises fresh language through the real
VAD → STT → classifier path. Run the server with `-qa` to mount the dev-only
endpoint `POST /api/qa/reply`; inject `scripts/browser-e2e/persona-answerer.js`
with `window.__persona` set. Full walkthrough (fake mic, the round-trip, the
endpoint, how to run) in **`docs/BROWSER-QA.md`**.

Built-in personas and what each asserts (`internal/qa/personas.go`):

| Persona | Asserts | Last run (2026-07-22, sonnet classify, candles) |
|---------|---------|--------------------------------------------------|
| Enthusiast | `completed`, all slots | ✅ `completed` 5/5, personalized close |
| Neutral | `completed`, vague accepted, no re-ask loop | ✅ `completed` 2/2 |
| Rusher | `bailed` mid-survey | ✅ `bailed` (answered Q1, bailed Q2) |
| Confused | `needs_help` fires, then completes | ✅ `needs_help` → re-pose → `completed` 2/2 |

`internal/qa` is unit tested (`TestFind`, `TestReplyUser`, `TestPersonaIDs`).

### Persona QA as a one-command suite (Playwright)

The persona QA above is agent-driven (I run it over Chrome DevTools MCP, one
persona at a time) — good for exploratory checks. The same harness also runs as a
**headless, assertion-backed regression suite** in `scripts/browser-e2e/playwright/`:

```bash
cd scripts/browser-e2e/playwright
npm install && npm run setup   # once
npm test                       # builds server (-qa on :8091), runs all 4 personas, asserts
```

It injects the identical `persona-answerer.js` and asserts on **outcomes**:
`end_reason` + slot statuses (from `GET /api/polls/<id>`) and the **real
classifier intents**. In `-qa` mode the server mirrors each per-turn decision to
the client as a `{"type":"qa_intent"}` frame (collected in `window.__qaIntents`),
so a test can assert `wants_stop` fired for the rusher and `needs_help` for the
confused persona — not just scrape the transcript. That channel is dev-only (gated
on `-qa`, `Handler.QA`) and inert in production. Runner docs:
`scripts/browser-e2e/playwright/README.md`; technique: `docs/BROWSER-QA.md`.

Last suite run (2026-07-22, `-qa` on :8091, `claude-sonnet-5`, candles): **4/4
passed** (8.9m). Enthusiast `completed` all slots; neutral `completed`, no
`needs_help` misfire; rusher `bailed` with `wants_stop` fired; confused
`needs_help` fired → re-pose → `completed`, all slots terminal.

### Prep (once)

```bash
go run ./cmd/genclips          # writes web/static/demo/{ans0,ans1,ans2,bail,repeat,calque,yes,offtopic}.wav
go run ./cmd/server            # http://localhost:8090
# To exercise the repair turn, run the classifier on a stronger model:
go run ./cmd/server -classify-model claude-sonnet-5
```

### Procedure (Chrome DevTools MCP, or any CDP driver / manual console)

1. Create a fresh poll (fresh survey state each run):
   ```js
   await fetch('/api/polls',{method:'POST',headers:{'content-type':'application/json'},
     body:JSON.stringify({product:'hand-poured scented soy candles for the home'})}).then(r=>r.json())
   ```
2. Navigate to `/poll/<id>` with `fakemic.js` as the **initScript**.
3. Click **Start & allow microphone** (fake `getUserMedia` → no prompt).
4. Inject `autoanswer.js`.
5. Wait for the end. Ground-truth check:
   ```bash
   grep end_reason data/<id>.json      # expect "completed"
   ```
   In-page trace is in `window.__log`.

### Cases to cover

| Case | How | Expected `end_reason` |
|------|-----|-----------------------|
| **Happy path** | default `autoanswer.js` (answers every turn) | `completed` |
| **Silence** | click Start, inject nothing, stay quiet | `silence` (one reprompt first) |
| **Bail-out** | set `window.__answers = ['bail.wav']` before first listen | `bailed` |
| **Repair (unclear)** | run server with `-classify-model claude-sonnet-5`; set `window.__answers = ['calque.wav','yes.wav','ans1.wav','ans2.wav']` | one confirm turn ("you said …, did I understand?"), then `completed`; **Q1 stores the calque, not "yes"** |
| **Ack + off-topic** | any strong classify model; set `window.__answers = ['offtopic.wav','ans0.wav','ans1.wav','ans2.wav']` | off-topic → warm ack-redirect (*"Ha, no worries — <question>"*), then each answer gets a specific ack lead-in before the next question; `completed`. On the local 3B acks are mostly absent (question asked plain) — that's expected. |
| **Barge-in** | tick "Enable barge-in", play a clip during agent playback | playback stops; turn continues (headphones IRL) |

> The repair turn only fires when the classifier flags an answer `unclear` — the
> local 3B rarely does, so use `-classify-model claude-sonnet-5` (or a cloud
> model) to see it.

### Last validated

- **Happy path** — poll `4cebed5b6a`, 5 questions auto-answered, `end_reason=completed`.
- **Silence** — poll `f080ec5d06`, no answer, one reprompt, `end_reason=silence`.
- **Repair — keep-original branch** — poll `56a1299bd6`, classifier
  `claude-sonnet-5`, candles. Calque answer ("very perfumed and I like too much…
  price is a little salty") → agent confirmed verbatim → "yes exactly"
  (affirmation) → advanced. `end_reason=completed`, Q1 stored the calque (not the
  "yes").
- **Repair — record-correction branch** — poll `b4e20dfde7`, classifier
  `gemma4:31b-cloud`, candles. Same calque → repair fired → confirm reply was a
  substantive (non-affirmation) answer → server recorded it as the correction,
  Q1 stored the new text. `end_reason=completed`.
  (both observed live in Chrome 2026-07-21; gemma flagged the calque `unclear`
  just as its 91.3% eval clarity predicts)
- **Ack layer + off-topic redirect — per classify model** (2026-07-21, candles,
  `window.__answers=['offtopic.wav','ans0.wav','ans1.wav','ans2.wav']`, all
  `end_reason=completed`):
  - `qwen2.5:3b` (poll `8be01b3aa2`) — off-topic → *"No worries — <Q1>"*;
    happy-path questions asked **plain** (no ack) — 3B under-produces acks, as
    its 15.4% eval ack score predicts. Layer stays safely inert locally.
  - `glm-5.2:cloud` (poll `b749ea40b4`) — *"Ha, no worries —"* redirect, then
    *"Sounds like a nice evening routine."*, *"Warm and calming — love that."*,
    ack even on the closing line.
  - `gemma4:31b-cloud` (poll `d1e84833e8`) — *"No problem —"* redirect, then
    *"Glad you find them relaxing."*, *"A nice evening reading ritual."*,
    *"Warm and calming scents, got it."*
  - `claude-sonnet-5` (poll `8fc80b827a`) — *"Ha, no worries —"* redirect, then a
    specific ack every turn with varied phrasing (*"…got it."*, *"…love it."*,
    *"…noted."*).
- **Human opening (hello → reply → consent) + purpose-driven questions**
  (2026-07-22, browser QA, `-classify-model claude-sonnet-5
  -insight-model gemma4:latest`): opener is a time-aware hello only; the reply
  reciprocates and frames the survey with count + purpose in one tight line
  ending *"…sound good?"*, then waits; the go-ahead opens Q1. A "Churn /
  cancellation" preset produced cancellation-focused questions (purpose steered
  generation, confirmed against the stored poll). Iterated live to remove a
  wall-of-text run-on and an abrupt double transition. Unit suite green
  (`go test ./...`); greeting/framing helpers covered by `TestGreetingLine`,
  `TestTimeOfDay`, `TestGreetingReplySystem`, `TestFixedFraming`,
  `TestSanitizeSpoken`.
- **Opening intro + personalized closing — per classify/closer model**
  (2026-07-21, candles, happy path `['ans0','ans1','ans2']`, all
  `end_reason=completed`). Intro is authored by the question-gen model (always
  `qwen2.5:3b`), so it's the same style across runs; the personalized closing is
  authored by the classify/closer model, so it varies:
  - `qwen2.5:3b` (poll `3da2b71595`) — intro *"Hello and thanks for taking a
    moment to share your thoughts on our hand-poured scented soy candles…"*;
    closing *"I hear you love lavender and vanilla the most, perfect for a
    relaxing evening. Thanks so much for your feedback!"* — even the 3B produced a
    real callback (the closing prompt is simpler than the ack).
  - `glm-5.2:cloud` (poll `cda6fafc19`) — closing *"It's great that warm, calming
    scents like lavender and vanilla make your living room feel so cozy — thanks
    so much for sharing your thoughts, and have a wonderful day!"*
  - `gemma4:31b-cloud` (poll `c1c7c35fab`) — intro included the honesty
    reassurance; closing *"It's great that you're looking for those warm and
    calming scents for your living room. Thanks for your time, and have a
    wonderful day!"*
  - `claude-sonnet-5` (poll `7de10d8a67`) — closing *"I really love that lavender
    and vanilla combo you mentioned for a warm, calming living room feel. Thanks
    so much for sharing your thoughts, and take care!"*
  - Fixed-fallback confirmed separately (headless probe, qwen, non-candle clip →
    nothing answered → generic close, no fabricated reference).
- **Insights page** — `/insights/<completed poll>` scored by `qwen2.5:3b`
  offline: positive sentiment, discriminating per-answer usefulness/confidence
  (off-question repeats correctly dropped to 1–2); cached re-fetch ~2ms; a
  0-answer silence poll scored all usefulness 1 / negative. Rendered in Chrome.

> Note: the greeting is a long clip and the silence window is 9s. When answering
> manually, answer promptly or the silence backstop may fire first — the
> auto-answerer handles this automatically.

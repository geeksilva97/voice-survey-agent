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

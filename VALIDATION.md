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

The turn classifier (`answer` / `wants_stop` / `repeat` / `off_topic` /
`unintelligible`) decides whether the agent advances, re-reads, follows up, or
ends — so a misclassification is what makes a conversation feel wrong (e.g.
re-asking an already-answered question). `cmd/eval` scores it against a broad
hand-labeled corpus (`cmd/eval/dataset.go`, ~73 cases across candles/coffee/
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

- **Overall intent accuracy** (`acc`) — all five intents. **Gate: ≥90%.**
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

### Insights / per-response scoring (`/insights/<id>`)

A separate results page scores a completed conversation with an **independent**
LLM call (`internal/insight`, via `llm.NewCompleter` — NOT the per-turn
classifier). Given the product + the transcript (question/answer/status per
slot) it returns product **sentiment**, per-answer **usefulness** (1–5) and
**confidence** (1–5), a short **summary**, and an aggregate. Scoring model is
`-insight-model` (default local `qwen2.5:3b`, offline). Results are cached on the
poll (`?refresh=1` recomputes). Reachable from `/results/<id>` via "View scored
insights".

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
- **Insights page** — `/insights/<completed poll>` scored by `qwen2.5:3b`
  offline: positive sentiment, discriminating per-answer usefulness/confidence
  (off-question repeats correctly dropped to 1–2); cached re-fetch ~2ms; a
  0-answer silence poll scored all usefulness 1 / negative. Rendered in Chrome.

> Note: the greeting is a long clip and the silence window is 9s. When answering
> manually, answer promptly or the silence backstop may fire first — the
> auto-answerer handles this automatically.

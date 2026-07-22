# Browser end-to-end QA: fake microphone + simulated personas

The unit tests and the classifier eval (`cmd/eval`) cover logic in isolation. But
the thing that actually breaks a voice agent lives in the *seams* — VAD
endpointing, STT transcription, the turn state machine, TTS playback, and the
timing between them. To exercise all of that without a human in a booth, we drive
the real page in **Chrome** with a **fake microphone** and a **simulated
respondent**.

This doc explains how that works: the fake mic, the full audio round-trip, and
the LLM-driven personas.

---

## Why a fake mic (and not a mocked pipeline)

We do **not** stub VAD/STT/TTS. The whole point is to run the exact code paths a
real caller would hit. So instead of faking the *components*, we fake only the
*input device*: the browser's microphone. Everything downstream — endpointing,
transcription, classification, pacing, turn-taking, the ending logic — runs for
real. That's why a run reaching `end_reason=completed` is meaningful evidence:
the mic re-armed, STT produced usable text, the classifier advanced, and the
state machine ended deterministically.

The only thing simulated is *a person talking into the mic*.

---

## The fake microphone

Browsers hand out mic audio through `navigator.mediaDevices.getUserMedia`. We
override it (in an **init script** that runs *before* the page's own scripts) to
return a synthesized stream instead of a real device:

```js
const ac = new AudioContext();
const dest = ac.createMediaStreamDestination();   // a MediaStream we control
navigator.mediaDevices.getUserMedia = async (c) =>
  (c && c.audio) ? dest.stream : orig(c);         // hand the page our stream
```

To make the "respondent" speak, we decode a WAV into an `AudioBuffer` and play it
into that destination:

```js
const src = ac.createBufferSource();
src.buffer = await ac.decodeAudioData(wavBytes);
src.connect(dest);   // -> flows out through the fake mic
src.start();
```

The page's Silero VAD hears it exactly as if a person spoke — no permission
prompt, no real audio hardware, fully deterministic timing. Injected via Chrome
DevTools' `navigate_page(initScript=...)`.

---

## The full round-trip

For each simulated answer, the audio makes a complete loop through the real
system:

```
persona WAV  ─▶ fake mic (MediaStreamDestination)
             ─▶ browser Silero VAD (@ricky0123/vad-web)   ← real endpointing
             ─▶ onSpeechEnd → PCM16 over the WebSocket
             ─▶ Go: Whisper STT (sherpa-onnx)             ← real transcription
             ─▶ turn classifier (intent/clarity/ack)       ← real classification
             ─▶ survey state machine advances / bails / ends
             ─▶ agent line synthesized live (Kokoro TTS)   ← real synthesis
             ─▶ streamed WAV frames ─▶ browser Web Audio playback
             ─▶ orb returns to "listening" → next answer
```

A consequence worth knowing: because the persona's answer is *played as audio* and
then *re-transcribed by Whisper*, the "You:" line in the transcript is the STT
output, not the original text. Small transcription artifacts (e.g. "I'd
definitely" → "I definitely") are expected and are themselves part of what's
being tested.

---

## Two ways to produce the respondent's audio

### 1. Fixed clips (`cmd/genclips`)

`go run ./cmd/genclips` synthesizes a handful of canned answer clips
(`web/static/demo/*.wav`) once, using a different Kokoro voice than the agent so
it sounds like another person. `scripts/browser-e2e/fakemic.js` loads them and
`window.__playAnswer('ans0.wav')` plays one. Deterministic and offline; good for a
quick scripted happy-path/bail/repair pass. Limited to the lines you baked in.

### 2. LLM-driven personas (on demand) — preferred

Each **persona** is a *temperament* (a system prompt) plus a distinct voice. At
test time the answers are generated **on demand**: the harness sends the agent's
current question to a dev-only endpoint, which runs the persona prompt through the
same completer the agent uses, synthesizes the reply in the persona's voice, and
returns the WAV. This produces realistic, varied, adaptive answers every run and
exercises language the fixed clips never would.

Because the answers aren't identical run-to-run, **assertions are on outcomes,
not exact strings**: `end_reason`, how many slots were answered, and which
intents fired.

#### The personas (`internal/qa/personas.go`)

| Persona | Temperament | Expected outcome |
|---------|-------------|------------------|
| **Enthusiast** | Loves it; warm, specific answers | `completed`, all slots answered |
| **Neutral** | Lukewarm, short, vague-but-valid | `completed`, no re-ask loop, no `needs_help` misfire |
| **Rusher** | Answers once, then bails | `bailed` |
| **Confused** | Doesn't know how to answer abstract Qs | `needs_help` fires (reassure + re-pose), then completes |

#### The endpoint (`POST /api/qa/reply`, dev-only)

Mounted **only** when the server runs with `-qa` — never in production. Request:

```json
{ "persona": "enthusiast", "question": "How do you like the scent?", "answered": 1 }
```

It looks up the persona, builds the prompt (`qa.ReplyUser` — `answered` lets
progress-dependent personas like the rusher decide when to bail), calls the
completer, synthesizes with the persona's voice (`speech.SynthesizeVoice`, which
never mutates the agent's own voice), and returns `audio/wav` with the generated
text echoed in the `X-QA-Text` header for logging. The API key stays server-side;
the browser never touches it.

#### The harness (`scripts/browser-e2e/persona-answerer.js`)

Injected as an init script with `window.__persona` set. It overrides
`getUserMedia`, and on **each listening turn** it reads the agent's current line
from `#caption`, derives `answered` from the on-screen progress (`k / n` →
`k-1`), fetches that persona's reply from `/api/qa/reply`, and plays it into the
fake mic. It answers once per *listening turn* (armed on entering `listening`,
disarmed on leaving), so a re-posed question (repair / needs-help) each gets a
fresh reply. A running log is exposed at `window.__qa.turns`.

---

## Running it (Chrome DevTools MCP)

1. Start the server with the endpoint on:
   ```bash
   ./bin/server -classify-model claude-sonnet-5 -qa
   ```
2. Create a fresh poll (`POST /api/polls`), grab its id.
3. `navigate_page` to `/poll/<id>` with `initScript` = the persona-answerer body,
   prefixed with `window.__persona = '<persona>';`.
4. Click the Start button (the AudioContext resume needs that user gesture).
5. Poll the page until `#ended` is visible; read the transcript (`.t-row`) and
   `window.__qa.turns`.
6. Confirm ground truth from `data/<id>.json` (`end_reason`, per-slot `status`).

One run at a time, one persona each — repeat for the set. This is **agent-driven**:
good for interactive, exploratory QA where a human watches and reacts. For the
**repeatable, one-command** version, see the Playwright suite below.

Timing note: an LLM generation (~2-3s) plus TTS plus playback is well under the
server's 12s silence backstop, so the deliberate think-time never trips a
reprompt. Enthusiast runs are the slowest simply because the answers are long to
synthesize and play.

---

## Running it (Playwright — headless, one command, CI)

The Chrome MCP flow is great for watching a run and reacting, but it's manual and
one persona at a time. The **Playwright suite** (`scripts/browser-e2e/playwright/`)
is the same thing made repeatable: it builds the server, launches it with `-qa` on
a dedicated port (default **8091**, so it never touches a dev server on :8090),
runs all four personas end-to-end, asserts outcomes, and tears the server down.

```bash
cd scripts/browser-e2e/playwright
npm install && npm run setup   # once: deps + the Chromium build Playwright drives
npm test                       # all four personas, headless, with assertions
```

It reuses the **exact same harness** — `persona-answerer.js` is injected verbatim
via `page.addInitScript`, with `window.__persona` set first. Nothing about the
pipeline changes; only the *driver* differs (a headless test runner instead of me
over MCP).

**How Playwright handles the mic.** Two levels, both used:

- **Permission** — granted up front with `permissions: ['microphone']` on the
  context, so there's no dialog. (Chromium is also launched with
  `--use-fake-ui-for-media-stream --use-fake-device-for-media-stream` and
  `--autoplay-policy=no-user-gesture-required` so `getUserMedia` is allowed and
  Web Audio isn't autoplay-blocked in headless.)
- **The audio itself** — still the `getUserMedia` override from
  `persona-answerer.js`. Chromium *can* also feed a fixed WAV file as the fake mic
  (`--use-file-for-fake-audio-capture=file.wav`), but that file is fixed at launch,
  which is no good for our on-demand, per-turn persona answers — so we keep the
  override, which plays each freshly-synthesized answer into a
  `MediaStreamDestination`.

**Assertions are on outcomes**, from two sources:

- **Ground truth** — `end_reason` and per-slot `status`, read back from
  `GET /api/polls/<id>` (authoritative; the state machine, not a string, decides).
- **Real classifier intents** — in `-qa` mode the server mirrors each per-turn
  classifier decision to the client as a `{"type":"qa_intent", ...}` frame
  (collected in `window.__qaIntents`). This lets a test assert that `wants_stop`
  actually fired for the rusher, or `needs_help` for the confused persona, rather
  than eyeballing the transcript. The channel is **dev-only** — it's emitted only
  when the server runs with `-qa` and is inert in production (the production client
  simply records it and moves on).

| Persona | Asserted outcome |
|---------|------------------|
| **enthusiast** | `completed`; every slot `answered` |
| **neutral** | `completed`; every slot `answered`; `needs_help` never fires |
| **rusher** | `bailed`; `wants_stop` fired; ≥1 answered, ≥1 left unanswered |
| **confused** | `completed`; `needs_help` fired ≥1×; every slot terminal |

The persona definitions (`internal/qa/personas.go`) are the source of truth for
expected behavior; the suite's `personas.spec.js` mirrors them. On failure,
`npm run report` opens an HTML report where each test attaches an `outcome.json`
(end_reason, slots, intents, spoken turns, transcript) plus a trace. Full runner
docs: `scripts/browser-e2e/playwright/README.md`.

Model note: the suite defaults its classifier to `claude-sonnet-5` (the
rusher/confused outcomes depend on accurate intent detection); set
`QA_CLASSIFY_MODEL=qwen2.5:3b` to run fully offline at lower fidelity. The
Anthropic key is loaded server-side and never appears in the suite.

---

## What this covers — and what it doesn't

**Covered (faithfully):** endpointing decisions, STT on synthesized speech, the
full intent/clarity/ack classification, pacing (ack → pause → question), the
greeting/consent flow, bail detection, `needs_help`, vague-answer acceptance, and
deterministic ending — end to end, over the real WebSocket.

**Not covered:** acoustic realism. The input is clean synthesized speech, so this
says nothing about background noise, accents, cross-talk, real barge-in timing, or
mic quality. A real-voice pass is still the thing that catches VAD/STT robustness
issues.

---

## Last run (2026-07-22, `-classify-model claude-sonnet-5 -qa`, candles)

All four personas, one after another, each on a fresh poll:

- **Enthusiast** → `completed`, 5/5. Warm specific answers; personalized close.
- **Neutral** → `completed`, 2/2. Vague answers ("it's fine, I guess") accepted;
  no re-ask loop, no `needs_help` misfire.
- **Rusher** → `bailed`. Answered Q1, then "Sorry, I've got to go" on Q2 → graceful
  bail close. Agent adapted the framing to the rushed tone ("I'll keep us moving").
- **Confused** → `completed`, 2/2. `needs_help` fired on the abstract question
  ("do you mean a category?") → agent reassured + re-posed (capped) → captured the
  eventual answer.

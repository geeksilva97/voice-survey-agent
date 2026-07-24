# CLAUDE.md — voice-survey PoC

A browser **voice survey agent**: a customer describes a product + purpose → a
local LLM generates 3–5 questions → a respondent opens a link and answers by
**talking** to a local STT+TTS agent that walks the script as a natural
conversation and **ends deterministically**. Everything is self-hosted and
English-only — no paid voice cloud.

Go module: `voicesurvey`. Single binary. macOS, CGO (sherpa-onnx).

---

## The core thesis (read this first)

The agent **cannot "feel" when it's done**. Ending is not a detection problem —
it's a **state machine** the code owns, not the LLM. Questions are explicit
**slots** (`unasked → asked → answered/skipped`); the machine ends when the slots
are done, and the LLM only classifies each turn on the way there.

One happy path, three early exits: **completed** (all slots filled), **bailed**
(user wants to stop), **silence** (timeout backstop). See `docs/RESEARCH.md` for
the full reasoning and `docs/ARCHITECTURE.md §4` for the implementation.

---

## Hard constraints (non-negotiable)

- 🔑 **`ANTHROPIC_API_KEY` must NEVER be printed, logged, echoed, or committed.**
  It is read **at runtime inside the Go process** via
  `llm.LoadAnthropicKey(llm.DefaultAnthropicEnvFile())` (from `~/projects/pepita/.env`).
  Never surface the value in output, code, or commits.
- 🧪 **QA always uses the product `hand-poured scented soy candles for the home`.**
  The demo answer clips and personas are candle-themed; another product mismatches.
- 📝 **Document every change in `VALIDATION.md` and re-validate.** Run
  `./scripts/validate.sh` (build + vet + unit + classifier + headless WS loop).
- 🌐 **After any conversational/voice/client change, run the browser E2E QA**
  (fake mic + personas) and capture screenshots. Screenshot paths must live under
  `/Users/edy/projects` (the Chrome DevTools MCP root).
- 🔌 **Dev server runs on `:8090`.** (The Playwright suite uses its own `:8091`.)
- 🚫 **Gitignored:** `data/`, `models/`, `bin/`, `*.wav`. Never commit these.
- 📦 **Commit/push only when explicitly asked.** Work on a branch, PR to `main`.
  Commit messages end with a `Claude-Session:` trailer; PR bodies end with the
  session link.

---

## Run / build / validate

```bash
go run ./cmd/server                     # dev server on http://localhost:8090
./scripts/validate.sh                   # the full non-browser gate (run after every change)
go build ./... && go vet ./... && go test ./...

# Anthropic-backed classify/insight (key loaded server-side from pepita .env):
go run ./cmd/server -classify-model claude-sonnet-5
```

Prereqs: `ollama serve` + `ollama pull qwen2.5:3b`; `./scripts/fetch-models.sh`
(Kokoro + Whisper into `models/`).

### Server flags (defaults)

| Flag | Default | Notes |
|------|---------|-------|
| `-addr` | `:8090` | listen address |
| `-model` | `qwen2.5:3b` | question-gen (always local Ollama) |
| `-classify-model` | = `-model` | per-turn classifier; Ollama or `claude-sonnet-5` |
| `-insight-model` | `gemma4:latest` | results scoring pass |
| `-greeting` | `true` | time-aware hello + consent beat before Q1 |
| `-agent-name` | `Ava` | spoken agent name |
| `-pacing` | `true` | two-beat delivery: ack → pause → question |
| `-qa` | `false` | mount DEV-ONLY persona endpoint + `qa_intent` channel |

---

## Layout

```
cmd/
  server/   the PoC web server (setup + voice + results/insights pages)
  probe/    drives the whole conversation over WebSocket, no browser/mic (used by validate.sh)
  eval/     scores the turn classifier against a labeled dataset (see cmd/eval/EVAL.md)
  genclips/ synthesizes fixed answer WAVs for the browser demo (one-off)
  spike/    Phase-0 proof that Kokoro+Whisper load from Go
internal/
  ws/       WebSocket handler + protocol + the conversation/turn loop (the heart)
  survey/   slot model + state machine + ending logic (owns the ending)
  llm/      Ollama + Anthropic: question-gen, turn classifier, completer
  speech/   sherpa-onnx wrappers: Whisper STT + Kokoro TTS (+ Silence, SynthesizeVoice)
  session/  poll store (in-mem + JSON in data/<id>.json)
  insight/  one-shot results scoring pass
  qa/       simulated respondent personas for browser E2E (dev/test only)
web/        index/poll/results/insights pages + static/js/client.js
scripts/
  validate.sh          the gate
  fetch-models.sh      model download
  browser-e2e/         fake-mic harness (persona-answerer.js, fakemic.js, autoanswer.js)
  browser-e2e/playwright/  headless one-command persona suite
```

---

## Testing & QA (three layers)

1. **`./scripts/validate.sh`** — everything checkable without a browser: build,
   vet, unit tests, classifier eval, and the full server loop via `cmd/probe`.
2. **Browser E2E, agent-driven (Chrome DevTools MCP)** — real page, fake mic,
   LLM personas; interactive/exploratory. See `docs/BROWSER-QA.md`.
3. **Browser E2E, headless one-command (Playwright)** — same harness, assertion-
   backed regression suite: `cd scripts/browser-e2e/playwright && npm test`.
   Asserts `end_reason` + slot statuses + **real classifier intents** (mirrored via
   the `-qa`-only `qa_intent` channel).

Nothing in the browser layers is mocked — VAD, STT, classifier, TTS, and the
ending logic all run for real; only the *input device* (mic) is faked.

---

## Doc index (secondary — load on demand)

- [README.md](README.md) — quick start, stack, "what's verified and how".
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — components, one turn end-to-end,
  **every turn goes through intent analysis** (§3), **how it ends** (⭐ §4),
  conversation states, WebSocket protocol, tuning knobs.
- [docs/RESEARCH.md](docs/RESEARCH.md) — the thesis: why ending is a state-machine
  problem, the `end_call` action, deterministic backstops, script-done vs.
  bail-early, end-of-turn ≠ end-of-conversation, recommended stack.
- [docs/PACING-RESEARCH.md](docs/PACING-RESEARCH.md) — turn-gap science and why the
  two-beat pacing (ack → ~400ms silent-PCM pause → question) is built the way it is.
- [docs/BROWSER-QA.md](docs/BROWSER-QA.md) — the fake microphone, the audio
  round-trip, fixed clips vs. LLM personas, the `-qa` endpoint, the `qa_intent`
  channel, and both ways to run it (Chrome MCP + Playwright).
- [cmd/eval/EVAL.md](cmd/eval/EVAL.md) — classifier eval strategy.
- [VALIDATION.md](VALIDATION.md) — the living log: every capability, how it's
  validated, and last-run results. **Update this on every change.**

---

## Conventions

- No competitor references anywhere (docs, code, comments, PRs).
- Match surrounding code style; keep comments at the density of the file you edit.
- Ending logic lives in `internal/survey` and `internal/ws` — the LLM classifies,
  it never decides to hang up. Keep that separation.

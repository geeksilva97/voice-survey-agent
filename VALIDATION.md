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
| 3 | `go test ./internal/survey/` | state machine + ending logic (happy, follow-up cap, bail, silence, volunteered-slot) | ok |
| 4 | `go test ./internal/llm/ -run TestClassifyTurn` | "I have to go" → `wants_stop`; clear reply → `answer/sufficient` | ok |
| 5 | `cmd/probe -mode happy` | full STT→LLM→state→TTS loop over WebSocket | `reason=completed` |
| 5 | `cmd/probe -mode silent` | silence backstop | `reason=silence` |

The gate starts a throwaway server on `:8099` and tears it down.

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
go run ./cmd/genclips          # writes web/static/demo/{ans0,ans1,ans2,bail}.wav
go run ./cmd/server            # http://localhost:8090
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
| **Barge-in** | tick "Enable barge-in", play a clip during agent playback | playback stops; turn continues (headphones IRL) |

### Last validated

- **Happy path** — poll `4cebed5b6a`, 5 questions auto-answered, `end_reason=completed`.
- **Silence** — poll `f080ec5d06`, no answer, one reprompt, `end_reason=silence`.
  (both observed live in Chrome on 2026-07-21)

> Note: the greeting is a long clip and the silence window is 9s. When answering
> manually, answer promptly or the silence backstop may fire first — the
> auto-answerer handles this automatically.

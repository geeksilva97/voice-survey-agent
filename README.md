# Voice Survey PoC

A browser voice agent that walks a respondent through an AI-generated opinion
poll — and, per the research in the parent folder, **knows when to stop**:
it ends when the question list is exhausted (happy path), when the respondent
wants to bail, or when they go silent.

Everything runs locally. No ElevenLabs, no paid cloud, no Python — the whole
server side is Go via [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx).

## Stack

| Layer | Choice | Notes |
|---|---|---|
| TTS (voice) | **Kokoro-82M** | natural, Apache-2.0, ~2.3× realtime on CPU |
| STT | **Whisper base.en** | via sherpa-onnx, int8 |
| VAD (pause detection) | **Silero VAD** in the browser | `@ricky0123/vad-web` 0.0.22, WASM |
| Question gen + turn classify | **Ollama** `qwen2.5:3b` | Go-native client |
| Transport | **WebSocket** | `gorilla/websocket`, single connection |
| Speech engine bindings | **sherpa-onnx-go-macos** | STT+TTS+VAD in one Apache-2.0 lib |

## How it ends (the whole point)

The `internal/survey` slot state machine owns termination — not the LLM's gut:

- **completed** — every question answered → closing line → `end_call`.
- **bailed** — a per-turn classifier flags "wants to stop" → graceful early end,
  partial answers saved.
- **silence** — no reply within a window → one reprompt → wrap up.
- Vague/off-topic answers get **one** follow-up probe (capped), then the agent
  moves on — no infinite loops.

## Prerequisites

- macOS arm64 (the sherpa-onnx binding here is `-go-macos`; swap the module for
  Linux/Windows), Go 1.24+.
- [Ollama](https://ollama.com) running with the model:
  ```
  ollama pull qwen2.5:3b
  ollama serve        # if not already running
  ```

## Run

```bash
cd poc
./scripts/fetch-models.sh          # downloads Kokoro + Whisper (~0.5 GB, once)
go run ./cmd/server                # http://localhost:8090
```

Then:

1. Open **http://localhost:8090** — pick a preset (candles, coffee shop, …) or
   describe your product → **Generate**. You get 3–5 questions and a poll link.
2. Open the poll link, **put on headphones**, click **Start & allow microphone**,
   and answer out loud. The agent asks each question, listens for your pause,
   and ends when done.
3. Visit **/results/&lt;id&gt;** to see the captured answers.

Flags: `-addr :8090`, `-model qwen2.5:3b`, `-models ./models`, `-data ./data`.

### Observability (optional)

Set `LANGFUSE_PUBLIC_KEY` + `LANGFUSE_SECRET_KEY` (and optionally
`LANGFUSE_HOST`, default Langfuse Cloud) and two things start exporting to
[Langfuse](https://langfuse.com):

- **the server** traces every classifier turn — question/reply in,
  intent/clarity/ack out, one session per conversation, tagged with the
  classifier prompt version;
- **the eval** publishes each run as a dataset experiment — the 82 labeled cases
  as a dataset, one run per model, per-case pass/fail plus the aggregate scores.
  So accuracy becomes history you can chart against prompt versions instead of
  terminal output that scrolls away.

Langfuse ships no Go SDK; traces go over the official OpenTelemetry Go SDK
(OTLP/HTTP) and the dataset/score APIs over a small `net/http` client. Unset keys
= fully offline, zero overhead, and a Langfuse outage can never fail the gate.

A local instance is already set up at `~/projects/langfuse-local` (UI on
**http://localhost:3001**). Bring it up and use the wrapper, which exports the
keys and fails fast if it isn't running:

```bash
cd ~/projects/langfuse-local && docker compose up -d
cd -   # back to poc
./scripts/with-langfuse.sh go run ./cmd/server
./scripts/with-langfuse.sh go run ./cmd/eval -run my-experiment
```

Details, including the port remaps and headless key provisioning, are in
`internal/obs` and `VALIDATION.md`.

## What's verified (and how)

Run these yourself:

```bash
# 1. Speech backbone (Kokoro TTS + Whisper STT round-trip)
go run ./cmd/spike

# 2. State machine + ending logic (unit tests)
go test ./internal/survey/

# 3. Bail-out + answer classification (needs Ollama)
go test ./internal/llm/ -run TestClassifyTurn -v

# 4. Full conversation over WebSocket, no mic needed:
go run ./cmd/server &            # in one shell
go run ./cmd/probe -mode happy   # walks all questions -> done:completed
go run ./cmd/probe -mode silent  # stays quiet    -> reprompt -> done:silence
```

Verified end-to-end headlessly: question generation, STT, LLM turn
classification (answer / wants_stop / off-topic), follow-up cap, happy-path
completion, silence backstop, bail-out routing, and JSON persistence.

**Needs a real browser + microphone** (build-verified, not yet exercised here):
the in-browser mic capture, Silero VAD endpointing, TTS playback, and **barge-in**
(`Enable barge-in` checkbox on the poll page — requires headphones, since there's
no acoustic echo cancellation over plain WebSocket).

## Layout

```
cmd/server   HTTP + WebSocket + wiring
cmd/spike    Phase-0 proof: TTS+STT from Go
cmd/probe    headless conversation driver (happy | silent)
internal/speech    sherpa-onnx STT+TTS wrapper
internal/survey    slot state machine + ending logic (+ tests)
internal/llm       Ollama question-gen + turn classifier (+ test)
internal/session   poll store + JSON persistence
internal/ws        conversation orchestrator + protocol
web/               setup, poll (voice), results pages + client.js
scripts/           model downloader
```

## Known limitations / next steps

- **Barge-in** needs headphones (no AEC on WebSocket). The documented upgrade is
  WebRTC (`pion/webrtc`), which brings browser-native echo cancellation + Opus.
- Single respondent at a time: the speech engine serializes calls with a mutex.
  For concurrency, pool engines or move STT/TTS behind a worker queue.
- TTS is synthesized per full utterance. For lower latency, stream chunks via
  sherpa's `GenerateWithCallback` (also tightens barge-in).
- VAD assets load from jsDelivr CDN (needs internet). To go fully offline,
  self-host the worklet + `silero_vad_v5.onnx` + ORT wasm and set
  `baseAssetPath`/`onnxWASMBasePath`.
- Linux/Windows: replace `sherpa-onnx-go-macos` with the matching module.
```

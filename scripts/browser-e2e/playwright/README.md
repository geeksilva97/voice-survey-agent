# Persona QA (Playwright)

Headless, one-command, assertion-backed version of the browser persona QA. Same
fake-mic + LLM-persona technique as the Chrome DevTools MCP flow — see
[`docs/BROWSER-QA.md`](../../../docs/BROWSER-QA.md) for the full explanation of how
the fake microphone and the audio round-trip work. This directory is just the
runner.

## Quick start

```bash
cd scripts/browser-e2e/playwright
npm install            # once
npm run setup          # once: download the Chromium build Playwright drives
npm test               # build server (-qa), run all four personas, assert outcomes
```

`npm test` builds the Go server, launches it with the DEV-ONLY `-qa` endpoint on a
dedicated port (default **8091**, so it never touches a dev server on :8090), runs
each persona end-to-end, and tears the server down.

## What it asserts

Outcomes, not exact words (answers are generated fresh each run):

| Persona | Assertion |
|---------|-----------|
| **enthusiast** | `end_reason=completed`; every slot `answered` |
| **neutral** | `completed`; every slot `answered`; `needs_help` never fires (vague ≠ confused) |
| **rusher** | `bailed`; `wants_stop` intent fired; ≥1 answered, ≥1 left unanswered |
| **confused** | `completed`; `needs_help` fired ≥1×; every slot terminal (answered/skipped) |

Ground truth (`end_reason`, slot statuses) comes from the API; the intents come
from the `-qa`-only `qa_intent` channel the server mirrors to the browser. The
persona definitions live in [`internal/qa/personas.go`](../../../internal/qa/personas.go)
(the source of truth for expected behavior) — keep the table above in sync.

## Config (env)

| Var | Default | Meaning |
|-----|---------|---------|
| `QA_PORT` | `8091` | test server port |
| `QA_CLASSIFY_MODEL` | `claude-sonnet-5` | turn-classifier model; set `qwen2.5:3b` for fully offline (lower fidelity) |
| `QA_REUSE_SERVER` | unset | `1` to reuse an already-running `-qa` server on `QA_PORT` |

The Anthropic key is loaded **server-side** from pepita's `.env`; it never appears
here or on the command line.

## On failure

`npm run report` opens the HTML report. Every test attaches an `outcome.json`
(end_reason, slots, intents, spoken turns, transcript) plus a Playwright trace, so
a failure is diagnosable without re-running.

## Recording a demo video (`demo-record.js`)

Runs one persona through the real voice page and produces a shareable MP4 —
screen video plus **the agent's voice**. (Half-voices: the persona's answers are
played into the fake mic, not the speakers, so only the agent is audible; the
on-screen transcript shows both sides.) It captures audio without any OS loopback
device by tapping the page's Web Audio output (`ctx.destination`) into a
`MediaStreamDestination` recorded via `MediaRecorder`, while Playwright records the
silent screen video; `ffmpeg` then muxes the two.

```bash
node demo-record.js                     # enthusiast, against :8090
QA_PERSONA=rusher node demo-record.js   # or confused / neutral
QA_PERSONA=silent node demo-record.js   # respondent never speaks -> silence-backstop ending
QA_BASE=http://localhost:8091 node demo-record.js
```

Output lands in `demo-out/` (gitignored). Needs the server running with `-qa`
(default `:8090`) and `ffmpeg` on `PATH`. `silent` mode injects a fake mic that
only emits silence, so VAD never fires and the server's silence backstop ends the
call — a clean demo of Ava detecting the respondent is gone and wrapping up.

## Relationship to the Chrome MCP flow

Both use the identical harness (`../persona-answerer.js`) and the same `-qa`
endpoint. The MCP flow is for **interactive, exploratory** QA (a human watching and
reacting); this Playwright suite is the **repeatable regression** version for CI.
Neither mocks the pipeline — VAD, STT, the classifier, TTS, and the ending logic
all run for real.

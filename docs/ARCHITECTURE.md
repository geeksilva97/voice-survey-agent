# Architecture

A browser voice agent that walks a respondent through an AI-generated opinion
poll and **decides on its own when the conversation is over**. Everything runs
locally — no cloud STT/TTS.

- **Frontend** (`web/`): mic capture + Silero VAD (endpointing) + streamed TTS
  playback, over one WebSocket. Vanilla JS.
- **Backend** (Go): a WebSocket **conductor** that owns the conversation, a
  **survey state machine** that owns the ending, local **STT/TTS** (sherpa-onnx:
  Whisper + Kokoro), and a small **LLM** (Ollama `qwen2.5:3b`) for question
  generation and per-turn intent classification.

---

## 1. Components

```mermaid
flowchart LR
  subgraph Browser["Browser (web/)"]
    MIC["🎤 getUserMedia"] --> VAD["Silero VAD<br/>(WASM, @ricky0123/vad-web)"]
    VAD -- "speech end → PCM16 16k" --> WSC["WebSocket client<br/>(client.js)"]
    WSC -- "streamed WAV chunks" --> PQ["playback queue<br/>(Web Audio)"]
    PQ --> SPK["🔊 speaker"]
  end

  subgraph Server["Go backend"]
    COND["ws conductor<br/>(internal/ws)"]
    SURVEY["survey state machine<br/>(internal/survey)"]
    SPEECH["speech engine<br/>(internal/speech → sherpa-onnx)"]
    LLM["LLM client<br/>(internal/llm)"]
    STORE["session store + JSON<br/>(internal/session)"]
  end

  WSC <== "WebSocket :8090/ws" ==> COND
  COND --> SURVEY
  COND --> SPEECH
  COND --> LLM
  COND --> STORE
  LLM <--> OLLAMA["Ollama<br/>qwen2.5:3b"]
  SPEECH <--> MODELS[("Whisper base.en<br/>Kokoro-82M")]
```

**Server is authoritative.** The browser only captures speech and plays audio;
every decision (what to say, when to listen, when to end) is made server-side.

---

## 2. One conversational turn

```mermaid
sequenceDiagram
  participant R as Respondent
  participant B as Browser
  participant S as Server
  participant L as LLM

  Note over S,B: agent turn
  S->>B: agent_say {text, index/total}
  S-->>B: WAV chunk (sentence 1)
  S-->>B: WAV chunk (sentence 2…)
  S->>B: tts_end
  B->>R: play streamed audio (queue)
  B->>S: playback_done
  Note over R,B: respondent turn (VAD listening)
  R->>B: speaks
  B->>S: speaking  (resets silence timer)
  R->>B: pause ≈900ms
  B->>S: PCM16 utterance (VAD speech-end)
  S->>S: Whisper STT
  S->>L: classify(question, reply)
  L->>S: {intent, sufficient}
  S->>S: survey state machine decides next
  Note over S,B: next turn, or end
```

TTS is **streamed sentence-by-sentence** so the first words play almost
immediately instead of waiting for the whole reply to synthesize.

---

## 3. Every utterance goes through intent analysis

There is exactly **one** way into the conversation logic: whatever the respondent
says is transcribed, labeled by the classifier, and then routed by Go code. The
greeting, the "ready?" beat, and every question slot all funnel through the same
`ClassifyTurn` with the same prompt — so "actually, I don't have time" ends the
call at hello just as reliably as it does on question four.

```mermaid
flowchart TD
  UTT(["respondent utterance<br/>(PCM16 16k, on VAD speech-end)"]) --> STT["Whisper STT → text"]
  STT --> PRE{"non-speech artifact?<br/>'(coughing)', '[inaudible]'"}
  PRE -- yes --> FORCED["forced 'unintelligible'<br/>(deterministic, no LLM call)"]
  PRE -- no --> CTX{"conversation<br/>context"}

  CTX -- "opening small-talk" --> CLS
  CTX -- "'ready?' beat" --> CLS
  CTX -- "current question slot" --> CLS

  CLS["🧠 ClassifyTurn(question, reply)<br/>one LLM call · temp 0 · JSON schema"] --> TURN
  FORCED --> TURN["{intent, sufficient, clarity, ack}"]

  TURN --> ROUTER{"route on intent<br/>(Go, not the LLM)"}
  ROUTER -- "wants_stop" --> R1["Bail → closing line → END"]
  ROUTER -- "repeat" --> R2["re-read the question verbatim"]
  ROUTER -- "needs_help" --> R3["reassure with 'ack' + re-pose"]
  ROUTER -- "answer + sufficient" --> R4{"clarity?"}
  ROUTER -- "off_topic · unintelligible ·<br/>answer but insufficient" --> R5{"follow-up<br/>budget left?"}

  R4 -- "clear" --> REC["RecordAnswer → advance cursor"]
  R4 -- "unclear" --> REPAIR["confirm once ('did I get that right?')"]
  R5 -- "yes" --> PROBE["one clarifying probe"]
  R5 -- "no" --> CAP["capture or skip → advance cursor"]

  REC --> NEXT(["next agent turn / end check"])
  CAP --> NEXT
  R2 --> LISTEN(["listen again"])
  R3 --> LISTEN
  REPAIR --> LISTEN
  PROBE --> LISTEN
```

The classifier reads the turn on **two axes**: `intent` (what they're doing) and
`clarity` (whether we pinned down the content). Clarity is what triggers a light
repair on a valid-but-hard-to-parse answer instead of throwing it away.

| Intent | Agent does | Slot cursor |
|---|---|---|
| `answer` + sufficient | acknowledge and move on (or confirm once if `unclear`) | advances |
| `answer`, insufficient | one gentle probe, then capture what it got | advances after the probe |
| `wants_stop` | closing line, hang up | — (ends) |
| `repeat` | re-read the question as-is | stays |
| `needs_help` | reassure + hint how to answer, re-pose the question | stays |
| `off_topic` | steer back once, then **skip** (never fabricate an answer) | advances after the probe |
| `unintelligible` | same as off-topic — probe once, then skip | advances after the probe |

Three properties make this safe to run unattended:

- **The LLM labels; it never acts.** Every branch above is a Go `if` in
  `internal/ws`. There is no tool the model can call to hang up, skip a
  question, or re-ask — see [`RESEARCH.md`](RESEARCH.md) for why that matters.
- **It fails open.** A classifier timeout or unparseable JSON degrades to
  `{answer, sufficient, clear}`, so a model glitch advances the survey instead
  of stalling it.
- **Every "stay put" branch is capped.** `repeat` and `needs_help` share a
  re-ask budget, probes are capped per question, and a repair happens at most
  once per slot — so no intent can hold the conversation on one question.

---

## 4. How it knows when to end  ⭐

The core design decision (validated by the research in
[`RESEARCH.md`](RESEARCH.md)): **an LLM cannot reliably feel when a scripted
conversation is "done."** So a deterministic **slot state machine** owns the
ending — the LLM only classifies each reply. The survey ends for exactly one of
three reasons.

```mermaid
flowchart TD
  START(["respondent utterance"]) --> STT["Whisper STT → text"]
  STT --> CLS{"LLM classifies intent"}

  CLS -- "wants_stop" --> BAIL["mark bailed<br/>save partial answers"]
  CLS -- "answer & sufficient" --> REC["record answer<br/>advance slot cursor"]
  CLS -- "vague / off-topic /<br/>unintelligible" --> FU{"follow-ups<br/>left? (cap 1)"}

  FU -- yes --> PROBE["ask one clarifying probe"] --> WAIT1(["listen again"])
  FU -- "no (cap hit)" --> CAP["capture what we got<br/>advance slot cursor"]

  REC --> DONECHK{"any slot<br/>still open?"}
  CAP --> DONECHK
  DONECHK -- yes --> ASK["ask next question"] --> WAIT2(["listen again"])
  DONECHK -- no --> COMPLETED(("END:<br/>completed"))
  BAIL --> BAILED(("END:<br/>bailed"))

  SILENCE["silence timer fires<br/>(no reply / no 'speaking')"] --> STRIKE{"2nd strike?"}
  STRIKE -- "no" --> REPROMPT["'Are you still there?'"] --> WAIT3(["listen again"])
  STRIKE -- "yes" --> SILENT(("END:<br/>silence"))
```

### The three ways it ends

| End reason | Trigger | Owned by |
|---|---|---|
| **completed** | every question slot answered or skipped | state machine (`survey`) |
| **bailed** | a reply classified `wants_stop` ("I have to go") | LLM classifier → state machine |
| **silence** | no reply for `silenceWindow`, twice in a row | server timer (`ws` conductor) |

Two guardrails keep it from misbehaving:

- **Follow-up cap (1 per question).** A vague or off-topic answer earns exactly
  one clarifying probe; after that the agent captures whatever it got and moves
  on — so it can never loop forever on one question.
- **The `speaking` keep-alive.** The silence timer measures *quiet time*, not
  *time since listening began*. While the respondent is talking the browser
  pings `speaking`, which resets the timer — so a long, pause-filled answer
  never trips the "still there?" nudge.

### Conversation states

```mermaid
stateDiagram-v2
  [*] --> Speaking: greeting + Q1 (streamed)
  Speaking --> Listening: playback_done
  Listening --> Thinking: utterance (STT + classify)
  Listening --> Reprompt: silence timer (strike 1)
  Reprompt --> Speaking
  Listening --> Ended: silence timer (strike 2)
  Thinking --> Speaking: ask next / follow-up / closing
  Thinking --> Ended: completed or bailed
  Ended --> [*]
  note right of Listening
    'speaking' pings reset
    the silence timer
  end note
```

---

## 5. WebSocket protocol

Text frames = JSON control; binary frames = audio (PCM16 in, WAV chunks out).

**Client → Server**

| message | meaning |
|---|---|
| `{"type":"ready"}` | page loaded, mic granted |
| `{"type":"speaking"}` | respondent is talking now (resets silence timer) |
| `{"type":"playback_done"}` | agent audio finished; safe to listen |
| `{"type":"barge_in"}` | user talked over the agent (barge-in mode) |
| *(binary)* | PCM16 mono 16 kHz utterance (on VAD speech-end) |

**Server → Client**

| message | meaning |
|---|---|
| `{"type":"agent_say", text, kind, index, total}` | new agent turn (caption + progress) |
| *(binary)* | a WAV sentence chunk for the current turn |
| `{"type":"tts_end"}` | no more audio chunks this turn |
| `{"type":"transcript", text}` | what STT heard |
| `{"type":"cancel"}` | stop/clear playback (barge-in ack) |
| `{"type":"done", reason}` | ended: `completed` \| `bailed` \| `silence` |

Each new WebSocket connection **starts a fresh run** of the same poll, so
reloading `/poll/<id>` re-takes it (used by the Restart button).

---

## 6. Tuning knobs

| Knob | Where | Default | Effect |
|---|---|---|---|
| `redemptionFrames` | `client.js` (VAD) | 28 (~900 ms) | trailing silence before a turn is "done"; raise if it cuts people off |
| `silenceWindow` | `ws.go` | 12 s | how long to wait before the "still there?" nudge |
| `maxSilenceStrikes` | `ws.go` | 2 | nudges before ending on silence |
| `maxFollowUps` | `survey.go` | 1 | clarifying probes per question |
| Kokoro voice id | `speech.go` | 0 | agent voice |

See [`VALIDATION.md`](../VALIDATION.md) for how every layer is tested.

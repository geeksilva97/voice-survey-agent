# Conversational pacing: how to make an agent connect turns like a person

Research brief behind the **two-beat delivery** feature (`-pacing`): after a
respondent answers, the agent says a short acknowledgment, pauses a beat, *then*
asks the next question — instead of reading "ack + question" in one breath.

The motivation: our agent delivered the acknowledgment and the next question in
one breath, which read as a block rather than a conversation. The fix turned out
to be **pacing**, not content — our acknowledgments were already specific; what
was missing was splitting a turn into two beats (ack, then question) with a short
pause between, the way a person naturally does. The goal was to add that pacing
*without* giving up the deterministic state machine that owns the ending.

Three research threads: the conversational science, what production frameworks
actually ship, and how to implement it over our half-duplex pipeline.

---

## 1. The science of pacing (what makes delivery feel human)

**The ~200ms turn-gap is a cross-linguistic universal.** Stivers et al. (2009,
PNAS), across 10 languages, found response offsets cluster around a mode of
0–200ms. English averages ~236ms. Turn-taking universally avoids overlap and
minimizes silence.
- https://www.pnas.org/doi/10.1073/pnas.0903616106

**Instant (<120ms) reads as robotic / not-listening.** Gaps under ~120ms aren't
even perceived as gaps (Heldner 2011). And producing a single word takes ~600ms
from concept to articulation (Indefrey & Levelt 2004) — so a zero-gap reply to a
just-finished question is physically impossible for a human and reads as
pre-scripted. A small, consistent gap signals a live listener.
- https://www.frontiersin.org/journals/psychology/articles/10.3389/fpsyg.2015.00731/full

**Past ~300ms a silence starts to carry meaning; ~700ms signals "bad news."**
Kendrick & Torreira (2015): silence beyond ~300ms lowers the odds a response is
heard as unqualified acceptance. Bögels, Kendrick & Levinson (2015, PLOS ONE)
showed a neural surprise (N400) after a 300ms gap before a "no" that vanished by
1000ms — listeners infer dispreference from the delay itself.
- https://pmc.ncbi.nlm.nih.gov/articles/PMC8504554/
- https://journals.plos.org/plosone/article?id=10.1371/journal.pone.0145474

**A front-loaded acknowledgment is legitimate — if it's crisp, not hesitant.**
Backchannels/acknowledgment tokens ("got it", "I see") signal the listener was
heard, and turn-initial "okay"/"yeah" before an answer is well-attested. A short
token also occupies the response slot and "resets the latency clock" for the
listener. **Caveat:** in conversation analysis, a drawn-out or hesitant preface
is *the* marker of a dispreferred response — so the ack must be prompt and
affiliative, or it reads as reluctance/bad-news.
- https://arxiv.org/pdf/2507.22352
- https://www.frontiersin.org/journals/psychology/articles/10.3389/fpsyg.2021.689275/full

**Chunking into intonation-unit-sized pieces aids comprehension — but cap it.**
Speech is delivered in intonation units at a ~1Hz rhythm (Inbar et al. 2020,
Nature Sci Reports); cortical tracking integrates meaning phrase-by-phrase
(eNeuro 2021). Honoring phrase boundaries with brief pauses matches how the brain
segments input. But over-chunking (many rapid fragments) overwhelms and reads as
spammy — guidance caps splits at ~2–3 units.
- https://www.nature.com/articles/s41598-020-72739-4
- https://www.eneuro.org/content/8/4/ENEURO.0562-20.2021

**When pauses hurt:** an inserted pause can (a) be misread by simple VAD as the
user's turn ending, and (b) if it reads as hesitation, signal reluctance. Keep
any pre-content gap well under ~1s and never shorten the *user's* allowed
silence.

### Design implications
- Target a **~250–500ms gap** before a turn; never sub-120ms.
- **Front-load a crisp ack, one beat (~300–500ms), then the question.** Keep the
  ack confident, not a drawn-out preface.
- Keep the inter-beat pause **under ~700ms**.
- **Cap at two beats.** Don't fragment further.
- Don't let the inserted pause trip endpointing or shorten the user's silence.

---

## 2. What production voice frameworks actually ship

**Bottom line: none ship a first-class "ack, pause, question" primitive.** You
assemble it from two sequential utterances. What they all ship is
**sentence-at-a-time TTS streaming** plus turn/endpointing knobs; a few add
explicit filler/backchannel features.

| Framework | Turn split | TTS chunking | Timed pause | Filler / backchannel |
|-----------|-----------|--------------|-------------|----------------------|
| **LiveKit Agents** | `session.say()` then `generate_reply()` (chain, each returns a `SpeechHandle`) | sentence tokenizer (blingfire) via `StreamAdapter` | none | preemptive generation; adaptive interruption (`backchannel_boundary` (1.0,1.0)); `interruption_ignore_words` proposed, unshipped ([#4450](https://github.com/livekit/agents/issues/4450)) |
| **Pipecat** | inject frames; ack processor only in 3rd-party NVIDIA code | `text_aggregation_mode: SENTENCE` (default) vs `TOKEN`; `stop_frame_timeout_s` 3.0 | none | `UserIdleProcessor` (idle re-prompt) |
| **Vapi** | none | `chunkPlan.minCharacters` (30), `punctuationBoundaries` | none | `backchannelingEnabled` + `fillerInjectionEnabled` **removed** Oct/Nov 2024 (now internal) |
| **Retell AI** | none | not documented | "pause" expressive tag (`enable_expressive_mode`) | `enable_backchannel`, `backchannel_frequency` 0.8, `natural_filler_words` |
| **Bland AI** | none | streaming only | inline marker `<\|N\|>`, N=0.1–10.0s (docs say avoid dashes/ellipses) | none |
| **ElevenLabs CAI** | comma-boundary early start | `chunk_length_schedule` [80,120,200,260], `flush:true` at turn end | SSML `<break time>` ≤3s; v3 `[pause]` tags | `soft_timeout_config` filler ("Let me think", rec ~3.0s, once/turn); interruption ignore terms |
| **Deepgram Voice Agent** | none | Aura-2 synth without waiting for punctuation, ~50ms increments | none | native barge-in; `eager_eot_threshold` → `TurnResumed` |
| **OpenAI Realtime** | multiple/out-of-band `response.create` | incremental PCM16 deltas | none | none; `semantic_vad` eagerness |

Sources: [LiveKit audio](https://docs.livekit.io/agents/multimodality/audio/) ·
[LiveKit adaptive interruption](https://docs.livekit.io/agents/logic/turns/adaptive-interruption-handling/) ·
[Pipecat TTS guide](https://docs.pipecat.ai/guides/learn/text-to-speech) ·
[Vapi voice pipeline](https://docs.vapi.ai/customization/voice-pipeline-configuration) ·
[Vapi changelog 2024/11/24](https://docs.vapi.ai/changelog/2024/11/24) ·
[Retell interaction config](https://docs.retellai.com/build/interaction-configuration) ·
[Bland TTS](https://docs.bland.ai/tutorials/btts) ·
[ElevenLabs conversation flow](https://elevenlabs.io/docs/eleven-agents/customization/conversation-flow) ·
[Deepgram Flux EOT](https://developers.deepgram.com/docs/flux/voice-agent-eager-eot) ·
[OpenAI Realtime](https://developers.openai.com/api/docs/guides/realtime-conversations)

### Patterns that generalize
1. **Sentence/clause-at-a-time TTS streaming** (universal). *We already do this.*
2. **Filler while the LLM is slow** — a spoken placeholder; just a sequential
   utterance, no SSML.
3. **Backchannels + ignoring the user's** — needs VAD/interruption classification.
4. **Explicit timed pauses are rare and never uniform** — only ElevenLabs/Bland/
   Retell expose one, all provider-specific markup (not SSML we can reuse).
5. **Speculative "start early, back off"** — extra LLM cost.

---

## 3. Implementing it over our half-duplex pipeline

Our stack: Go backend, single WebSocket, sherpa-onnx **Kokoro** TTS streamed as
binary WAV frames sentence-by-sentence, browser Web Audio playback, browser
Silero VAD (`@ricky0123/vad-web`), half-duplex, server-side silence timeout reset
by a `speaking` keep-alive ping.

**Kokoro has no SSML** — so a `<break>` tag is off the table. The pause is either
a **client-scheduled Web Audio gap** or a **server-appended silent-PCM buffer**.

**The load-bearing hazard (Pipecat [#453](https://github.com/pipecat-ai/pipecat/issues/453)):**
in a multi-segment turn, a per-segment "TTS stopped" can be misread as
end-of-turn — reopening the mic *mid-turn* so the agent talks over itself or the
silence timer treats the deliberate pause as user silence. The fix is to bracket
the **whole turn**, keep the keep-alive ping flowing through the pause, and re-arm
the mic only when the final segment drains.

Barge-in during the gap (only relevant with the barge-in toggle on): cancel +
flush the queued question segment — don't drain, don't resume. A minimum-duration
guard (Silero `minSpeechFrames`) debounces clicks/backchannels.
- https://hamming.ai/resources/voice-agent-interruption-handling-runbook

### The design we chose: server-side silent-PCM, single-turn envelope

We deliver both beats as **one turn** with a single trailing `tts_end`:

```
agent_say {kind:"ack", text}      → 1st transcript bubble, caption, state=speaking
<binary WAV: ack sentences>
<binary WAV: ~400ms of silence>   ← the beat (speech.Silence)
agent_add {kind:"question", text, index, total}  → 2nd bubble, caption, progress
<binary WAV: question sentences>
tts_end                           → mic re-arms only now, after full drain
```

Why silent-PCM over a client-scheduled gap:

- It keeps the turn **atomic** — nothing downstream ever sees a gap, so the
  existing VAD-pause / silence-timer / mic-rearm logic is untouched. This is the
  direct antidote to the #453 hazard: there is exactly one `tts_end`, and the mic
  re-arms only on final drain.
- It reuses our existing queue + `onended` playback chain with **zero Web Audio
  scheduler surgery** (no `nextStartTime` cursor to add). The silent buffer plays
  like any other buffer.
- Kokoro can't emit SSML anyway, so a `<break>` was never an option.
- Cost: ~400ms of 24kHz mono 16-bit ≈ 19KB per paced turn — negligible on
  localhost/LAN.

The new `agent_add` control message appends the second bubble and updates
caption/progress **without** resetting the turn or re-arming the mic — that's what
makes it a second beat rather than a new turn.

**Pause = 400ms** (`pacingPauseMS`): inside the research's 250–500ms band, well
under the ~700ms dispreference threshold. SSML presets cluster at 200/300/500/700
for short breaks, so 400 is a natural "breath."
- https://speechgen.io/en/node/pausa/

**Graceful degradation:** an empty ack — or `-pacing=false` — collapses to a
single beat with no pause (`speakPaced` falls back to `speakQ(withLead(...))`). So
weak-model turns (where the classifier produces no ack) and the off switch both
behave exactly like the pre-pacing agent.

**Where it's applied:** the two "connect to the next question" transitions — the
normal answer→next-question advance (`askNextOrFinish`) and the opening question
after the consent beat (`startSurvey`). Re-poses (repair/help/off-topic) and the
closing stay single-utterance on purpose — they aren't forward transitions, and
capping the pattern keeps us at the research's 2-beat ceiling.

### Where the code lives
- `internal/speech/speech.go` — `Silence(ms)` (silent WAV at the TTS sample rate).
- `internal/ws/ws.go` — `speakPaced`, `streamTTS`, `pacingPauseMS`, `Handler.Pacing`.
- `web/static/js/client.js` — the `agent_add` case.
- `cmd/server/main.go` — the `-pacing` flag (default on).

---

## Sources

Primary (peer-reviewed): PNAS (Stivers 2009), Frontiers in Psychology
(Levinson & Torreira 2015; Kendrick & Torreira 2015), PLOS ONE (Bögels 2015),
Nature Scientific Reports (Inbar 2020), eNeuro (2021), arXiv HCI (2507.22352,
2508.11781). Framework/implementation: linked inline per row above, plus
[LiveKit sequential pipeline](https://livekit.com/blog/sequential-pipeline-architecture-voice-agents),
[Pipecat #453](https://github.com/pipecat-ai/pipecat/issues/453) /
[#2484](https://github.com/pipecat-ai/pipecat/issues/2484),
[Hamming interruption runbook](https://hamming.ai/resources/voice-agent-interruption-handling-runbook),
[MDN advanced audio sequencing](https://developer.mozilla.org/en-US/docs/Web/API/Web_Audio_API/Advanced_techniques),
[web.dev A Tale of Two Clocks](https://web.dev/articles/audio-scheduling),
[@ricky0123/vad-web](https://www.npmjs.com/package/@ricky0123/vad-web).

Sourcing caveats: the widely-cited "sub-700ms = 73% higher satisfaction" traces
to a vendor blog, not a primary study; Vapi's ~465/965ms latency is a third-party
figure; some LiveKit/Deepgram numeric reference pages 404'd at research time.

# Experiment: agent loop with tool calls vs. classifier + state machine

Branch: `experiment/agent-loop-tool-calls`. Not merged to `main`.

The production design has the LLM **label** each turn (`{intent, sufficient, clarity, ack}`)
while Go owns the flow and the ending. The obvious alternative is to let the model
**act**: give it tools and run an agent loop. This branch builds that, runs both
against the same personas on the same model, and records what actually differs.

The question being answered is the one that matters for a voice product: **is it
more fluid and more natural?**

---

## 1. What was built

`internal/ws/agentloop.go` — a second conversation driver, selected by
`-agent-model claude-sonnet-5`. The model gets four tools:

| Tool | Effect | Ends the turn? |
|---|---|---|
| `record_answer(slot, answer)` | fills a slot by index — callable several times per turn | no |
| `ask_question(slot, preamble)` | speaks an optional lead-in + the **exact** survey wording | yes |
| `say(text)` | speaks anything else (greeting, probe, reassurance) | yes |
| `end_call(farewell, reason)` | speaks a farewell and hangs up | yes (terminates) |

It shares this package's transport with the classifier path — STT, streamed
Kokoro TTS, the WebSocket protocol, the `select` loop, the silence timer — so the
A/B isolates one variable: who decides. In `ws.go` the entire behavioural fork is
three lines choosing `handleUtterance` vs `handleUtteranceAgent`.

Two things stayed in Go **because they cannot move**:

- **The event loop and the silence clock.** Verified below.
- **Question wording.** `ask_question` speaks the survey's exact text and the model
  supplies only the lead-in. An early draft let the model phrase questions itself
  and it immediately began paraphrasing them — which silently changes the survey
  instrument. This is a real constraint, not a stylistic preference: two
  respondents must be asked the same question.

Anthropic-only, on purpose: a 3B local model failing to drive a tool loop would
say nothing about the architecture, so the alternative runs on a model capable of
doing it justice. Thinking is disabled and effort set `low` for the same reason —
adaptive thinking on the critical path of a voice turn is dead air the respondent
hears, so the agent loop gets its best shot at competing on latency.

---

## 2. Fluidity: the honest answer

**No. The agent loop is not more fluid, and on the hardest path it is worse.**

That is the opposite of what I expected going in, which is the reason it was worth
building rather than arguing about.

On the three easy paths the two are **indistinguishable** — 1.00 respondent turns
per question on both, warm specific acknowledgements on both, both reciprocating
when the respondent asked *"how about yours?"*. On the one path that tests
conversational skill (`confused`, a respondent who keeps asking what you mean) the
classifier path handled it and the agent path did not:

**Classifier — asks what you mean, gets helped, twice:**

```
RESP: Um, do you mean like, do I like it or how strong it is? Not sure what angle you're going for.
AVA : Either angle works — whatever comes to mind first. What do you think of the scent?
RESP: Oh um, it's nice, kind of fresh.
```

**Agent — asks what you mean, gets ignored:**

```
RESP: Sure thing, ready to dive in. What specifically do you mean by smell and burn time?
AVA : Great, let's start simple.
```

The agent used *fewer* respondent turns on `confused` (1.50/question vs 2.00) —
and that is not a fluidity win, it is the agent declining to help. The classifier
path spent those extra turns because `needs_help` fired and it reassured and
re-posed the question. Fewer turns is only better when the skipped turns weren't
doing anything.

The agent loop also produced two defects the classifier path structurally cannot:

**A wasted turn** — `say` and `ask_question` are both terminal, and it picked the
wrong one, announcing a question and then waiting instead of asking it:

```
AVA : Vanilla's a lovely choice - I'm partial to it too! Next question for you.
RESP: Alright, let's move on then. What do you think about our next question?    ← turn burned
AVA : How long do your candles typically burn before needing a relight?
```

**A spoken stray quote mark** — it called `say("\"")` and the agent dutifully
synthesized it at the respondent:

```
AVA : "
```

The classifier path can't do either, because it never speaks model output that
isn't a survey question or a length-checked ack. (A `hasSpeech` guard was added
after the run; the transcripts above predate it.)

### Where the agent loop genuinely won

One thing, and it is structural rather than stylistic — **compound utterances**.
The rusher answered and bailed in a single breath:

```
RESP: The burn usually lasts about an hour for me. I've got to run.
      → agent: record_answer(2, "About an hour") + end_call(reason="bailed")
```

`llm.Turn` carries **one** intent. Faced with that utterance it must choose
`answer` or `wants_stop` — and either choice loses information. The tool loop just
did both. This is the same underlying capability as multi-slot fills, and it is
the one thing on this page worth porting back (§8).

Caveat: not a head-to-head. Persona replies are generated fresh, and the
classifier run's rusher bailed *without* answering, so it never faced this
utterance. The structural limitation is real regardless of the run.

### Fluidity, tabulated

Respondent turns per question, **answer phase only** (excluding the fixed
greeting + ready-check overhead, which both paths spend identically):

| persona | classifier | agent | note |
|---|---|---|---|
| enthusiast | 1.00 | 1.00 | wash |
| neutral | 1.00 | 1.00 | wash |
| rusher | 1.00 | 1.00 | wash |
| confused | 2.00 | 1.50 | agent lower **because it skipped helping** |

> ⚠️ **Do not read the raw wall-clock or total-turn numbers as fluidity.** Every
> run regenerates its own questions, so the classifier's enthusiast poll had 5
> questions and the agent's had 3. That makes the 207s-vs-108s gap a question-count
> artifact, not a speed result. Only per-question rates within a phase compare.

---

## 3. Measured results

Four personas × two drivers, `claude-sonnet-5` on both sides, real page + fake mic
+ real VAD/STT/TTS. All 8 runs reached a terminal state; no test errors.

| persona | end_reason (clf / agent) | slots | agent steps/turn | agent avg model call |
|---|---|---|---|---|
| enthusiast | completed / completed | 5 / 3 | 1.20 | 1.97 s |
| neutral | completed / completed | 2 / 2 | 1.25 | 2.52 s |
| rusher | **bailed / bailed** | 2 / 2 | 1.25 | 1.97 s |
| confused | completed / completed | 2 / 2 | 1.40 | 2.96 s |

Both drivers got every ending right, including the rusher's bail.

Agent-loop cost per session: **9.6k–15.0k input tokens, 533–784 output**. The
classifier's prompt is flat (~1.7k in per call) no matter how far into the survey
it is.

From the headless probe (`cmd/probe`, canned off-topic clip, 5 questions):

| | classifier | agent loop |
|---|---|---|
| model round trips | 1 per turn, by construction | 8 over 7 turns = **1.14/turn** |
| avg time inside the model | not instrumented | **3.0 s** per call |
| tokens (one session) | ~1.6k in per call, flat | **19,959 in / 1,138 out**, growing every turn |
| `end_reason` | `completed` | `completed` |

Two things worth flagging in that table:

- **Parallel tool calls largely dissolve the round-trip objection.** The model
  batches `record_answer` + `ask_question` into one response, so the loop runs at
  ~1.1 calls per turn, not the 2–3 predicted. The per-call latency (3.0 s) is the
  real cost, not the number of calls.
- **Input tokens grew 1,683 → 3,573 across one short survey**, because the agent
  loop accumulates history. The classifier's prompt is the same size on question 5
  as on question 1.

---

## 4. Where it breaks: recorded data

The finding that matters most, and it isn't about fluidity at all.

Same input on both paths — the probe's canned off-topic clip, repeated for every
question ("After early nightfall the yellow lamps would light up here and there
the squalid quarter of the brothels", from the Whisper test corpus):

| | classifier path | agent loop |
|---|---|---|
| `end_reason` | `completed` | `completed` |
| slots `answered` | 2 | **5** |
| slots `skipped` | 3 | **0** |
| what's in the `answer` field | the respondent's verbatim words, or empty | the model's **description of the non-answer** |

The agent wrote things like `"No clear answer provided; repeated unrelated quote."`
into the answer field and marked the slot **answered**. It is being helpful — that
is a genuinely useful note for a human reader — and in doing so it destroyed the
distinction between *answered* and *no data*.

Consequences, all real in this codebase:

- `internal/insight` scores those strings as if they were opinions.
- `/results/<id>` and `/insights/<id>` count 5 answers where there were 0.
- Nothing downstream can tell a real answer from a model's summary of silence,
  because both are `status: answered` with prose in the field.

The classifier path is not flawless here either — it stored the verbatim
off-topic line as the answer to two questions, because the classifier labelled it
`answer`/sufficient. But it stored **what the respondent said**. That is
auditable; a model's opinion about what they said is not.

### It is not just the degenerate input

The persona runs confirm the same behaviour on **real, cooperative answers** — all
four, every slot. The agent rewrites the respondent into the third person:

| what the respondent said | classifier stored | agent stored |
|---|---|---|
| *"Every time I light one of your candles, it's like stepping into a cozy spa right away. Love that lavender scent especially."* | the sentence, verbatim | `"Lavender scent — feels like stepping into a cozy spa"` |
| *"It depends on the size and scent throw, but usually about a month or when it gets really small."* | the sentence, verbatim | `"About a month, or until the candle gets small"` |
| *"Not really sure depends on the time of day."* | the sentence, verbatim | `"Not sure, depends on the time of day"` |

The summaries are *good* summaries. That is the problem: they are fluent,
plausible, and they are not what anyone said. A survey's value is that the
respondent's words are the record — you can re-read them, re-code them, quote them,
and count how many people used the word "cozy". You cannot do any of that on a
paraphrase, and nothing downstream flags that a paraphrase is what it got.

The classifier path has its own noise here — storing the raw transcript means the
respondent's own trailing question lands in the answer field
(`"Oh um, it's nice, kind of fresh. Do you want more detail than that or is that
good?"`). That is untidy but recoverable; a rewrite is not.

This is fixable in the agent design (a stricter `record_answer` description, a
`verbatim` field, or a Go-side check that the recorded answer is a substring of the
transcript). It is worth noting that the classifier path gets it right *by
construction* — it stores `text`, the STT output, and `CaptureAndAdvance("")`
records absence — whereas the agent path needs to be *told*, and complied only when
told.

---

## 5. What survived unchanged

**The silence backstop works identically.** `-mode silent` against the agent path:

```
AGENT(followup): Hi there! This is Ava. How's your day going so far?
  · (staying silent)
AGENT(reprompt): Are you still there? Take your time — whenever you're ready.
  · (staying silent)
AGENT(closing): It seems you've stepped away, so I'll wrap up here.
=== DONE, reason=silence ===

AGENT REPORT turns=0 steps=1 ... end_claim=
```

`steps=1` and an empty `end_claim` are the whole point: the model was invoked once
to author the greeting and **never again**, because a respondent who says nothing
generates nothing to invoke it with. Go's timer ended the call. An agent loop
cannot own this — there is no input on which to run inference — so a production
tool-calling voice agent still needs a code-owned clock. This is architecture, not
preference.

---

## 6. Termination

The prediction was that a model owning `end_call` would end early or late. On
these runs **it did not**: every `end_call` claim matched the actual slot tally,
and the instrumented `MISMATCH` warning never fired.

That is a fair result for the alternative and it should be said plainly. The
caveat is the shape of the risk, not its observed rate: `Done()` is `remaining()
== 0`, which cannot be wrong, while `end_call` is a judgment that was correct
every time it was tested. Those are different guarantees, and a survey product
that silently drops a respondent's last two answers fails in a way nobody
notices. The experiment measured an error rate of zero on a handful of runs; it
did not — and could not — establish a bound.

---

## 7. Verdict

**Don't adopt it.** The thing it was supposed to buy — fluidity — it did not
deliver, and the thing it costs is the integrity of the data.

| | winner | margin |
|---|---|---|
| Fluidity on easy paths | tie | indistinguishable (1.00 turns/question both) |
| Fluidity on the hard path | **classifier** | it helps a confused respondent; the agent ignored them |
| Compound utterances (answer + bail) | **agent** | structural — one intent can't express two acts |
| Correct endings | tie | 4/4 both, including the bail |
| Latency | classifier | ~1.2–1.4 calls/turn × ~2–3 s vs. one call |
| Cost | **classifier** | flat ~1.7k prompt vs. 9.6k–15k and growing |
| Data fidelity | **classifier, decisively** | verbatim + honest `skipped` vs. fluent paraphrase |
| Owns the clock | Go, either way | not a choice (§5) |

Two predictions I got wrong, worth recording because they were the reasons to
build it:

1. **Latency was not the disqualifier.** Parallel tool calls batch `record_answer`
   with `ask_question`, so the loop runs at ~1.2 calls per turn, not 2–3. The
   round-trip objection was mostly wrong.
2. **Termination held.** Every `end_call` matched the slot tally; the `MISMATCH`
   warning never fired. The model did not hallucinate completion.

And the one I got backwards entirely: **the agent loop is not more natural.** It is
more *free* — and its freedom showed up as a wasted turn, a spoken quote mark, an
ignored question, and four sessions of rewritten answers, while the classifier path
did the same conversational work with none of them.

The deeper reason is that this task is **not** open-ended. A survey has a fixed
instrument, a fixed goal, and a hard requirement that the record be the
respondent's own words. Agent loops earn their keep when the path can't be
specified in advance. Here it can — so the loop's freedom is all cost and no
benefit, except in the one narrow place where a single utterance does two things at
once.

---

## 8. What to port back to `main`

Ranked by value over risk:

1. **Let one utterance do more than one thing.** The single measured win (§2), and
   it needs no agent loop — only a wider verdict:

   ```json
   {"fills": [{"slot": 2, "answer": "about an hour"}],
    "intent": "wants_stop", "clarity": "clear", "ack": ""}
   ```

   That shape covers both cases the current `Turn` cannot express: answering two
   questions at once, and answering *while* bailing (the rusher utterance that
   forced a lossy either/or). `survey.Fill(idx, answer)` is already on this branch
   and tested (`TestFillByIndexAdvancesCursor`); it also finally gives
   `TryFillOther`'s capability a caller, after being dead code since it was
   written. The state machine, the caps, the clock and the ending all stay exactly
   where they are.
2. **A verbatim-answer guard.** Whatever records an answer, assert it came from
   the transcript. The agent path exposed that nothing enforces this today.
3. **Nothing else.** The remaining agent-loop advantages are consequences of the
   model composing turns freely, which is exactly what the fidelity result says
   not to allow for a survey instrument.

---

## 9. Reproducing

```bash
# classifier path (production)
go run ./cmd/server -qa -classify-model claude-sonnet-5

# agent loop (this branch)
go run ./cmd/server -qa -agent-model claude-sonnet-5

# headless, either path
go run ./cmd/probe -addr localhost:8090 -mode happy
go run ./cmd/probe -addr localhost:8090 -mode silent

# the A/B itself (personas, both paths, transcripts to JSON)
cd scripts/browser-e2e/playwright
QA_REUSE_SERVER=1 QA_PORT=8091 QA_LABEL=agent QA_OUT=/tmp/ab-agent.json \
  npx playwright test compare.spec.js --retries=0
```

The agent path logs a per-session `AGENT REPORT` line (turns, model round trips,
steps/turn, wall time in the model, token spend, slot tally, end claim) and a
`MISMATCH` warning whenever `end_call` claims `completed` with slots still open.

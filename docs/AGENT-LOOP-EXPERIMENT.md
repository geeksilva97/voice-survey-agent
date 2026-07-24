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

⏳ **PENDING — the persona A/B was still running when this section was written.**
Do not cite this section until it is filled in from `§3`. Nothing below this line
in §2 is a measured claim yet.

---

## 3. Measured results

⏳ **PENDING** — to be filled from the A/B run (4 personas × 2 drivers, same
model, transcripts recorded to JSON).

Already measured, from the headless probe (`cmd/probe`, canned off-topic clip,
5-question survey, `claude-sonnet-5` both sides):

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

This is fixable in the agent design (a stricter `record_answer` description, a
`verbatim` field, a Go-side check that the recorded answer is a substring of the
transcript). It is worth noting that the classifier path gets it right *by
construction* — `CaptureAndAdvance("")` records absence — whereas the agent path
needs to be *told*, and complied only when told.

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

Established so far:

- **Latency: closer than predicted.** Parallel tool calls collapse
  `record_answer` + `ask_question` into a single round trip, so the loop runs at
  ~1.1 model calls per turn rather than the 2–3 assumed. Per-call latency (~3 s)
  is the cost that matters.
- **Cost: the classifier path wins.** Flat prompt vs. history that grows every
  turn.
- **Data fidelity: the classifier path wins decisively** (§4) — and this is the
  finding with real product consequences.
- **The clock stays in Go either way** (§5). Not a preference; an invariant.
- **Termination held up** on the runs tested (§6), against prediction.

⏳ **Fluidity: pending §2/§3.** The whole point of the experiment, and the one
claim not yet backed by data at time of writing.

---

## 8. What to port back to `main`

Ranked by value over risk:

1. **Multi-slot fills.** `survey.Fill(idx, answer)` (added on this branch, tested
   in `survey_test.go`) plus a `fills[]` array in the classifier's JSON output.
   Needs no agent loop, and it finally gives `TryFillOther` a caller — that
   function has been dead code since it was written. (How much of the fluidity
   difference this actually accounts for depends on §3; the capability gap is real
   either way, since the classifier's verdict structurally cannot express it.)
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

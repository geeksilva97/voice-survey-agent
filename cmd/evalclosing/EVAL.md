# Evaluating the closing line (`cmd/evalclosing`)

This harness measures the **last thing a respondent hears** — the personalized
sign-off the agent speaks before it hangs up. In a project about *knowing when to
stop*, that line is the audible form of stopping, so it's worth a number rather
than a vibe.

It exists because of one observed defect, and it's a good illustration of why
"measure before you fix" matters.

---

## The defect it was built for

A browser QA run produced this closing:

> *"**Sure thing**, adding more exotic scents sounds like it would appeal to your
> love for coastal and tropical blends. Thanks so much for sharing!"*

The respondent had opened every answer with *"Sure thing, …"*. The agent absorbed
their verbal tic and led its farewell with it — and *"sure thing"* answers a
request rather than closing a conversation, so the register is wrong too.

Two claims were made about that line. Measurement upheld one and killed the other:

| Claim | Verdict |
|---|---|
| It echoed the respondent's filler | **Confirmed** — reproduces about half the time |
| It reused their words verbatim, against "their idea, not their exact words" | **Refuted** — 0 of 6 runs copied a phrase; *"coastal and tropical blends"* is a genuine paraphrase of *"a blend of coastal vibes with some tropical touches"* |

That second row is the point of the harness. The verbatim-reuse reading was
plausible, repeated confidently, and wrong. `LongestCopiedSpan` settles it in
milliseconds, and a test pins the paraphrase case so the misreading can't return.

---

## Everything here is deterministic

There is **no LLM judge**, deliberately. The defect is exactly string-detectable,
so a judge would insert cost and noise between you and the number. This mirrors
the asymmetry in [`cmd/eval/EVAL.md`](../eval/EVAL.md): deterministic checks
decide, model-graded ones only inform.

| Metric | What it measures |
|---|---|
| **clean opener** | the line does NOT open with a filler phrase — *the metric a fix targets* |
| **clean opener, tic cases** | the same, restricted to transcripts where the respondent used a tic — where the defect actually lives |
| **within 35 words** | `ClosingSystem`'s length rule, counted exactly |
| **no question mark** | a farewell must not ask something the agent is about to stop listening for |
| **mean copied span** | longest run of consecutive words lifted from the respondent — *informational, never a verdict* |

`copied_span` stays informational on purpose: a long shared span is sometimes
legitimate, because occasionally there is only one sensible way to name a product
feature.

### Why the filler check requires a comma

`FillerOpener` only fires when the filler is followed by a comma. That is what
separates *"Well, thanks so much"* (a tic) from *"Well-made candles are worth the
wait"* (content), and it's why the guard can strip without mangling.

Fillers also **stack**. The very first baseline run produced *"Okay so, now I
check…"*, which a single-phrase matcher missed completely — so up to two
consecutive fillers are consumed before the comma is required. That gap is pinned
by `TestFillerOpenerStacked`; finding it *before* recording the baseline is why
the first run's numbers were discarded.

---

## Fidelity: it drives the real prompt

The harness calls `ws.ClosingSystem`, `ws.ClosingUserPrompt`, `ws.CloseTranscript`
and `ws.SanitizeClosing` directly, and builds a real `survey.Survey` per case so
the transcript is rendered by production code rather than hand-typed here.

Those five identifiers were exported from `internal/ws` for exactly this reason.
Re-typing the prompt into the harness is the classic way an eval silently stops
measuring the thing that ships.

---

## The corpus (`corpus.go`)

Hand-written, like `cmd/eval`'s — engineered to stress the failure, not sampled
from traffic. Every case uses the project's QA product (hand-poured scented soy
candles); what varies is the **respondent's voice**, because that is what the
agent absorbs.

- **tic cases** — every answer opens with a discourse marker (`Sure thing,`
  `Honestly,` `I mean,` `To be fair,` `You know,` `Well,` `Like,` `Okay so,`).
- **control cases** — clean, terse, rambling, critical, and one with filler
  *mid-sentence*. These are the important half: if a control ever fails the
  clean-opener check, the guard is over-reaching and stripping legitimate lines.

`TestCorpusWellFormed` enforces that both kinds stay present.

---

## Running it

```bash
# Local model, print only
go run ./cmd/evalclosing

# Publish the run to Langfuse as a dataset experiment
./scripts/with-langfuse.sh go run ./cmd/evalclosing -run baseline-before-fix

# See the ceiling on a stronger writer
go run ./cmd/evalclosing -model claude-sonnet-5
```

**Not wired into `scripts/validate.sh`.** The gate must stay fast and offline, and
this needs a live model. The scorer unit tests (`go test ./cmd/evalclosing/`) run
anywhere and are the part worth gating.

---

## Langfuse export

With `LANGFUSE_*` credentials set, each run publishes as a dataset experiment:

- the transcript corpus becomes the dataset `closing-line` (items upsert by a
  content hash, so re-pushing every run creates no duplicates);
- each run is `<model>@<run>`, with the **closing prompt fingerprint** in its
  metadata;
- each case links its generated line's trace as a run item, with per-case
  `clean_opener` / `within_35_words` / `no_question_mark` booleans and a numeric
  `copied_span_words`;
- run-level aggregates: `clean_opener_rate`, `within_word_cap_rate`,
  `no_question_rate`.

`ws.ClosingPromptVersion()` is the load-bearing piece. It hashes `ClosingSystem`,
so editing the prompt changes the fingerprint and Langfuse splits before/after on
its own. Without it, old and new output pile up in one undifferentiated set — which
was the actual state of things before this harness existed.

### Two rates, and why the final one is not the interesting one

Once `ws.StripFillerOpener` is in the production path, the **post-guard** rate is
100% by construction — it verifies the guard is wired in, nothing more. The
**pre-guard (MODEL)** rate is the only one that moves when the prompt changes, so
that is the headline for a prompt experiment:

| Score | Reads |
|---|---|
| `model_clean_opener_rate` | prompt quality — did the model get it right unaided? |
| `clean_opener_rate` | what the respondent actually hears — should be 1.0 |
| difference | how often the guard had to intervene |

### Baseline — 2026-07-29, `qwen2.5:3b`, closing prompt `a2844dfca273`

| Metric | Result |
|---|---|
| clean opener | **9/13 — 69.2%** |
| clean opener, tic cases | **4/8 — 50.0%** |
| within 35 words | 13/13 |
| no question mark | 13/13 |
| mean copied span | 2.2 words |

Run name `qwen2.5:3b@baseline-before-fix`. **On tic transcripts the agent parrots
the respondent's filler half the time**, and every control case passed — so the
defect is specific to tic input, not a general register problem.

The worst single case is `okay-so`, which failed both checks at once: it opened
with the filler *and* lifted nine consecutive words, reproducing the respondent's
sentence nearly whole.

---

### After the fix — closing prompt `99bfe50db4e1`

The fix was the prompt rule plus `ws.StripFillerOpener`. Result on the
filler-opener metric, over 15 cases:

| Metric | Before (`a2844dfca273`) | After (`99bfe50db4e1`) |
|---|---|---|
| MODEL clean opener | 69.2% | **86.7%** |
| MODEL clean opener, tic cases | **50.0%** | **75.0%** |
| FINAL clean opener (post-guard) | 69.2% | **100%** |
| guard fired | n/a | 2/15 |

Langfuse runs `qwen2.5:3b@baseline-before-fix` → `qwen2.5:3b@after-fix-v2-with-anchors`.

**Two regressions the harness caught along the way — both worth reading.**

**1. A prompt example became a template.** The first attempt at the rule ended with
a worked example: *Open with the substance or a warm lead-in ("Lavender for the
bedroom — lovely.", "Glad the scent lasts.")*. The 3B model copied it into **all 13
cases**, telling a respondent who had said only *"Vanilla."* about lavender and
bedrooms — a straight fabrication, against the prompt's own "never invent
anything". Every deterministic score sat at 100% while the output got far worse.

The lesson is specific and reusable: **negative examples were safe, the positive
example was not.** The filler phrases listed as things NOT to do were never
copied. Removing the positive example fixed it. `UnsupportedWords` was added as a
tripwire for this class, and `TestUnsupportedWords` pins the exact case.

**2. Verbatim parroting, found by browser QA and missed by this corpus.** After the
fix, a live run produced a closing that copied **16 consecutive words** from the
respondent's last answer:

> respondent: *"Price might be a bit steep but if they had a loyalty program or
> discounts I'd buy again."*
> agent: *"Price might be a bit steep, but if they had a loyalty program or
> discounts, I'd definitely buy again. Thanks for your time!"*

This corpus missed the whole class because every case had short answers — the tic
cases are brief and the controls terse. **A long, fluent, quotable final answer is
the shape that tempts wholesale repetition**, and there wasn't one. The
`quotable-long-answer` and `quotable-two-clause` cases were added from that live
transcript and now reproduce it deterministically at 16 words.

That also corrects an earlier over-claim recorded above: the verbatim-reuse concern
was marked "refuted" on the strength of 6 transcripts, none of which had this
shape. The refutation was **under-powered, not wrong** — with the right input the
behaviour is real and frequent. Treat a "refuted" verdict as only as strong as the
shapes the corpus actually contains.

### Known open: `unsupported_words` has poor precision

It fires on 14 of 15 cases, mostly on the agent's own reaction vocabulary
("cozy", "perfect", "classic", "soothing"). It decisively catches an outright
fabrication like the lavender case — which is why it and its test are kept — but at
this hit rate it cannot be acted on per case. Separating an invented noun from a
reaction adjective needs part-of-speech awareness or, as originally scoped, an
actual LLM judge for groundedness. It is reported and labelled low-precision rather
than quietly presented as a real signal.

## Extending it

- **Add a case** by appending to `corpus`. Add a control alongside any new tic
  case, so an over-reaching guard has something to trip on.
- **Adding a case is how a fix gets locked in** — add the transcript that
  misbehaved, watch the rate drop, then fix.
- **Groundedness and specificity** are the criteria that genuinely need a judge
  (did it invent a claim? is it more than a pleasantry?). Those belong in a
  separate ungated pass, or in a Langfuse live evaluator over production
  `closing_line` traces — not here.

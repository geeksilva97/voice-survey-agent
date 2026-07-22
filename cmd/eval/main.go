// Command eval scores the per-turn intent classifier against a broad labeled
// dataset (see dataset.go) using LIVE models. It can run several models and
// print a side-by-side comparison matrix plus a per-model detail block
// (confusion matrix, per-intent P/R/F1, failures).
//
// The classifier decides whether the agent advances, re-reads, follows up, or
// ends — so a misclassification is what makes a conversation feel wrong (e.g.
// re-asking an already-answered question). Two headline metrics:
//   - overall intent accuracy (all five intents)
//   - valid-answer acceptance: of replies that ARE answers, how many were
//     classified answer AND sufficient — maps to "doesn't re-ask answered Qs".
//
// Models are routed by name: anything containing "claude"/"sonnet"/"opus"/
// "haiku" hits the Anthropic API (key from $ANTHROPIC_API_KEY, else pepita's
// .env); everything else goes through the local Ollama daemon (cloud models
// like "glm-5.2:cloud" are proxied by Ollama).
//
// By default it runs ALL models in defaultModels; pass -models to narrow. The
// FIRST model listed is the gate model: its pass/fail sets the exit code. Other
// models are comparison-only and never fail the gate — handy when a cloud model
// is down. scripts/validate.sh pins -models to local qwen so the gate stays
// fast/offline.
//
// Usage:
//
//	go run ./cmd/eval                         # all models (comparison matrix)
//	go run ./cmd/eval -models qwen2.5:3b      # just the local gate model
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"voicesurvey/internal/llm"
)

var intents = []llm.Intent{
	llm.IntentAnswer, llm.IntentWantsStop, llm.IntentRepeat,
	llm.IntentNeedsHelp, llm.IntentOffTopic, llm.IntentUnintellig,
}

func short(i llm.Intent) string {
	switch i {
	case llm.IntentAnswer:
		return "answer"
	case llm.IntentWantsStop:
		return "stop"
	case llm.IntentRepeat:
		return "repeat"
	case llm.IntentNeedsHelp:
		return "help"
	case llm.IntentOffTopic:
		return "offtop"
	case llm.IntentUnintellig:
		return "unintl"
	}
	return string(i)
}

// report holds one model's results.
type report struct {
	model    string
	buildErr error // fatal: couldn't construct the classifier at all
	cm        map[llm.Intent]map[llm.Intent]int
	correct   int
	total     int
	ansTotal  int
	ansOK     int
	clarTotal int // cases with an expected clarity label
	clarOK    int // clarity classified correctly
	ackTotal  int // cases where an ack is expected (judged, ungated)
	ackGood   int // acks the judge rated good
	ackBad    []ackFail
	errs      int // per-case classify errors
	failures  []failure
	elapsed   time.Duration
}

type failure struct {
	c           evalCase
	got         llm.Turn
	err         error
	clarityMiss bool // intent correct but clarity axis wrong
}

// ackFail is one acknowledgment the judge rated poor (for the detail block).
type ackFail struct {
	reply  string
	ack    string
	reason string
}

func (r report) acc() float64      { return ratio(r.correct, r.total) }
func (r report) ansRate() float64  { return ratio(r.ansOK, r.ansTotal) }
func (r report) clarRate() float64 { return ratio(r.clarOK, r.clarTotal) }
func (r report) ackRate() float64  { return ratio(r.ackGood, r.ackTotal) }
func (r report) recall(k llm.Intent) float64 {
	tp := r.cm[k][k]
	support := 0
	for _, g := range intents {
		support += r.cm[k][g]
	}
	return ratio(tp, support)
}

// defaultModels run when -models is not given. First entry is the gate model.
var defaultModels = []string{"qwen2.5:3b", "glm-5.2:cloud", "gemma4:31b-cloud", "claude-sonnet-5"}

func main() {
	models := flag.String("models", strings.Join(defaultModels, ","), "comma-separated model ids; first is the gate model")
	conc := flag.Int("c", 4, "concurrent classify calls per model")
	minAcc := flag.Float64("min-acc", 0.90, "minimum overall intent accuracy to pass")
	minAns := flag.Float64("min-answer", 0.95, "minimum valid-answer acceptance rate to pass")
	pepitaEnv := flag.String("pepita-env", llm.DefaultAnthropicEnvFile(), "path to pepita .env for ANTHROPIC_API_KEY")
	judgeModel := flag.String("judge", "claude-sonnet-5", "model that judges acknowledgment quality (ungated); empty to skip")
	flag.Parse()

	names := splitCSV(*models)
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "no models given")
		os.Exit(2)
	}

	// The ack judge is a single fixed model used across every evaluated model, so
	// the ack-quality metric is comparable. It's ungated and best-effort: if it
	// can't be built (e.g. no key, offline), we just skip ack scoring.
	var judge llm.Completer
	if jm := strings.TrimSpace(*judgeModel); jm != "" {
		j, err := buildCompleter(jm, *pepitaEnv)
		if err != nil {
			fmt.Printf("(ack judge %q unavailable, skipping ack scoring: %v)\n", jm, err)
		} else {
			judge = j
			fmt.Printf("ack judge: %s\n", jm)
		}
	}

	fmt.Printf("Intent-classification eval — cases=%d concurrency=%d\nmodels: %s\n\n",
		len(dataset), *conc, strings.Join(names, ", "))

	reports := make([]report, 0, len(names))
	for _, name := range names {
		cl, err := buildClassifier(name, *pepitaEnv)
		if err != nil {
			fmt.Printf("### %s — SKIPPED: %v\n\n", name, err)
			reports = append(reports, report{model: name, buildErr: err})
			continue
		}
		rep := evaluate(name, cl, *conc, judge)
		printReport(rep)
		reports = append(reports, rep)
	}

	printMatrix(reports, *minAcc, *minAns)

	// Exit code is driven by the gate (first) model only.
	gate := reports[0]
	if gate.buildErr != nil {
		fmt.Printf("\nGATE model %q could not run: %v\n", gate.model, gate.buildErr)
		os.Exit(1)
	}
	if gate.acc() >= *minAcc && gate.ansRate() >= *minAns {
		fmt.Printf("\nEVAL PASSED — gate model %q: acc %.1f%%, answer %.1f%%\n",
			gate.model, 100*gate.acc(), 100*gate.ansRate())
		return
	}
	fmt.Printf("\nEVAL FAILED — gate model %q: need acc>=%.0f%% (got %.1f%%), answer>=%.0f%% (got %.1f%%)\n",
		gate.model, 100**minAcc, 100*gate.acc(), 100**minAns, 100*gate.ansRate())
	os.Exit(1)
}

// evaluate runs the whole dataset through one classifier with a worker pool,
// then (if a judge is given) scores the acknowledgments the classifier produced.
func evaluate(name string, cl llm.Classifier, conc int, judge llm.Completer) report {
	start := time.Now()
	type outcome struct {
		c   evalCase
		got llm.Turn
		err error
	}
	outcomes := make([]outcome, len(dataset))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := range dataset {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			c := dataset[i]
			turn, err := cl.ClassifyTurn(ctx, c.q, c.reply)
			outcomes[i] = outcome{c: c, got: turn, err: err}
		}(i)
	}
	wg.Wait()

	rep := report{model: name, cm: map[llm.Intent]map[llm.Intent]int{}, elapsed: time.Since(start)}
	for _, w := range intents {
		rep.cm[w] = map[llm.Intent]int{}
	}
	for _, o := range outcomes {
		if o.err != nil {
			rep.errs++
			rep.failures = append(rep.failures, failure{c: o.c, err: o.err})
			continue
		}
		rep.total++
		rep.cm[o.c.want][o.got.Intent]++
		intentOK := o.got.Intent == o.c.want
		if intentOK {
			rep.correct++
		} else {
			rep.failures = append(rep.failures, failure{c: o.c, got: o.got})
		}
		if o.c.want == llm.IntentAnswer {
			rep.ansTotal++
			if o.got.Intent == llm.IntentAnswer && o.got.Sufficient {
				rep.ansOK++
			} else if intentOK {
				// answer but sufficient=false → would trigger an unwanted follow-up.
				rep.failures = append(rep.failures, failure{c: o.c, got: o.got})
			}
		}
		// Clarity axis (only where we have an expectation).
		if o.c.clarity != "" {
			rep.clarTotal++
			if o.got.Clarity == o.c.clarity {
				rep.clarOK++
			} else if intentOK {
				// intent right but clarity wrong → flag it (drives over/under-confirming).
				rep.failures = append(rep.failures, failure{c: o.c, got: o.got, clarityMiss: true})
			}
		}
	}

	// Acknowledgment quality (ungated). For every case where the product would
	// SPEAK an ack — a clear answer or an off-topic steer-back — ask the judge
	// whether the ack the classifier produced is good. Judged on ground-truth
	// expectation so it measures "does this model give good acks where we want
	// them", independent of whether its own intent/clarity call was right.
	if judge != nil {
		var mu sync.Mutex
		var jwg sync.WaitGroup
		for _, o := range outcomes {
			if o.err != nil || !ackExpected(o.c) {
				continue
			}
			jwg.Add(1)
			sem <- struct{}{}
			go func(c evalCase, ack string) {
				defer jwg.Done()
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				good, reason, err := judgeAck(ctx, judge, c, ack)
				if err != nil {
					return // best-effort: a judge glitch doesn't count against the model
				}
				mu.Lock()
				rep.ackTotal++
				if good {
					rep.ackGood++
				} else {
					rep.ackBad = append(rep.ackBad, ackFail{reply: c.reply, ack: ack, reason: reason})
				}
				mu.Unlock()
			}(o.c, o.got.Ack)
		}
		jwg.Wait()
	}
	return rep
}

// ackExpected reports whether the agent would speak an acknowledgment for this
// case: a clear answer (reflect-back), an off-topic reply (warm steer-back), or
// a needs-help reply (reassurance + hint how to answer).
func ackExpected(c evalCase) bool {
	return (c.want == llm.IntentAnswer && c.clarity == clear) ||
		c.want == llm.IntentOffTopic || c.want == llm.IntentNeedsHelp
}

func printReport(r report) {
	fmt.Printf("### %s  (%.1f%% acc, %.1f%% answer-accept, %.1f%% clarity, %d err, %s)\n",
		r.model, 100*r.acc(), 100*r.ansRate(), 100*r.clarRate(), r.errs, r.elapsed.Round(time.Millisecond))

	// Confusion matrix.
	fmt.Printf("%8s", "")
	for _, g := range intents {
		fmt.Printf("%8s", short(g))
	}
	fmt.Println("   total")
	for _, w := range intents {
		fmt.Printf("%8s", short(w))
		rowTotal := 0
		for _, g := range intents {
			fmt.Printf("%8d", r.cm[w][g])
			rowTotal += r.cm[w][g]
		}
		fmt.Printf("%8d\n", rowTotal)
	}

	// Failures (de-duped).
	if len(r.failures) > 0 {
		sort.SliceStable(r.failures, func(i, j int) bool {
			return short(r.failures[i].c.want) < short(r.failures[j].c.want)
		})
		fmt.Printf("failures (%d):\n", len(r.failures))
		seen := map[string]bool{}
		for _, f := range r.failures {
			key := f.c.q + "|" + f.c.reply
			if seen[key] {
				continue
			}
			seen[key] = true
			if f.err != nil {
				fmt.Printf("  ERROR  R:%q  %v\n", f.c.reply, f.err)
				continue
			}
			if f.clarityMiss {
				fmt.Printf("  clarity want=%-7s got=%-7s (intent ok)  R:%q\n",
					f.c.clarity, f.got.Clarity, f.c.reply)
				continue
			}
			note := ""
			if f.c.want == llm.IntentAnswer && f.got.Intent == llm.IntentAnswer && !f.got.Sufficient {
				note = " [answer but sufficient=false → unwanted follow-up]"
			}
			fmt.Printf("  want=%-6s got=%-6s(suff=%v)  R:%q%s\n",
				short(f.c.want), short(f.got.Intent), f.got.Sufficient, f.c.reply, note)
		}
	}

	// Acknowledgment quality (ungated) — a sample of the acks the judge disliked.
	if r.ackTotal > 0 {
		fmt.Printf("ack quality: %d/%d good (%.1f%%)\n", r.ackGood, r.ackTotal, 100*r.ackRate())
		for i, a := range r.ackBad {
			if i >= 6 {
				fmt.Printf("  …and %d more\n", len(r.ackBad)-6)
				break
			}
			fmt.Printf("  bad ack %q  (%s)  R:%q\n", a.ack, a.reason, a.reply)
		}
	}
	fmt.Println()
}

func printMatrix(reports []report, minAcc, minAns float64) {
	fmt.Println("=== comparison matrix (acc/ans✓/clar/ack = headline; per-intent = recall) ===")
	fmt.Printf("%-20s %7s %7s %7s %7s %7s %7s %7s %7s %7s %7s %8s\n",
		"model", "acc", "ans✓", "clar", "ack", "answer", "stop", "repeat", "help", "offtop", "unintl", "time")
	for i, r := range reports {
		tag := ""
		if i == 0 {
			tag = "(gate)"
		}
		if r.buildErr != nil {
			fmt.Printf("%-20s %7s  %s %s\n", r.model, "—", "SKIPPED", tag)
			continue
		}
		status := "pass"
		if i == 0 && !(r.acc() >= minAcc && r.ansRate() >= minAns) {
			status = "FAIL"
		}
		ackCell := "      —"
		if r.ackTotal > 0 {
			ackCell = fmt.Sprintf("%6.1f%%", 100*r.ackRate())
		}
		fmt.Printf("%-20s %6.1f%% %6.1f%% %6.1f%% %7s %6.1f%% %6.1f%% %6.1f%% %6.1f%% %6.1f%% %6.1f%% %7s  %s %s\n",
			trunc20(r.model),
			100*r.acc(), 100*r.ansRate(), 100*r.clarRate(), ackCell,
			100*r.recall(llm.IntentAnswer), 100*r.recall(llm.IntentWantsStop),
			100*r.recall(llm.IntentRepeat), 100*r.recall(llm.IntentNeedsHelp),
			100*r.recall(llm.IntentOffTopic), 100*r.recall(llm.IntentUnintellig),
			r.elapsed.Round(time.Millisecond), status, tag)
	}
	fmt.Printf("\nthresholds: acc>=%.0f%%, answer-accept>=%.0f%%  (gate model only; clar/ack ungated)\n",
		100*minAcc, 100*minAns)
}

// ---- classifier routing + key loading ----

func buildClassifier(name, pepitaEnv string) (llm.Classifier, error) {
	var key string
	if llm.IsAnthropicModel(name) {
		var err error
		if key, err = llm.LoadAnthropicKey(pepitaEnv); err != nil {
			return nil, err
		}
	}
	return llm.NewClassifier(name, key)
}

func buildCompleter(name, pepitaEnv string) (llm.Completer, error) {
	var key string
	if llm.IsAnthropicModel(name) {
		var err error
		if key, err = llm.LoadAnthropicKey(pepitaEnv); err != nil {
			return nil, err
		}
	}
	return llm.NewCompleter(name, key)
}

// ---- acknowledgment judge (ungated, generative score) ----

const ackJudgeSystem = "You grade a single one-line acknowledgment produced by a survey " +
	"voice-agent. Right before asking its next question, the agent says this short line so it " +
	"sounds human instead of like a form. Given the QUESTION the respondent just answered, their " +
	"REPLY, whether the reply was OFF-TOPIC, and the agent's ACK, decide if the ack is good.\n" +
	"A GOOD ack is: short (a phrase, not a full sentence, never a question), natural and spoken, " +
	"and SPECIFIC to what the respondent said. For a normal answer it briefly reflects their point " +
	"back. For an OFF-TOPIC reply it lightly acknowledges the aside and steers back WITHOUT engaging " +
	"the tangent. For a NEEDS-HELP reply (the respondent didn't know how to answer or asked the agent " +
	"to clarify) a good ack reassures them and hints HOW to answer, addressing their specific confusion " +
	"('No need for a score — just your honest gut feeling.'); it should NOT restate the question.\n" +
	"A BAD ack is: empty when one is expected, generic/templated (a bare 'Got it, thanks' with no " +
	"specificity), phrased as a question, rude or dismissive, too long/rambly, — for an off-topic " +
	"reply — actually engaging the tangent, or — for a needs-help reply — unhelpful or just repeating " +
	"the question.\n" +
	`Respond ONLY as JSON: {"good": true|false, "reason": "a few words"}.`

// judgeAck asks the judge model whether one produced ack is good.
func judgeAck(ctx context.Context, judge llm.Completer, c evalCase, ack string) (bool, string, error) {
	user := fmt.Sprintf("QUESTION: %s\nREPLY: %s\nOFF-TOPIC: %v\nNEEDS-HELP: %v\nACK: %q",
		c.q, c.reply, c.want == llm.IntentOffTopic, c.want == llm.IntentNeedsHelp, ack)
	raw, err := judge.Complete(ctx, ackJudgeSystem, user)
	if err != nil {
		return false, "", err
	}
	good, reason, ok := parseJudge(raw)
	if !ok {
		return false, "", fmt.Errorf("unparseable judge output: %q", trunc20(raw))
	}
	return good, reason, nil
}

// parseJudge isolates the first JSON object and reads {good, reason}.
func parseJudge(raw string) (good bool, reason string, ok bool) {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		if j := strings.LastIndexByte(raw, '}'); j > i {
			raw = raw[i : j+1]
		}
	}
	var v struct {
		Good   bool   `json:"good"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil {
		return false, "", false
	}
	return v.Good, v.Reason, true
}

// ---- small helpers ----

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func trunc20(s string) string {
	if len(s) > 20 {
		return s[:19] + "…"
	}
	return s
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

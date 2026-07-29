// Command evalclosing measures the CLOSING LINE — the last thing a respondent
// hears before the agent hangs up.
//
// It exists because a browser QA run caught the agent opening its farewell with
// the respondent's own filler ("Sure thing, thanks for sharing!"), against
// ClosingSystem's explicit instruction to use "their idea, not their exact
// words". A single sample is a lead, not a pattern, so this harness turns the
// question into a number: run a fixed corpus of transcripts through the REAL
// closing prompt and count how often the line opens in the wrong register.
//
// Every metric here is deterministic — no LLM judge. The defect is exactly
// string-detectable, so a judge would only add noise and cost. Judgment-shaped
// criteria (groundedness, specificity) belong in a separate ungated pass.
//
// Fidelity matters more than convenience: the harness drives ws.ClosingSystem,
// ws.ClosingUserPrompt, ws.CloseTranscript and ws.SanitizeClosing directly, and
// builds a real survey.Survey per case, so what is measured is what production
// does. Nothing about the prompt is re-typed here.
//
// With LANGFUSE_* credentials set it also publishes the run as a Langfuse dataset
// experiment, so a prompt change shows up as a before/after instead of a number
// that scrolls away. Without them it just prints.
//
// Usage:
//
//	go run ./cmd/evalclosing                        # local model, print only
//	./scripts/with-langfuse.sh go run ./cmd/evalclosing -run baseline
//	go run ./cmd/evalclosing -model claude-sonnet-5  # see the ceiling
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"voicesurvey/internal/llm"
	"voicesurvey/internal/obs"
	"voicesurvey/internal/survey"
	"voicesurvey/internal/ws"
)

// result is one case's generated closing plus its deterministic scores.
type result struct {
	c          closingCase
	transcript string
	line       string
	err        error
	rejected   bool // SanitizeClosing threw it out; production falls back to a fixed line

	// rawFiller is what the MODEL produced, before the guard. modelFiller and
	// filler are scored separately on purpose: once StripFillerOpener is in the
	// path, the final rate is guaranteed clean by construction, so it measures the
	// guard being wired in — not whether the prompt is working. The raw rate is the
	// only one that shows a prompt change moving the model.
	rawLine     string
	rawFiller   string
	filler      string // after the guard; "" if clean
	words       int
	question    bool
	copiedN     int
	copiedSpan  string
	unsupported []string // content words the transcript doesn't support (groundedness proxy)

	traceID string
}

func (r result) cleanOpener() bool      { return r.filler == "" }
func (r result) modelCleanOpener() bool { return r.rawFiller == "" }
func (r result) guardFired() bool       { return r.rawFiller != "" }
func (r result) withinWordCap() bool    { return r.words > 0 && r.words < 35 }

func main() {
	model := flag.String("model", "qwen2.5:3b", "model that writes the closing line (the server wires this from -classify-model)")
	conc := flag.Int("c", 4, "concurrent generations")
	runName := flag.String("run", "", "name for this run in Langfuse (default: <prompt-version>-<unix>)")
	dataset := flag.String("langfuse-dataset", "closing-line", "Langfuse dataset name")
	pepitaEnv := flag.String("pepita-env", llm.DefaultAnthropicEnvFile(), "file to read ANTHROPIC_API_KEY from for Anthropic models")
	flag.Parse()

	var key string
	if llm.IsAnthropicModel(*model) {
		k, err := llm.LoadAnthropicKey(*pepitaEnv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "model %q needs an Anthropic key: %v\n", *model, err)
			os.Exit(2)
		}
		key = k
	}
	closer, err := llm.NewCompleter(*model, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "completer: %v\n", err)
		os.Exit(2)
	}

	promptVer := ws.ClosingPromptVersion()
	run := strings.TrimSpace(*runName)
	if run == "" {
		run = fmt.Sprintf("%s-%d", promptVer, time.Now().Unix())
	}

	// Langfuse export is opt-in via credentials, exactly like cmd/eval.
	obsShutdown, obsOn, err := obs.Init(context.Background())
	if err != nil {
		fmt.Printf("(langfuse unavailable, continuing without it: %v)\n", err)
		obsOn = false
	}
	var lf *lfExport
	if obsOn {
		closer = obs.TraceCompleter(closer, *model)
		fmt.Printf("langfuse run %q -> %s\n", run, obs.Host())
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := obsShutdown(ctx); err != nil {
				fmt.Printf("(langfuse flush: %v)\n", err)
			}
		}()
		if c, ok := obs.NewClient(); ok {
			lf = &lfExport{c: c, dataset: *dataset, run: run, model: *model, promptVer: promptVer}
			if err := lf.pushCorpus(context.Background()); err != nil {
				fmt.Printf("(langfuse dataset push failed, export disabled: %v)\n", err)
				lf = nil
			}
		}
	}

	fmt.Printf("Closing-line eval — cases=%d model=%s closing-prompt=%s\n\n", len(corpus), *model, promptVer)

	results := generate(closer, *conc, run, promptVer)
	report(results)

	if lf != nil {
		if err := lf.pushRun(context.Background(), results); err != nil {
			fmt.Printf("(langfuse export degraded: %v)\n", err)
		}
	}
}

// generate runs every case through the real closing path.
func generate(closer llm.Completer, conc int, run, promptVer string) []result {
	out := make([]result, len(corpus))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := range corpus {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = generateOne(closer, corpus[i], run, promptVer)
		}(i)
	}
	wg.Wait()
	return out
}

func generateOne(closer llm.Completer, c closingCase, run, promptVer string) result {
	r := result{c: c}

	// Build a REAL survey and fill it, so CloseTranscript renders exactly what it
	// would in a live conversation rather than a hand-typed approximation.
	questions := make([]string, 0, len(c.qa))
	for _, p := range c.qa {
		questions = append(questions, p.q)
	}
	sv := survey.New(questions)
	for _, p := range c.qa {
		// CaptureAndAdvance rather than filling by index: this is the same call a
		// live conversation makes for each answered slot, so the survey state (and
		// therefore CloseTranscript's output) is produced exactly as in production.
		sv.CaptureAndAdvance(p.a)
	}
	r.transcript = ws.CloseTranscript(sv)
	if r.transcript == "" {
		r.err = fmt.Errorf("empty transcript — corpus case did not fill any slot")
		return r
	}

	ctx := obs.WithLabel(context.Background(), "closing_line")
	ctx = obs.WithPromptVersion(ctx, promptVer)
	ctx = obs.WithEvalCase(ctx, obs.EvalCase{Run: run, ExpectedIntent: "clean_opener"})
	var ref obs.TraceRef
	ctx = obs.WithTraceRef(ctx, &ref)
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	raw, err := closer.Complete(ctx, ws.ClosingSystem, ws.ClosingUserPrompt(qaProduct, r.transcript))
	r.traceID = ref.ID()
	if err != nil {
		r.err = err
		return r
	}
	// Sanitize, then measure BEFORE and AFTER the guard, mirroring production's
	// order exactly (personalClose does StripFillerOpener(SanitizeClosing(raw))).
	r.rawLine = ws.SanitizeClosing(raw)
	if r.rawLine == "" {
		r.rejected = true
		return r
	}
	r.rawFiller = FillerOpener(r.rawLine)
	r.line = ws.StripFillerOpener(r.rawLine)
	r.filler = FillerOpener(r.line)
	r.words = WordCount(r.line)
	r.question = HasQuestion(r.line)
	r.copiedN, r.copiedSpan = LongestCopiedSpan(r.line, c.answers())
	r.unsupported = UnsupportedWords(r.line, c.answers())
	return r
}

func report(rs []result) {
	var scored, clean, within, noQ, ticScored, ticClean int
	var modelClean, ticModelClean, guardFired, unsupportedCases int
	copiedTotal := 0
	for _, r := range rs {
		if r.err != nil {
			fmt.Printf("  %-19s ERROR %v\n", r.c.name, r.err)
			continue
		}
		if r.rejected {
			fmt.Printf("  %-19s REJECTED by SanitizeClosing (production would use the fixed line)\n", r.c.name)
			continue
		}
		scored++
		if r.cleanOpener() {
			clean++
		}
		if r.withinWordCap() {
			within++
		}
		if !r.question {
			noQ++
		}
		copiedTotal += r.copiedN
		if r.modelCleanOpener() {
			modelClean++
		}
		if r.guardFired() {
			guardFired++
		}
		if len(r.unsupported) > 0 {
			unsupportedCases++
		}
		if r.c.tic != "" {
			ticScored++
			if r.cleanOpener() {
				ticClean++
			}
			if r.modelCleanOpener() {
				ticModelClean++
			}
		}

		var flags []string
		if r.filler != "" {
			flags = append(flags, fmt.Sprintf("FILLER-OPENER SURVIVED %q", r.filler))
		}
		if r.guardFired() {
			flags = append(flags, fmt.Sprintf("guard stripped %q", r.rawFiller))
		}
		if !r.withinWordCap() {
			flags = append(flags, fmt.Sprintf("WORDS=%d", r.words))
		}
		if r.question {
			flags = append(flags, "QUESTION")
		}
		if r.copiedN >= 4 {
			flags = append(flags, fmt.Sprintf("copied %d: %q", r.copiedN, r.copiedSpan))
		}
		// Only surface a large cluster: the check has poor precision (see below), so
		// printing every hit buries the real signal in reaction adjectives.
		if len(r.unsupported) >= 5 {
			flags = append(flags, fmt.Sprintf("unsupported? %v", r.unsupported))
		}
		mark := "  "
		if len(flags) > 0 {
			mark = "!!"
		}
		fmt.Printf("%s%-19s %s\n", mark, r.c.name, r.line)
		if len(flags) > 0 {
			fmt.Printf("    -> %s\n", strings.Join(flags, "; "))
		}
	}

	fmt.Printf("\n=== deterministic results (%d scored) ===\n", scored)
	if scored == 0 {
		fmt.Println("  nothing scored")
		return
	}
	// Two rates, because they answer different questions. The model rate is the
	// honest measure of the PROMPT; the final rate is what the respondent actually
	// hears, and with the guard in the path it is clean by construction — so it
	// verifies the guard is wired in rather than proving the prompt improved.
	fmt.Printf("  MODEL clean opener  %3d/%-3d  %5.1f%%   <- prompt quality (pre-guard)\n", modelClean, scored, pct(modelClean, scored))
	if ticScored > 0 {
		fmt.Printf("    on tic cases      %3d/%-3d  %5.1f%%   <- where the defect lives\n", ticModelClean, ticScored, pct(ticModelClean, ticScored))
	}
	fmt.Printf("  FINAL clean opener  %3d/%-3d  %5.1f%%   <- what the respondent hears (post-guard)\n", clean, scored, pct(clean, scored))
	if ticScored > 0 {
		fmt.Printf("    on tic cases      %3d/%-3d  %5.1f%%\n", ticClean, ticScored, pct(ticClean, ticScored))
	}
	fmt.Printf("  guard fired on      %3d/%-3d  cases\n", guardFired, scored)
	fmt.Printf("  within 35 words     %3d/%-3d  %5.1f%%\n", within, scored, pct(within, scored))
	fmt.Printf("  no question mark    %3d/%-3d  %5.1f%%\n", noQ, scored, pct(noQ, scored))
	fmt.Printf("  mean copied span    %.1f words (informational)\n", float64(copiedTotal)/float64(scored))
	fmt.Printf("  unsupported-word cases %3d/%-3d      (LOW PRECISION — see EVAL.md; reaction words trip it)\n\n", unsupportedCases, scored)
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

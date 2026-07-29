package main

import (
	"context"
	"fmt"
	"time"

	"voicesurvey/internal/obs"
)

// lfExport publishes a closing-line run to Langfuse as a dataset experiment, the
// same shape cmd/eval uses for the classifier. The point is a before/after: run
// once, change ClosingSystem, run again, and the two runs sit side by side over
// identical transcripts with the prompt fingerprint distinguishing them.
type lfExport struct {
	c         *obs.Client
	dataset   string
	run       string
	model     string
	promptVer string
}

func itemID(c closingCase) string {
	// Hash the transcript content, so editing a case changes only that item.
	parts := []string{"closing-line", c.name}
	for _, p := range c.qa {
		parts = append(parts, p.q, p.a)
	}
	return obs.StableID(parts...)
}

// pushCorpus creates the dataset and upserts every transcript case.
func (e *lfExport) pushCorpus(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	if err := e.c.EnsureDataset(ctx, e.dataset,
		"Hand-written survey transcripts for measuring the agent's closing line (cmd/evalclosing/corpus.go). "+
			"Tic cases open every answer with a discourse marker; control cases don't."); err != nil {
		return err
	}
	for _, c := range corpus {
		input := map[string]any{"product": qaProduct, "exchanges": c.qa2maps()}
		// There is no single right closing line, so the expectation is the RULE the
		// line must satisfy rather than a target string.
		expected := map[string]any{
			"clean_opener":     true,
			"respondent_tic":   c.tic,
			"within_35_words":  true,
			"no_question_mark": true,
		}
		if err := e.c.UpsertItem(ctx, e.dataset, itemID(c), input, expected); err != nil {
			return err
		}
	}
	fmt.Printf("langfuse dataset %q: %d items pushed\n", e.dataset, len(corpus))
	return nil
}

// qa2maps renders the exchanges as plain maps so they survive JSON round-tripping
// into Langfuse's item input.
func (c closingCase) qa2maps() []map[string]string {
	out := make([]map[string]string, 0, len(c.qa))
	for _, p := range c.qa {
		out = append(out, map[string]string{"question": p.q, "answer": p.a})
	}
	return out
}

// pushRun links each generated line to its dataset item and attaches scores:
// per-case booleans on the trace, aggregates on the run.
func (e *lfExport) pushRun(ctx context.Context, rs []result) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// Traces must be ingested before run items can reference them.
	if err := obs.Flush(ctx); err != nil {
		return fmt.Errorf("flush traces: %w", err)
	}

	runName := fmt.Sprintf("%s@%s", e.model, e.run)
	desc := fmt.Sprintf("closing prompt %s, model %s", e.promptVer, e.model)
	meta := map[string]any{
		"model":                  e.model,
		"closing_prompt_version": e.promptVer,
		"cases":                  len(corpus),
	}

	var runID string
	linked := 0
	var scored, clean, modelClean, within, noQ int
	for _, r := range rs {
		if r.traceID == "" || r.err != nil {
			continue
		}
		id, err := e.c.AddRunItem(ctx, runName, itemID(r.c), r.traceID, desc, meta)
		if err != nil {
			return fmt.Errorf("run item for %q: %w", r.c.name, err)
		}
		runID = id
		linked++

		if r.rejected {
			// A rejected line is a real outcome, not a scoring gap: production would
			// speak the fixed close. Record it so the run's totals stay honest.
			if err := e.c.Score(ctx, obs.Score{
				ID: obs.StableID(runName, r.c.name, "sanitize_rejected"), Name: "sanitize_rejected",
				Value: 1, DataType: "BOOLEAN", TraceID: r.traceID,
				Comment: "SanitizeClosing threw the line out; the fixed close would be spoken",
			}); err != nil {
				return err
			}
			continue
		}

		scored++
		if r.cleanOpener() {
			clean++
		}
		if r.modelCleanOpener() {
			modelClean++
		}
		if r.withinWordCap() {
			within++
		}
		if !r.question {
			noQ++
		}

		per := []struct {
			name    string
			ok      bool
			comment string
		}{
			// model_clean_opener is the one that tracks the PROMPT. clean_opener is
			// post-guard and therefore clean by construction once the guard ships — it
			// verifies wiring, not prompt quality.
			{"model_clean_opener", r.modelCleanOpener(), fmt.Sprintf("model opened with %q", r.rawFiller)},
			{"clean_opener", r.cleanOpener(), fmt.Sprintf("final opened with %q", r.filler)},
			{"within_35_words", r.withinWordCap(), fmt.Sprintf("%d words", r.words)},
			{"no_question_mark", !r.question, ""},
		}
		for _, p := range per {
			v := 0.0
			if p.ok {
				v = 1
			}
			if err := e.c.Score(ctx, obs.Score{
				ID: obs.StableID(runName, r.c.name, p.name), Name: p.name,
				Value: v, DataType: "BOOLEAN", TraceID: r.traceID, Comment: p.comment,
			}); err != nil {
				return fmt.Errorf("case score %s: %w", p.name, err)
			}
		}
		// Informational, numeric: how much of the respondent's wording carried over.
		if err := e.c.Score(ctx, obs.Score{
			ID: obs.StableID(runName, r.c.name, "copied_span_words"), Name: "copied_span_words",
			Value: float64(r.copiedN), DataType: "NUMERIC", TraceID: r.traceID, Comment: r.copiedSpan,
		}); err != nil {
			return err
		}
	}
	if runID == "" {
		return fmt.Errorf("no traces linked (%d results had no trace id)", len(rs))
	}

	aggs := []struct {
		name  string
		value float64
	}{
		// The headline for a prompt change is model_clean_opener_rate; clean_opener_rate
		// is the post-guard result the respondent actually hears.
		{"model_clean_opener_rate", rate(modelClean, scored)},
		{"clean_opener_rate", rate(clean, scored)},
		{"within_word_cap_rate", rate(within, scored)},
		{"no_question_rate", rate(noQ, scored)},
	}
	for _, a := range aggs {
		if err := e.c.Score(ctx, obs.Score{
			ID: obs.StableID(runName, a.name), Name: a.name,
			Value: a.value, DataType: "NUMERIC", DatasetRunID: runID,
		}); err != nil {
			return fmt.Errorf("run score %s: %w", a.name, err)
		}
	}
	fmt.Printf("langfuse run %q: %d/%d cases linked, %d aggregate scores\n",
		runName, linked, len(rs), len(aggs))
	return nil
}

func rate(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

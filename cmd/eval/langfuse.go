package main

import (
	"context"
	"fmt"
	"time"

	"voicesurvey/internal/obs"
)

// lfExport publishes an eval run to Langfuse as a dataset experiment.
//
// Why bother, when the terminal already prints the numbers: the terminal output
// scrolls away and can't be compared. As a Langfuse experiment the same run
// becomes history — accuracy per prompt version over time, models side by side
// on identical items, and a click-through from any aggregate straight to the
// individual replies that failed.
//
// Shape of the export:
//   - the hand-labeled corpus is one DATASET (idempotent: items upsert by a
//     content-hash id, so pushing every run creates no duplicates);
//   - each model's pass is one RUN over that dataset, named "<model>@<run>";
//   - each case contributes a RUN ITEM linking the dataset item to the trace
//     that classification produced, plus a per-case correctness score;
//   - the gated + ungated metrics land as run-level scores.
type lfExport struct {
	c         *obs.Client
	dataset   string
	run       string
	promptVer string
}

// itemID is the stable dataset-item id for a case: a hash of the exact
// (question, reply), so the same case always maps to the same item and editing
// dataset.go only adds/changes the items that actually changed.
func itemID(c evalCase) string {
	return obs.StableID("turn-classifier", c.q, c.reply)
}

// pushCorpus creates the dataset and upserts every labeled case. Runs once per
// eval invocation, before any model executes.
func (e *lfExport) pushCorpus(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := e.c.EnsureDataset(ctx, e.dataset,
		"Hand-labeled per-turn intent + clarity corpus for the voice-survey classifier (cmd/eval/dataset.go)."); err != nil {
		return err
	}
	for _, c := range dataset {
		input := map[string]string{"question": c.q, "reply": c.reply}
		expected := map[string]string{"intent": string(c.want)}
		if c.clarity != "" {
			expected["clarity"] = string(c.clarity)
		}
		if err := e.c.UpsertItem(ctx, e.dataset, itemID(c), input, expected); err != nil {
			return err
		}
	}
	fmt.Printf("langfuse dataset %q: %d items pushed\n", e.dataset, len(dataset))
	return nil
}

// pushRun exports one model's results: a run item per case (linked to its
// trace), a per-case correctness score, and the run-level aggregates.
//
// Traces are force-flushed first — the run-item API rejects a trace it hasn't
// ingested, and OTLP export is asynchronous.
func (e *lfExport) pushRun(ctx context.Context, r report) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if err := obs.Flush(ctx); err != nil {
		return fmt.Errorf("flush traces: %w", err)
	}

	runName := fmt.Sprintf("%s@%s", r.model, e.run)
	desc := fmt.Sprintf("classifier prompt %s, model %s", e.promptVer, r.model)
	meta := map[string]any{
		"model":          r.model,
		"prompt_version": e.promptVer,
		"cases":          len(dataset),
		"elapsed_ms":     r.elapsed.Milliseconds(),
	}

	var runID string
	linked := 0
	for _, cr := range r.cases {
		if cr.traceID == "" {
			continue // untraced (e.g. the call errored before a span was recorded)
		}
		id, err := e.c.AddRunItem(ctx, runName, itemID(cr.c), cr.traceID, desc, meta)
		if err != nil {
			return fmt.Errorf("run item for %q: %w", cr.c.reply, err)
		}
		runID = id
		linked++

		// Per-case verdict, attached to that case's own trace. This is what makes
		// the run browsable: filter to intent_correct=0 and read only the misses.
		correct := 0.0
		if cr.err == nil && cr.got.Intent == cr.c.want {
			correct = 1
		}
		if err := e.c.Score(ctx, obs.Score{
			ID:       obs.StableID(runName, itemID(cr.c), "intent_correct"),
			Name:     "intent_correct",
			Value:    correct,
			DataType: "BOOLEAN",
			TraceID:  cr.traceID,
			Comment:  fmt.Sprintf("want %s, got %s", cr.c.want, cr.got.Intent),
		}); err != nil {
			return fmt.Errorf("case score: %w", err)
		}
	}
	if runID == "" {
		return fmt.Errorf("no traces linked (%d cases had no trace id)", len(r.cases))
	}

	// Run-level aggregates: the same numbers the terminal prints, but chartable
	// across runs. The two gated metrics first, then the informational ones.
	aggregates := []aggScore{
		{"intent_accuracy", r.acc(), true},
		{"answer_acceptance", r.ansRate(), true},
		{"clarity_accuracy", r.clarRate(), r.clarTotal > 0},
		{"ack_quality", r.ackRate(), r.ackTotal > 0},
	}
	pushed := 0
	for _, a := range aggregates {
		if !a.scored {
			continue // metric wasn't measured this run (e.g. no ack judge)
		}
		if err := e.c.Score(ctx, obs.Score{
			ID:           obs.StableID(runName, a.name),
			Name:         a.name,
			Value:        a.value,
			DataType:     "NUMERIC",
			DatasetRunID: runID,
		}); err != nil {
			return fmt.Errorf("run score %s: %w", a.name, err)
		}
		pushed++
	}
	fmt.Printf("langfuse run %q: %d/%d cases linked, %d aggregate scores\n",
		runName, linked, len(r.cases), pushed)
	return nil
}

// aggScore is one run-level metric. scored is false when the run didn't measure
// it, so it's skipped rather than exported as a misleading zero.
type aggScore struct {
	name   string
	value  float64
	scored bool
}

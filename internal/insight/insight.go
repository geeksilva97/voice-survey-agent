// Package insight computes LLM-based scorings about a completed (or partial)
// survey conversation. It is a fully INDEPENDENT pass, decoupled from the
// per-turn classifier in package survey/ws: it takes the whole transcript
// (product + every question/answer pair with its status) and reasons about it
// as a document — product sentiment, per-answer usefulness, per-answer
// confidence, and an overall summary.
//
// Scoring runs through llm.Completer (one-shot system+user), so it can be a
// local Ollama model (default qwen2.5:3b, works offline) or a hosted Anthropic
// model — the exact same routing the rest of the app uses. The package never
// imports session, so there is no import cycle: callers hand it plain inputs.
package insight

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"voicesurvey/internal/llm"
)

// flexInt/flexFloat tolerate weaker models that quote numbers as strings
// ("4") or emit floats where an int is expected ("4.0"). They fail soft to 0.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(string(b), `"`))
	if s == "" || s == "null" {
		return nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		*f = flexInt(int(v + 0.5))
	}
	return nil
}

type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(string(b), `"`))
	if s == "" || s == "null" {
		return nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		*f = flexFloat(v)
	}
	return nil
}

// QA is one question/answer pair from the transcript, plus the slot status
// (answered / skipped / asked / unasked). Status matters: a "skipped" slot or an
// empty answer should score low on usefulness regardless of wording.
type QA struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Status   string `json:"status"`
}

// Input is everything the scorer needs: the product under survey and the full
// ordered transcript.
type Input struct {
	Product   string
	EndReason string
	Answers   []QA
}

// AnswerScore is the per-answer scoring the model returns, merged back onto the
// question/answer text we already know (we never trust the model to echo text).
type AnswerScore struct {
	Question       string `json:"question"`
	Answer         string `json:"answer"`
	Status         string `json:"status"`
	Usefulness     int    `json:"usefulness"`      // 1 (useless) .. 5 (highly actionable)
	UsefulnessNote string `json:"usefulness_note"` // one short clause of rationale
	Confidence     int    `json:"confidence"`      // 1 (very hedged) .. 5 (committed/certain)
	ConfidenceNote string `json:"confidence_note"` // one short clause of rationale
}

// Result is the full scored insight for one response.
type Result struct {
	Product            string        `json:"product"`
	Sentiment          string        `json:"sentiment"`           // positive | mixed | negative
	SentimentRationale string        `json:"sentiment_rationale"` // one line
	Summary            string        `json:"summary"`             // 2-3 sentences
	Overall            float64       `json:"overall"`             // aggregate 1-5
	Answers            []AnswerScore `json:"answers"`
	Model              string        `json:"model"`
	GeneratedAt        time.Time     `json:"generated_at"`
}

// scored is the raw shape we ask the model for. Per-answer entries are keyed by
// the 1-based index "n" we hand it, so a dropped or reordered entry can't
// silently misalign onto the wrong question.
type scored struct {
	Sentiment          string    `json:"sentiment"`
	SentimentRationale string    `json:"sentiment_rationale"`
	Summary            string    `json:"summary"`
	Overall            flexFloat `json:"overall"`
	Answers            []struct {
		N              flexInt `json:"n"`
		Usefulness     flexInt `json:"usefulness"`
		UsefulnessNote string  `json:"usefulness_note"`
		Confidence     flexInt `json:"confidence"`
		ConfidenceNote string  `json:"confidence_note"`
	} `json:"answers"`
}

const system = "You are an analyst scoring a completed VOICE opinion survey. You are given a product " +
	"and a transcript of questions with the respondent's spoken answers (and each answer's status). " +
	"Reason about the WHOLE conversation, then score it. Return STRICT JSON only — no prose, no code fences.\n\n" +
	"Score these:\n" +
	"1. sentiment: the respondent's overall feeling about the PRODUCT — exactly one of \"positive\", \"mixed\", or \"negative\".\n" +
	"2. sentiment_rationale: one short sentence explaining that sentiment, grounded in what they said.\n" +
	"3. summary: 2-3 plain sentences summarizing this respondent's response overall.\n" +
	"4. overall: a single number 1-5 (one decimal ok) rating how useful/valuable this whole response is to the business.\n" +
	"5. answers: an array with one object PER numbered question, each with:\n" +
	"   - n: the question number exactly as given.\n" +
	"   - usefulness: 1-5. How useful/actionable is THIS answer? 'Nothing comes to mind', 'no', silence, or a skipped/empty " +
	"answer is 1. A vague-but-on-topic answer is 2-3. A specific, concrete suggestion or clear preference is 4-5.\n" +
	"   - usefulness_note: a short clause (<= 10 words) saying why.\n" +
	"   - confidence: 1-5. How confident/committed did the respondent sound, vs hedging/uncertain? Lots of 'maybe', 'I guess', " +
	"'I don't know', 'kind of' is 1-2. A firm, decisive statement is 4-5.\n" +
	"   - confidence_note: a short clause (<= 10 words) saying why.\n\n" +
	"Be honest and discriminating — do not give everything 5s. An empty or skipped answer must score 1 on usefulness."

// Score runs the independent scoring pass and returns a validated Result. On a
// bad or missing model response it FAILS SAFE: it returns a Result with neutral
// per-answer scores derived from the transcript so the page always renders,
// alongside the error so the caller can surface/log it.
func Score(ctx context.Context, c llm.Completer, model string, in Input) (Result, error) {
	res := baseline(model, in) // safe default we fill in / return on failure

	user := buildPrompt(in)
	raw, err := c.Complete(ctx, system, user)
	if err != nil {
		return res, fmt.Errorf("insight: completer failed: %w", err)
	}

	sc, err := parseScored(raw)
	if err != nil {
		return res, fmt.Errorf("insight: %w", err)
	}

	// Merge model scores onto the known transcript, keyed by 1-based index.
	byN := map[int]int{} // question number -> index into res.Answers
	for i := range res.Answers {
		byN[i+1] = i
	}
	for _, a := range sc.Answers {
		idx, ok := byN[int(a.N)]
		if !ok {
			continue
		}
		res.Answers[idx].Usefulness = clamp(int(a.Usefulness))
		res.Answers[idx].UsefulnessNote = strings.TrimSpace(a.UsefulnessNote)
		res.Answers[idx].Confidence = clamp(int(a.Confidence))
		res.Answers[idx].ConfidenceNote = strings.TrimSpace(a.ConfidenceNote)
	}
	// Empty/skipped answers are always usefulness 1, whatever the model said.
	for i := range res.Answers {
		if !answered(res.Answers[i]) {
			res.Answers[i].Usefulness = 1
			res.Answers[i].Confidence = 1
			if res.Answers[i].UsefulnessNote == "" {
				res.Answers[i].UsefulnessNote = "no answer given"
			}
		}
	}

	if s := normalizeSentiment(sc.Sentiment); s != "" {
		res.Sentiment = s
	}
	if r := strings.TrimSpace(sc.SentimentRationale); r != "" {
		res.SentimentRationale = r
	}
	if s := strings.TrimSpace(sc.Summary); s != "" {
		res.Summary = s
	}
	if o := float64(sc.Overall); o >= 1 && o <= 5 {
		res.Overall = round1(o)
	} else {
		res.Overall = round1(avgUsefulness(res.Answers))
	}
	res.GeneratedAt = time.Now()
	return res, nil
}

// buildPrompt renders the transcript as a numbered list the model can score.
func buildPrompt(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Product: %s\n", strings.TrimSpace(in.Product))
	if r := strings.TrimSpace(in.EndReason); r != "" {
		fmt.Fprintf(&b, "How the survey ended: %s\n", r)
	}
	b.WriteString("\nTranscript:\n")
	for i, qa := range in.Answers {
		ans := strings.TrimSpace(qa.Answer)
		if ans == "" {
			ans = "(no answer)"
		}
		fmt.Fprintf(&b, "%d. Q: %s\n   A: %s\n   (status: %s)\n", i+1, strings.TrimSpace(qa.Question), ans, qa.Status)
	}
	b.WriteString("\nReturn the JSON now.")
	return b.String()
}

// parseScored isolates the JSON object (models sometimes wrap it in prose or
// code fences) and unmarshals it.
func parseScored(raw string) (scored, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			s = s[i : j+1]
		}
	}
	var out scored
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return scored{}, fmt.Errorf("bad JSON from model: %w", err)
	}
	return out, nil
}

// baseline is the fail-safe Result: transcript echoed with neutral scores so the
// page always has something coherent to render even if the LLM misbehaves.
func baseline(model string, in Input) Result {
	answers := make([]AnswerScore, 0, len(in.Answers))
	for _, qa := range in.Answers {
		as := AnswerScore{
			Question:   qa.Question,
			Answer:     qa.Answer,
			Status:     qa.Status,
			Usefulness: 3,
			Confidence: 3,
		}
		if !answered(as) {
			as.Usefulness = 1
			as.Confidence = 1
			as.UsefulnessNote = "no answer given"
		}
		answers = append(answers, as)
	}
	return Result{
		Product:            in.Product,
		Sentiment:          "mixed",
		SentimentRationale: "Scoring unavailable; showing neutral defaults.",
		Summary:            "Automated scoring was unavailable for this response. The captured answers are shown below without LLM analysis.",
		Overall:            round1(avgUsefulness(answers)),
		Answers:            answers,
		Model:              model,
		GeneratedAt:        time.Now(),
	}
}

func answered(a AnswerScore) bool {
	return strings.TrimSpace(a.Answer) != "" && a.Status != "skipped" && a.Status != "unasked"
}

func normalizeSentiment(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "positive", "negative", "mixed":
		return strings.ToLower(strings.TrimSpace(s))
	case "neutral":
		return "mixed"
	}
	return ""
}

func clamp(n int) int {
	if n < 1 {
		return 1
	}
	if n > 5 {
		return 5
	}
	return n
}

func avgUsefulness(as []AnswerScore) float64 {
	if len(as) == 0 {
		return 0
	}
	sum := 0
	for _, a := range as {
		sum += a.Usefulness
	}
	return float64(sum) / float64(len(as))
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

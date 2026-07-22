package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ---- Meta navigation ("can we go back to the first question?") ----
//
// This is a TOP layer over the survey state, separate from per-turn intent
// classification. When a reply looks like the respondent wants to jump to a
// different question (revisit/re-answer one they missed), we ask a model — which
// sees the WHOLE question list and their current position — to decide whether it
// really is a navigation request and, if so, which question they mean. Keeping
// it out of the per-turn classifier avoids destabilizing that (gated) prompt and
// gives the resolver the full survey context it needs.

// NavQuestion is one slot as the resolver sees it.
type NavQuestion struct {
	Text    string
	Status  string // answered / skipped / asked / unasked
	Current bool   // the question they're on right now
}

// NavResult is the resolver's decision. Target is 1-based (matching the numbered
// list shown to the model); 0 means "no specific question". IsNav is false when
// the utterance is just a normal answer to the current question.
type NavResult struct {
	IsNav  bool `json:"is_nav"`
	Target int  `json:"target"`
}

const navSystem = "You watch a spoken survey for one thing only: is the respondent asking to GO TO a " +
	"DIFFERENT question — to revisit, re-answer, or jump back/forward to one — instead of answering the " +
	"current question? You are given the numbered questions (with status and which one is current) and " +
	"the respondent's latest utterance.\n" +
	"Decide:\n" +
	"- is_nav = true ONLY if they clearly want to move to another question (e.g. 'can we go back to the " +
	"first one?', 'I want to redo question two', 'let me answer the one I skipped', 'go back'). For 'go " +
	"back' with no number, target the question just before the current one.\n" +
	"- is_nav = false if they're answering the current question, chatting, or it's unclear. When unsure, " +
	"choose false — a normal answer must NEVER be treated as navigation.\n" +
	"- target = the 1-based number of the question they want (0 if is_nav is false or you can't tell which).\n" +
	`Respond ONLY as JSON: {"is_nav": true|false, "target": <number>}.`

// ResolveNavigation asks the completer whether the utterance is a request to
// jump to another question, and which one. Fails safe to "not navigation".
func ResolveNavigation(ctx context.Context, c Completer, utterance string, questions []NavQuestion) (NavResult, error) {
	var b strings.Builder
	b.WriteString("Questions:\n")
	for i, q := range questions {
		tag := q.Status
		if q.Current {
			tag = "current, " + tag
		}
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, tag, strings.TrimSpace(q.Text))
	}
	fmt.Fprintf(&b, "\nRespondent said: %q\n\nRespond with the JSON.", strings.TrimSpace(utterance))

	raw, err := c.Complete(ctx, navSystem, b.String())
	if err != nil {
		return NavResult{}, err
	}
	return parseNav(raw), nil
}

// parseNav isolates the first JSON object and reads {is_nav, target}. Tolerant
// of a bare number or stray text; anything unparseable is "not navigation".
func parseNav(raw string) NavResult {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		if j := strings.IndexByte(raw[i:], '}'); j >= 0 {
			raw = raw[i : i+j+1]
		}
	}
	var r struct {
		IsNav  bool            `json:"is_nav"`
		Target json.RawMessage `json:"target"`
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return NavResult{}
	}
	return NavResult{IsNav: r.IsNav, Target: parseIntLoose(string(r.Target))}
}

// parseIntLoose reads an int from a JSON number or quoted string ("1" or 1).
func parseIntLoose(s string) int {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), `"`))
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

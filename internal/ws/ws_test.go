package ws

import (
	"strings"
	"testing"

	"voicesurvey/internal/llm"
)

// TestIsAffirmation locks down the yes/no rule that decides, after a repair
// ("did I get that right?"), whether we keep the original answer (bare "yes") or
// treat the reply as a correction (anything substantive).
func TestIsAffirmation(t *testing.T) {
	yes := []string{
		"yes", "Yeah", "yep", "Right.", "correct", "Exactly!", "sure",
		"that's right", "that's it", "uh huh", "yes exactly",
		"Yeah, that's what I meant", "correct, that one",
	}
	no := []string{
		"", "no, I meant a banana smoothie", "actually the lavender one",
		"I said better chairs", "no not really",
		"I want to change my answer to vanilla",
	}
	for _, s := range yes {
		if !isAffirmation(s) {
			t.Errorf("isAffirmation(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isAffirmation(s) {
			t.Errorf("isAffirmation(%q) = true, want false", s)
		}
	}
}

// TestRepairPrompt confirms the repair echoes the respondent's own words and
// invites a correction (natural repair, not a verbatim re-ask).
func TestRepairPrompt(t *testing.T) {
	p := repairPrompt("  a banana vitamin  ")
	if want := "“a banana vitamin”"; !strings.Contains(p, want) {
		t.Errorf("repairPrompt should echo trimmed words %q; got %q", want, p)
	}
	if !strings.Contains(p, "another way") {
		t.Errorf("repairPrompt should invite a rephrase; got %q", p)
	}
}

// TestWithLead checks the acknowledgment lead-in is prepended (spaced) when
// present and is a no-op otherwise.
func TestWithLead(t *testing.T) {
	q := "What's your favorite scent?"
	if got := withLead("Comfier seating, got it.", q); got != "Comfier seating, got it. "+q {
		t.Errorf("withLead should prepend the ack; got %q", got)
	}
	if got := withLead("   ", q); got != q {
		t.Errorf("blank ack should be a no-op; got %q", got)
	}
}

// TestFollowUpPrompt: an off-topic aside leads with the classifier's ack and
// re-poses the question (warm steer-back), not a robotic "let me ask again".
func TestFollowUpPrompt(t *testing.T) {
	q := "How often do you burn candles at home?"

	got := followUpPrompt(llm.IntentOffTopic, "Ha, no worries —", q)
	if !strings.HasPrefix(got, "Ha, no worries —") || !strings.Contains(got, q) {
		t.Errorf("off-topic should lead with the ack and re-pose the question; got %q", got)
	}

	// No ack from the model → neutral redirect, still re-poses the question.
	got = followUpPrompt(llm.IntentOffTopic, "", q)
	if !strings.Contains(got, q) || strings.Contains(got, "let me ask again") {
		t.Errorf("off-topic fallback should re-pose the question without the robotic phrasing; got %q", got)
	}

	// A vague on-topic answer gets a gentle probe, not a re-read.
	if got := followUpPrompt(llm.IntentAnswer, "", q); strings.Contains(got, q) {
		t.Errorf("vague-answer probe should not re-read the question; got %q", got)
	}
}

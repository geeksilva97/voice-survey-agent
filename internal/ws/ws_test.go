package ws

import (
	"strings"
	"testing"

	"voicesurvey/internal/llm"
	"voicesurvey/internal/survey"
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

// TestIntroLine: the LLM-authored opening wins when present; otherwise a fixed,
// product-named greeting is used so the first turn always sounds complete.
func TestIntroLine(t *testing.T) {
	if got := introLine("  Hey there, just a few quick questions! Let's start:  ", "candles"); got != "Hey there, just a few quick questions! Let's start:" {
		t.Errorf("introLine should return the trimmed LLM intro; got %q", got)
	}
	fb := introLine("   ", "hand-poured soy candles")
	if !strings.Contains(fb, "hand-poured soy candles") || !strings.HasSuffix(fb, "first:") {
		t.Errorf("blank intro should fall back to a fixed product greeting ending in a hand-off; got %q", fb)
	}
}

// TestSanitizeClosing: a good one-liner is kept (quotes stripped, newlines
// collapsed); empty or oversized output is rejected so the fixed close is used.
func TestSanitizeClosing(t *testing.T) {
	if got := sanitizeClosing("  \"Loved that the lavender one is your\nevening ritual — take care!\"  "); got != "Loved that the lavender one is your evening ritual — take care!" {
		t.Errorf("sanitizeClosing should strip quotes and collapse newlines; got %q", got)
	}
	if got := sanitizeClosing("   "); got != "" {
		t.Errorf("empty closing should be rejected; got %q", got)
	}
	if got := sanitizeClosing(strings.Repeat("blah ", 100)); got != "" {
		t.Errorf("oversized closing should be rejected so the fixed line is used; got %q", got)
	}
}

// TestCloseTranscript: only actually-answered slots feed the personalized close,
// so it can never reference a skipped or unanswered question.
func TestCloseTranscript(t *testing.T) {
	sv := survey.New([]string{"Q1?", "Q2?", "Q3?"})
	sv.RecordAnswer("I love the lavender one")
	sv.RecordAnswer("about three times a week")
	sv.CaptureAndAdvance("") // Q3 skipped → must not appear

	got := closeTranscript(sv)
	if !strings.Contains(got, "Q1?") || !strings.Contains(got, "I love the lavender one") {
		t.Errorf("transcript should include answered slots; got %q", got)
	}
	if strings.Contains(got, "Q3?") {
		t.Errorf("transcript must omit skipped slots; got %q", got)
	}

	if got := closeTranscript(survey.New([]string{"Q1?"})); got != "" {
		t.Errorf("no answers → empty transcript (forces fixed close); got %q", got)
	}
}

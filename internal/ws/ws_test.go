package ws

import (
	"strings"
	"testing"
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

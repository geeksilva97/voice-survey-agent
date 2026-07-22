package ws

import (
	"strings"
	"testing"
	"time"

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

// TestSanitizeSpoken: a good line is kept (quotes stripped, newlines collapsed)
// and a question mark is tolerated (Ava may reciprocate); empty or oversized
// output is rejected so the fixed framing line is used instead.
func TestSanitizeSpoken(t *testing.T) {
	if got := sanitizeSpoken("  \"I'm great, thanks for asking!\nHere's the first one:\"  ", 260); got != "I'm great, thanks for asking! Here's the first one:" {
		t.Errorf("sanitizeSpoken should strip quotes and collapse newlines; got %q", got)
	}
	if got := sanitizeSpoken("Doing well — how about you? Let's dive in:", 260); !strings.Contains(got, "?") {
		t.Errorf("a reciprocating question mark should be tolerated; got %q", got)
	}
	if got := sanitizeSpoken("   ", 260); got != "" {
		t.Errorf("empty line should be rejected; got %q", got)
	}
	if got := sanitizeSpoken(strings.Repeat("blah ", 100), 260); got != "" {
		t.Errorf("oversized line should be rejected so the fixed framing is used; got %q", got)
	}
}

// TestGreetingReplySystem: the small-talk reply prompt must name the agent and
// product, state the question count and purpose, and forbid asking the survey
// question itself (it gets appended).
func TestGreetingReplySystem(t *testing.T) {
	p := greetingReplySystem("Ava", "hand-poured candles", "see how happy customers are", 3)
	if !strings.Contains(p, "Ava") || !strings.Contains(p, "hand-poured candles") {
		t.Errorf("prompt should name the agent and product; got %q", p)
	}
	if !strings.Contains(p, "three questions") {
		t.Errorf("prompt should state the question count in words; got %q", p)
	}
	if !strings.Contains(p, "see how happy customers are") {
		t.Errorf("prompt should state the survey purpose; got %q", p)
	}
	if !strings.Contains(strings.ToLower(p), "do not ask") {
		t.Errorf("prompt must forbid asking the survey question; got %q", p)
	}
	// Blank name/product/purpose and unknown count → safe generic phrasing.
	fb := greetingReplySystem("  ", "", "", 0)
	if !strings.Contains(fb, "Ava") || !strings.Contains(fb, "the product") || !strings.Contains(fb, "a few quick questions") {
		t.Errorf("blanks should default (Ava / the product / a few quick questions); got %q", fb)
	}
	if strings.Contains(fb, "it's to ") {
		t.Errorf("no purpose should omit the goal clause; got %q", fb)
	}
}

// TestFixedFraming: the deterministic fallback states the spelled-out count,
// the product, and (when given) the purpose, and ends by asking if they're ready.
func TestFixedFraming(t *testing.T) {
	got := fixedFraming("hand-poured candles", "measure satisfaction", 3)
	for _, want := range []string{"three quick questions", "about hand-poured candles", "to measure satisfaction", "Sound good?"} {
		if !strings.Contains(got, want) {
			t.Errorf("fixedFraming missing %q; got %q", want, got)
		}
	}
	// Single question → no plural "s"; no purpose → no goal clause.
	if got := fixedFraming("candles", "", 1); !strings.Contains(got, "one quick question ") || strings.Contains(got, ", to ") {
		t.Errorf("singular count / no purpose mishandled; got %q", got)
	}
	// Unknown count → generic phrasing.
	if got := fixedFraming("", "", 0); !strings.Contains(got, "a few quick questions") {
		t.Errorf("zero count should say 'a few quick questions'; got %q", got)
	}
}

// TestTimeOfDay: the clock is bucketed into morning/afternoon/evening.
func TestTimeOfDay(t *testing.T) {
	cases := map[int]string{6: "morning", 11: "morning", 12: "afternoon", 17: "afternoon", 18: "evening", 23: "evening"}
	for h, want := range cases {
		if got := timeOfDay(time.Date(2026, 7, 22, h, 0, 0, 0, time.UTC)); got != want {
			t.Errorf("timeOfDay(%02d:00) = %q, want %q", h, got, want)
		}
	}
}

// TestGreetingLine: every template introduces the agent by name and asks how
// they are, but names NO product or agenda (that comes after they reply). It
// weaves in the time of day. Blank name defaults to Ava.
func TestGreetingLine(t *testing.T) {
	for i := 0; i < 30; i++ {
		g := greetingLine("Ava", "morning")
		if !strings.Contains(g, "Ava") {
			t.Fatalf("greeting must name the agent; got %q", g)
		}
		if !strings.Contains(g, "?") {
			t.Fatalf("greeting must ask how they are; got %q", g)
		}
		if !strings.Contains(g, "morning") {
			t.Fatalf("greeting must weave in the time of day; got %q", g)
		}
		if strings.Contains(strings.ToLower(g), "question") || strings.Contains(strings.ToLower(g), "candles") {
			t.Fatalf("greeting must NOT mention the agenda or product; got %q", g)
		}
	}
	if g := greetingLine("  ", "evening"); !strings.Contains(g, "Ava") {
		t.Errorf("blank name should default to Ava; got %q", g)
	}
}

// TestSplitGreetingBeats: the authored greeting reply splits at the transition
// connective so the reaction is beat 1 and the framing + "ready?" is beat 2;
// un-splittable input returns ok=false (→ single beat).
func TestSplitGreetingBeats(t *testing.T) {
	r, f, ok := splitGreetingBeats("Glad to hear it, even with a busy morning! So I've got five questions about candles — ready to dive in?")
	if !ok || r != "Glad to hear it, even with a busy morning!" {
		t.Errorf("reaction beat wrong: ok=%v r=%q", ok, r)
	}
	if !strings.HasPrefix(f, "So I've got") || !strings.HasSuffix(f, "ready to dive in?") {
		t.Errorf("framing beat should carry the framing + ready-check; got %q", f)
	}

	// Two reaction sentences before the transition → both land in beat 1.
	r, f, ok = splitGreetingBeats("I'm doing well, thanks! Glad your day's good. Alright, I've got three questions — sound good?")
	if !ok || !strings.Contains(r, "thanks!") || !strings.Contains(r, "Glad your day's good.") || !strings.HasPrefix(f, "Alright") {
		t.Errorf("multi-sentence reaction mis-split: r=%q f=%q", r, f)
	}

	// No transition marker → first sentence is the reaction.
	if r, f, ok = splitGreetingBeats("Nice to hear. I've got a couple questions for you, ready?"); !ok || r != "Nice to hear." {
		t.Errorf("no-transition fallback should cut after first sentence; r=%q f=%q ok=%v", r, f, ok)
	}

	// Single sentence → can't split.
	if _, _, ok := splitGreetingBeats("Ready to dive in?"); ok {
		t.Errorf("single sentence should not split")
	}
}

// TestHelpPrompt: a needs-help turn leads with the classifier's reassurance (or
// a neutral fallback) and always re-poses the question so the respondent gets a
// real second shot at answering.
func TestHelpPrompt(t *testing.T) {
	q := "How would you rate the quality of our coffee?"

	got := helpPrompt("No need for a score — just your gut feeling.", q)
	if !strings.HasPrefix(got, "No need for a score") || !strings.Contains(got, q) {
		t.Errorf("help should lead with the reassurance and re-pose the question; got %q", got)
	}

	// No ack from the model → neutral reassurance, still re-poses the question.
	fb := helpPrompt("   ", q)
	if !strings.Contains(fb, q) || !strings.Contains(fb, "honest take") {
		t.Errorf("empty-ack fallback should reassure and re-pose the question; got %q", fb)
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

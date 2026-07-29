package ws

import "testing"

// FillerOpener is the single source of truth for the closing-line register check:
// StripFillerOpener repairs what it finds, and cmd/evalclosing scores with it. So
// its behaviour is pinned here in detail — a change to this table changes both the
// product's output and the number the fix is judged by.
//
// Fully offline: no model, no network, safe in the gate.
func TestFillerOpener(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		// Real qwen2.5:3b output from the measured baseline.
		{"Sure thing, thanks for sharing! More exotic scents sound exciting.", "sure thing"},
		{"To be fair, thanks for your honest feedback on the scent and price.", "to be fair"},
		{"You know, glad to hear it fits into your evening routine.", "you know"},
		{"Okay so, now I check whether they're soy first. Thanks!", "okay so"},

		{"Well, thanks so much for your time today.", "well"},
		{"Honestly, your feedback means a lot — take care!", "honestly"},

		// Longest match wins: "sure thing" must never be reported as "sure".
		{"Sure thing, take care.", "sure thing"},
		{"Sure, take care.", "sure"},

		// Stacked fillers — missed by an earlier single-phrase matcher, which is why
		// the first baseline run was thrown away.
		{"Well so, thanks for the detail there.", "well so"},
		{"Yeah so, that's really helpful — take care!", "yeah so"},

		// Clean openers from the same baseline: these must NOT be touched.
		{"Great choice on lavender – it really fits the room. Have a wonderful day!", ""},
		{"I love hearing how they look great on your shelf — thanks!", ""},
		{"Thanks, your preference for a warm scent like vanilla sounds perfect.", ""},
		{"Sounds like a nice cozy evening for some vanilla candles. Thanks!", ""},

		// The comma requirement — content words that merely START with a filler.
		{"Well-made candles are worth the wait — thanks!", ""},
		{"Rightly or wrongly, I think you'll love it.", ""},
		{"Actually helpful feedback like yours is rare — thank you.", ""},
		{"Surely, that's the best one — thanks!", ""},
		{"Wellness aside, thanks for your time.", ""},
		{"Okay so we agree the fig one wins — thanks!", ""},

		// Mid-sentence filler is not an opener.
		{"Thanks — honestly, that fig pick surprised me too.", ""},

		// Casing and whitespace tolerance.
		{"YOU KNOW, thanks a lot.", "you know"},
		{"   sure thing , thanks!", "sure thing"},

		{"", ""},
	}
	for _, c := range cases {
		if got := FillerOpener(c.line); got != c.want {
			t.Errorf("FillerOpener(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

// StripFillerOpener must remove the tic, re-capitalize, and leave everything else
// byte-identical — the rest of the line is the part worth keeping.
func TestStripFillerOpener(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		// The three measured failures.
		{
			"Sure thing, thanks for sharing! More exotic scents sound exciting to me too.",
			"Thanks for sharing! More exotic scents sound exciting to me too.",
		},
		{
			"To be fair, thanks for your honest feedback on both the scent and price.",
			"Thanks for your honest feedback on both the scent and price.",
		},
		{
			"You know, glad to hear it fits perfectly into your evening routine.",
			"Glad to hear it fits perfectly into your evening routine.",
		},
		// Stacked filler.
		{
			"Okay so, now I check whether they're soy first. Thanks!",
			"Now I check whether they're soy first. Thanks!",
		},
		// Already clean: untouched, byte for byte.
		{
			"Great choice on lavender – it really fits the room. Have a wonderful day!",
			"Great choice on lavender – it really fits the room. Have a wonderful day!",
		},
		{
			"Well-made candles are worth the wait — thanks!",
			"Well-made candles are worth the wait — thanks!",
		},
		// Extra spacing around the filler and comma.
		{"sure thing ,  thanks so much!", "Thanks so much!"},
		// Already-capitalized remainder stays as-is.
		{"Well, Lavender really is the one — take care.", "Lavender really is the one — take care."},
	}
	for _, c := range cases {
		if got := StripFillerOpener(c.line); got != c.want {
			t.Errorf("StripFillerOpener(%q)\n  got  %q\n  want %q", c.line, got, c.want)
		}
	}
}

// FAIL OPEN: a guard that can empty the agent's last words is worse than the tic
// it removes. If nothing survives stripping, the original line is kept.
func TestStripFillerOpenerFailsOpen(t *testing.T) {
	for _, line := range []string{"Sure thing,", "Well,   ", "Okay so,", ""} {
		if got := StripFillerOpener(line); got != line {
			t.Errorf("StripFillerOpener(%q) = %q, want the original back", line, got)
		}
	}
}

// The guard must be idempotent — running it twice changes nothing more, so it is
// safe wherever it lands in the pipeline.
func TestStripFillerOpenerIdempotent(t *testing.T) {
	once := StripFillerOpener("Sure thing, thanks for sharing!")
	if twice := StripFillerOpener(once); twice != once {
		t.Errorf("not idempotent: %q then %q", once, twice)
	}
}

// The prompt fingerprint must move when the prompt changes, since that is what
// splits a before/after in Langfuse. Only its shape can be asserted here; the
// value is expected to change with every prompt edit.
func TestClosingPromptVersion(t *testing.T) {
	v := ClosingPromptVersion()
	if v != ClosingPromptVersion() {
		t.Error("ClosingPromptVersion is not stable across calls")
	}
	if len(v) != 12 {
		t.Errorf("len = %d, want 12 (got %q)", len(v), v)
	}
}

// The rule the guard enforces must also be stated in the prompt: the guard is the
// backstop, not the only defence, and a stronger model should get it right unaided.
func TestClosingSystemForbidsFillerOpeners(t *testing.T) {
	for _, want := range []string{"OWN VOICE", "Sure thing,", "echoing"} {
		if !contains(ClosingSystem, want) {
			t.Errorf("ClosingSystem should mention %q so the model is told the rule, not just corrected after", want)
		}
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

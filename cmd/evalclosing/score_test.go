package main

import (
	"strings"
	"testing"

	"voicesurvey/internal/survey"
	"voicesurvey/internal/ws"
)

// These scorers define the number the eventual prompt/guard fix gets judged by,
// so they are pinned before the baseline is trusted. All offline — no Ollama, no
// network — which is what makes them safe to run anywhere.

func TestWordCount(t *testing.T) {
	cases := []struct {
		line string
		want int
	}{
		{"Thanks so much!", 3},
		// An em dash on its own is punctuation, not a word — it must not inflate the
		// count against the 35-word rule.
		{"Lavender — take care", 3},
		{"", 0},
		{"   ", 0},
		{"one 2 three", 3},
	}
	for _, c := range cases {
		if got := WordCount(c.line); got != c.want {
			t.Errorf("WordCount(%q) = %d, want %d", c.line, got, c.want)
		}
	}
}

func TestHasQuestion(t *testing.T) {
	if !HasQuestion("Anything else before we wrap up?") {
		t.Error("expected a question mark to be detected")
	}
	if HasQuestion("Thanks so much — take care!") {
		t.Error("did not expect a question")
	}
}

func TestLongestCopiedSpan(t *testing.T) {
	answers := []string{
		"I love a blend of coastal vibes with some tropical touches.",
		"More exotic scents would pique my interest.",
	}
	// A genuine paraphrase: words are reused but never three-plus consecutively.
	// This is the case I originally MIS-read as a verbatim rubric violation, so it
	// is pinned here as a paraphrase, not a copy.
	n, _ := LongestCopiedSpan("Your love for coastal and tropical blends came through — thanks!", answers)
	if n > 3 {
		t.Errorf("paraphrase scored as a %d-word copy; expected a short span", n)
	}
	// A real lift of a distinctive phrase.
	n, span := LongestCopiedSpan("Glad you love a blend of coastal vibes — take care!", answers)
	if n < 6 {
		t.Errorf("verbatim lift scored only %d words (span %q); expected >=6", n, span)
	}
	// Nothing in common.
	if n, _ := LongestCopiedSpan("Thanks for your time today!", answers); n > 1 {
		t.Errorf("unrelated line reported a %d-word overlap", n)
	}
	// Punctuation and casing must not defeat matching.
	if n, _ := LongestCopiedSpan("EXOTIC SCENTS WOULD PIQUE MY INTEREST!!!", answers); n < 5 {
		t.Errorf("case/punctuation defeated matching: got %d", n)
	}
}

// Every corpus case must fill at least one slot, or it silently contributes
// nothing to the baseline.
func TestCorpusWellFormed(t *testing.T) {
	if len(corpus) == 0 {
		t.Fatal("corpus is empty")
	}
	seen := map[string]bool{}
	tics, controls := 0, 0
	for _, c := range corpus {
		if seen[c.name] {
			t.Errorf("duplicate case name %q", c.name)
		}
		seen[c.name] = true
		if len(c.qa) == 0 {
			t.Errorf("case %q has no exchanges", c.name)
		}
		for _, p := range c.qa {
			if p.q == "" || p.a == "" {
				t.Errorf("case %q has an empty question or answer", c.name)
			}
		}
		if c.tic != "" {
			tics++
		} else {
			controls++
		}
	}
	// Both kinds must be present: tics measure the defect, controls catch a guard
	// that over-reaches and starts stripping legitimate openers.
	if tics == 0 || controls == 0 {
		t.Errorf("corpus needs both tic and control cases; got %d tic, %d control", tics, controls)
	}
}

// UnsupportedWords exists because a first attempt at fixing the filler defect
// added an example closing to the prompt and the 3B model copied it into every
// case — telling a respondent who said only "Vanilla." about lavender and
// bedrooms, while every other score sat at 100%. This pins the check that would
// have caught it.
func TestUnsupportedWords(t *testing.T) {
	answers := []string{"Vanilla.", "Yes, probably every couple of months."}

	// The actual regression output.
	got := UnsupportedWords("Lavender for the bedroom — lovely, I'd definitely buy it again.", answers)
	if len(got) == 0 {
		t.Fatal("expected lavender/bedroom to be flagged as unsupported")
	}
	found := map[string]bool{}
	for _, w := range got {
		found[w] = true
	}
	for _, w := range []string{"lavender", "bedroom"} {
		if !found[w] {
			t.Errorf("expected %q among unsupported, got %v", w, got)
		}
	}

	// A grounded line must come back clean: every content word is either the
	// respondent's or the agent's own register.
	if got := UnsupportedWords("Vanilla it is — thanks so much for your time!", answers); len(got) != 0 {
		t.Errorf("grounded line flagged %v", got)
	}
	// Simple plural morphology against the transcript is tolerated.
	if got := UnsupportedWords("Glad the months add up — take care!", answers); len(got) != 0 {
		t.Errorf("morphology tolerance failed: %v", got)
	}
}

// The harness builds a real survey.Survey and lets ws.CloseTranscript render it,
// so the closing prompt sees production's exact input. This pins that every corpus
// exchange actually reaches the transcript — a filling bug here would silently
// measure the model against a truncated conversation.
func TestCorpusRendersFullTranscript(t *testing.T) {
	for _, c := range corpus {
		questions := make([]string, 0, len(c.qa))
		for _, p := range c.qa {
			questions = append(questions, p.q)
		}
		sv := survey.New(questions)
		for _, p := range c.qa {
			sv.CaptureAndAdvance(p.a)
		}
		got := ws.CloseTranscript(sv)
		if got == "" {
			t.Errorf("case %q rendered an empty transcript", c.name)
			continue
		}
		for _, p := range c.qa {
			if !strings.Contains(got, p.q) {
				t.Errorf("case %q: transcript missing question %q", c.name, p.q)
			}
			if !strings.Contains(got, p.a) {
				t.Errorf("case %q: transcript missing answer %q", c.name, p.a)
			}
		}
	}
}

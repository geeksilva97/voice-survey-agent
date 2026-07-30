package main

import (
	"strings"
	"testing"

	"voicesurvey/internal/llm"
)

// TestNoLeakInCompiledPrompt is the offline half of the guard: it runs under
// `go test ./...` (and therefore validate.sh) with no credentials, so a leak
// introduced by editing the Go prompt fails the gate immediately. The runtime check
// in main covers the other half — an anchor authored in the Langfuse UI.
func TestNoLeakInCompiledPrompt(t *testing.T) {
	if ls := fewShotLeaks(dataset); len(ls) > 0 {
		t.Fatal("\n" + reportLeaks(ls))
	}
}

// TestShotExamplesParse guards the assumption the check rests on: if the anchors
// stopped parsing, fewShotLeaks would compare against nothing and pass forever.
func TestShotExamplesParse(t *testing.T) {
	anchors := llm.ClassifyShotExamples()
	if len(anchors) < 10 {
		t.Fatalf("parsed %d anchors from the classifier prompt, expected the full set — the leak check silently passes if this returns nothing", len(anchors))
	}
	for _, a := range anchors {
		if strings.TrimSpace(a[1]) == "" {
			t.Errorf("anchor with empty reply: %q", a)
		}
		if strings.Contains(a[1], "{{") {
			t.Errorf("the templated final turn must not be treated as an anchor: %q", a)
		}
	}
}

func TestFewShotLeaksDetects(t *testing.T) {

	// Anchors are taken from the LIVE prompt rather than hardcoded: rewriting an
	// anchor is exactly what the guard exists to support, and a fixture naming one
	// by hand would silently stop testing anything the next time one is reworded.
	anchors := llm.ClassifyShotExamples()
	if len(anchors) == 0 {
		t.Fatal("no anchors to build fixtures from")
	}
	// A long anchor, so the containment subtest has >= minContained characters.
	var long string
	for _, a := range anchors {
		if len(normalize(a[1])) > len(normalize(long)) {
			long = a[1]
		}
	}

	t.Run("exact match with punctuation and casing changed", func(t *testing.T) {
		shouted := strings.ToUpper(anchors[0][1]) + "!!"
		ls := fewShotLeaks([]evalCase{{reply: shouted, want: llm.IntentAnswer}})
		if len(ls) != 1 || !ls[0].exact {
			t.Errorf("normalization should see through case/punctuation; got %+v for %q", ls, shouted)
		}
	})

	t.Run("corpus reply that swallows an anchor", func(t *testing.T) {
		ls := fewShotLeaks([]evalCase{{reply: "Well, " + long + " That's my take.", want: llm.IntentAnswer}})
		if len(ls) == 0 {
			t.Errorf("a corpus reply that swallows the anchor %q is still a leak", long)
		}
	})

	t.Run("short shared phrase is not flagged", func(t *testing.T) {
		// "Vanilla, definitely." is in the corpus; nothing that short should trip
		// containment, or the check becomes noise people learn to ignore.
		if ls := fewShotLeaks([]evalCase{{reply: "Sure.", want: llm.IntentAnswer}}); len(ls) != 0 {
			t.Errorf("short replies must not trip containment; got %+v", ls)
		}
	})

	t.Run("novel sentence is clean", func(t *testing.T) {
		if ls := fewShotLeaks([]evalCase{{reply: "The wick tunnels down the middle after the second burn.", want: llm.IntentAnswer}}); len(ls) != 0 {
			t.Errorf("a novel sentence must not be flagged; got %+v", ls)
		}
	})

}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"(Buzzing) (buzzing)!":   "buzzing buzzing",
		"  Vanilla,  definitely": "vanilla definitely",
		"I'd say a solid eight.": "id say a solid eight",
		"!!!":                    "",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

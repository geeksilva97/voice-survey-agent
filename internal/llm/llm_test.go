package llm

import (
	"context"
	"net"
	"testing"
	"time"
)

// ollamaUp reports whether a local Ollama daemon is reachable; tests skip if not.
func ollamaUp() bool {
	c, err := net.DialTimeout("tcp", "localhost:11434", 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func TestClassifyTurn(t *testing.T) {
	if !ollamaUp() {
		t.Skip("ollama not running on :11434")
	}
	c, err := New("qwen2.5:3b")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const q = "What do you think of our scented candles?"

	cases := []struct {
		name       string
		reply      string
		wantIntent Intent
		wantSuff   bool
	}{
		{"wants_stop", "Honestly I have to go now, I don't have time for this.", IntentWantsStop, false},
		{"good_answer", "I really love the lavender one, it's so relaxing in the evenings.", IntentAnswer, true},
		{"repeat", "Sorry, what was the question? I didn't catch it.", IntentRepeat, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.ClassifyTurn(ctx, q, tc.reply)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			t.Logf("reply=%q -> %+v", tc.reply, got)
			if tc.wantIntent == IntentWantsStop && got.Intent != IntentWantsStop {
				t.Errorf("expected wants_stop, got %q", got.Intent)
			}
			if tc.wantIntent == IntentRepeat && got.Intent != IntentRepeat {
				t.Errorf("expected repeat, got %q", got.Intent)
			}
			if tc.wantIntent == IntentAnswer {
				if got.Intent != IntentAnswer {
					t.Errorf("expected answer intent, got %q", got.Intent)
				}
				if !got.Sufficient {
					t.Errorf("expected sufficient=true for a clear answer")
				}
			}
		})
	}
}

// TestClassifyQuirkyAnswer is a regression test: an unexpected-but-on-topic
// suggestion must be a sufficient answer, not off_topic. Real bug: "a banana
// vitamin would be awesome" for a drinks question was flagged off_topic, so the
// agent re-read an already-answered question.
func TestClassifyQuirkyAnswer(t *testing.T) {
	if !ollamaUp() {
		t.Skip("ollama not running on :11434")
	}
	c, err := New("qwen2.5:3b")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Each reply is genuinely ON-TOPIC for its paired question, just quirky/vague.
	cases := []struct{ q, reply string }{
		{"Is there a specific type of drink you would like us to offer more often?", "A banana vitamin would be awesome"},
		{"Do you think we should add more types of pastries to our menu?", "Of course, I think soft-drink pastries would be nice"},
		{"What's one thing you'd like to see improved at our coffee shop?", "I don't know, maybe better chairs, I'm not sure"},
	}
	for _, tc := range cases {
		t.Run(tc.reply, func(t *testing.T) {
			got, err := c.ClassifyTurn(ctx, tc.q, tc.reply)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			t.Logf("q=%q reply=%q -> %+v", tc.q, tc.reply, got)
			if got.Intent == IntentOffTopic || got.Intent == IntentUnintellig {
				t.Errorf("quirky on-topic reply misclassified as %q (want answer)", got.Intent)
			}
		})
	}
}

// TestIsNonSpeechArtifact locks the deterministic guard that stops a cough or
// other non-speech STT annotation from being treated as an answer (the agent
// must never say "Got it" and advance on a cough). It runs with no network.
func TestIsNonSpeechArtifact(t *testing.T) {
	artifacts := []string{"(coughing)", "  (coughing)  ", "(buzzing) (buzzing)",
		"[inaudible]", "(clears throat)", "(laughs)", "(background noise)", "(...)"}
	for _, s := range artifacts {
		if !IsNonSpeechArtifact(s) {
			t.Errorf("IsNonSpeechArtifact(%q) = false, want true", s)
		}
	}
	speech := []string{"", "I love the lavender one", "no idea (honestly)",
		"it freezes (sometimes) when I open it", "great", "(cough) but yeah it's good"}
	for _, s := range speech {
		if IsNonSpeechArtifact(s) {
			t.Errorf("IsNonSpeechArtifact(%q) = true, want false", s)
		}
	}
}

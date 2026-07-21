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

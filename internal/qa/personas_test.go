package qa

import (
	"strings"
	"testing"
)

// TestFind: every built-in persona is resolvable and has the fields the endpoint
// relies on; an unknown id is rejected.
func TestFind(t *testing.T) {
	for _, want := range []string{"enthusiast", "neutral", "rusher", "confused"} {
		p, ok := Find(want)
		if !ok {
			t.Fatalf("Find(%q) not found", want)
		}
		if p.System == "" || p.Name == "" {
			t.Errorf("persona %q missing System/Name", want)
		}
	}
	if _, ok := Find("nope"); ok {
		t.Errorf("unknown persona should not resolve")
	}
}

// TestReplyUser: the user prompt carries the question and the answered count so
// progress-dependent personas (the rusher) can decide when to bail.
func TestReplyUser(t *testing.T) {
	u := ReplyUser("How do you like the scent?", 2)
	if !strings.Contains(u, "How do you like the scent?") {
		t.Errorf("prompt should include the question; got %q", u)
	}
	if !strings.Contains(u, "2 survey answer") {
		t.Errorf("prompt should state the answered count; got %q", u)
	}
}

// TestPersonaIDs lists all built-ins.
func TestPersonaIDs(t *testing.T) {
	ids := PersonaIDs()
	for _, want := range []string{"enthusiast", "neutral", "rusher", "confused"} {
		if !strings.Contains(ids, want) {
			t.Errorf("PersonaIDs missing %q; got %q", want, ids)
		}
	}
}

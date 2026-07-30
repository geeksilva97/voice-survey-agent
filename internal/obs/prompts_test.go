package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"voicesurvey/internal/prompt"
)

// chatPayload renders a Langfuse GET /prompts response for a chat prompt.
func chatPayload(name string, version int, msgs ...prompt.Msg) string {
	b, _ := json.Marshal(map[string]any{
		"name": name, "version": version, "type": "chat", "prompt": msgs,
		"labels": []string{"production"},
	})
	return string(b)
}

func TestGetPromptNotFound(t *testing.T) {
	c, _ := fakeLangfuse(t, func(n int, path string) (int, string) {
		return http.StatusNotFound, `{"message":"not found"}`
	})
	_, found, err := c.GetPrompt(context.Background(), "voicesurvey/x", "production")
	if err != nil {
		t.Fatalf("a 404 is the normal first-run case, not an error: %v", err)
	}
	if found {
		t.Error("found should be false on 404")
	}
}

func TestGetPromptRejectsTextType(t *testing.T) {
	c, _ := fakeLangfuse(t, func(n int, path string) (int, string) {
		return http.StatusOK, `{"name":"x","version":1,"type":"text","prompt":"hello"}`
	})
	if _, _, err := c.GetPrompt(context.Background(), "x", ""); err == nil {
		t.Error("a text-type prompt must be rejected; this app only sends chat prompts")
	}
}

// TestEnsurePromptSkipsIdenticalContent is the whole reason EnsurePrompt reads
// before it writes: POST always mints a new version, so a blind push on every
// deploy would bury the real edits under identical ones.
func TestEnsurePromptSkipsIdenticalContent(t *testing.T) {
	msgs := []prompt.Msg{{Role: "system", Content: "Survey {{product}}."}}
	c, got := fakeLangfuse(t, func(n int, path string) (int, string) {
		return http.StatusOK, chatPayload("voicesurvey/t", 4, msgs...)
	})
	res, err := c.EnsurePrompt(context.Background(), prompt.Def{Name: "voicesurvey/t", Messages: msgs}, "production")
	if err != nil {
		t.Fatalf("EnsurePrompt: %v", err)
	}
	if res.Created || res.Version != 4 {
		t.Errorf("got v%d created=%v, want the existing v4 reused", res.Version, res.Created)
	}
	for _, r := range *got {
		if strings.HasPrefix(r.path, "/api/public/v2/prompts") && r.body != nil && len(r.body) > 0 {
			t.Errorf("no POST should have been sent; got body %v", r.body)
		}
	}
}

// TestEnsurePromptIgnoresWhitespaceDrift: the Langfuse editor is a text area, and a
// trailing newline someone's cursor left behind is not a prompt change.
func TestEnsurePromptIgnoresWhitespaceDrift(t *testing.T) {
	remote := []prompt.Msg{{Role: "system", Content: "Survey {{product}}.\n"}}
	local := []prompt.Msg{{Role: "system", Content: "Survey {{product}}."}}
	c, _ := fakeLangfuse(t, func(n int, path string) (int, string) {
		return http.StatusOK, chatPayload("voicesurvey/t", 2, remote...)
	})
	res, err := c.EnsurePrompt(context.Background(), prompt.Def{Name: "voicesurvey/t", Messages: local}, "")
	if err != nil {
		t.Fatalf("EnsurePrompt: %v", err)
	}
	if res.Created {
		t.Error("a whitespace-only difference should not mint a new version")
	}
}

func TestEnsurePromptCreatesOnDrift(t *testing.T) {
	remote := []prompt.Msg{{Role: "system", Content: "Old text {{product}}."}}
	local := []prompt.Msg{{Role: "system", Content: "New text {{product}}."}}
	c, got := fakeLangfuse(t, func(n int, path string) (int, string) {
		if n == 1 {
			return http.StatusOK, chatPayload("voicesurvey/t", 2, remote...)
		}
		return http.StatusOK, chatPayload("voicesurvey/t", 3, local...)
	})
	res, err := c.EnsurePrompt(context.Background(),
		prompt.Def{Name: "voicesurvey/t", Description: "why", Messages: local}, "production")
	if err != nil {
		t.Fatalf("EnsurePrompt: %v", err)
	}
	if !res.Created || res.Version != 3 {
		t.Errorf("got v%d created=%v, want a new v3", res.Version, res.Created)
	}
	last := (*got)[len(*got)-1]
	if last.body["type"] != "chat" {
		t.Errorf("payload type = %v, want chat", last.body["type"])
	}
	if last.body["commitMessage"] != "why" {
		t.Errorf("the Def description should ride along as the commit message; got %v", last.body["commitMessage"])
	}
	if labels, _ := last.body["labels"].([]any); len(labels) != 1 || labels[0] != "production" {
		t.Errorf("labels = %v, want [production] so the push is what production serves", last.body["labels"])
	}
}

// TestLoadPromptsFailsOnMissingPrompt: LoadPrompts must never fall back to the
// compiled-in default. Testing against the old prompt while believing you're
// testing the new one is the failure mode this whole mode exists to avoid.
func TestLoadPromptsFailsOnMissingPrompt(t *testing.T) {
	t.Cleanup(prompt.Reset)
	c, _ := fakeLangfuse(t, func(n int, path string) (int, string) {
		return http.StatusNotFound, `{"message":"not found"}`
	})
	err := LoadPrompts(context.Background(), c, "production")
	if err == nil {
		t.Fatal("LoadPrompts should fail when a declared prompt has no labeled version")
	}
	if !strings.Contains(err.Error(), "cmd/prompts push") {
		t.Errorf("the error should name the fix; got %v", err)
	}
}

func TestSameMessagesComparesRoles(t *testing.T) {
	a := []prompt.Msg{{Role: "system", Content: "x"}}
	if sameMessages(a, []prompt.Msg{{Role: "user", Content: "x"}}) {
		t.Error("same content under a different role is not the same prompt")
	}
	if sameMessages(a, append(a, prompt.Msg{Role: "user", Content: "y"})) {
		t.Error("differing lengths are not equal")
	}
}

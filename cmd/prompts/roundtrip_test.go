package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"voicesurvey/internal/obs"
	"voicesurvey/internal/prompt"
	"voicesurvey/internal/ws"
)

// fakePromptStore is a minimal stand-in for Langfuse Prompt Management: it keeps
// one current version per name and bumps the number on every POST, which is the
// server behavior EnsurePrompt's read-before-write exists to work with.
type fakePromptStore struct {
	cur map[string]struct {
		version int
		msgs    []prompt.Msg
	}
}

func newFakeLangfuse(t *testing.T) (*fakePromptStore, *obs.Client) {
	t.Helper()
	s := &fakePromptStore{cur: map[string]struct {
		version int
		msgs    []prompt.Msg
	}{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var body struct {
				Name   string       `json:"name"`
				Prompt []prompt.Msg `json:"prompt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			e := s.cur[body.Name]
			e.version++
			e.msgs = body.Prompt
			s.cur[body.Name] = e
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": body.Name, "version": e.version, "type": "chat", "prompt": e.msgs,
			})
			return
		}
		// r.URL.Path is the DECODED path, so the %2F the client sends for the
		// namespace separator arrives as a real slash here.
		name := strings.TrimPrefix(r.URL.Path, "/api/public/v2/prompts/")
		e, ok := s.cur[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": name, "version": e.version, "type": "chat", "prompt": e.msgs,
			"labels": []string{"production"},
		})
	}))
	t.Cleanup(srv.Close)

	t.Setenv("LANGFUSE_HOST", srv.URL)
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-lf-test")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-lf-test")
	c, ok := obs.NewClient()
	if !ok {
		t.Fatal("client should be enabled with test keys set")
	}
	return s, c
}

// TestRoundTrip walks the whole feature: push the binary's prompts, confirm a
// second push is a no-op, load them back as overrides, then edit one the way a
// human would in the UI and confirm the app picks the edit up.
func TestRoundTrip(t *testing.T) {
	t.Cleanup(prompt.Reset)
	store, c := newFakeLangfuse(t)
	ctx := context.Background()

	// 1. First push creates every prompt at v1.
	res, err := obs.PushPrompts(ctx, c, "production")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(res) != len(prompt.All()) {
		t.Fatalf("pushed %d prompts, want all %d", len(res), len(prompt.All()))
	}
	for _, r := range res {
		if !r.Created || r.Version != 1 {
			t.Errorf("%s: got v%d created=%v, want a new v1", r.Name, r.Version, r.Created)
		}
	}

	// 2. Pushing again must NOT mint v2 — the content did not move.
	res, err = obs.PushPrompts(ctx, c, "production")
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	for _, r := range res {
		if r.Created {
			t.Errorf("%s: second push created v%d; push must be idempotent by content", r.Name, r.Version)
		}
	}

	// 3. Loading installs them. The text came from code, so behavior is unchanged —
	// only the provenance differs, which is exactly what makes this mode safe to
	// switch into before anything has been edited.
	codeFP := ws.ClosingPrompt.Resolve().Fingerprint()
	if err := obs.LoadPrompts(ctx, c, "production"); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := ws.ClosingPrompt.Resolve()
	if got.Version != 1 || got.Source != string(prompt.ModeLangfuse) {
		t.Errorf("closing prompt = v%d/%s, want v1/langfuse", got.Version, got.Source)
	}
	if got.Fingerprint() != codeFP {
		t.Error("a round-trip of unedited text must not change the fingerprint")
	}

	// 4. Now the point of the whole exercise: someone edits the prompt in the UI.
	edited := []prompt.Msg{
		{Role: "system", Content: "Write a one-word farewell."},
		{Role: "user", Content: "Product: {{product}}\nSaid: {{transcript}}"},
	}
	store.cur["voicesurvey/closing-line"] = struct {
		version int
		msgs    []prompt.Msg
	}{version: 2, msgs: edited}

	if err := obs.LoadPrompts(ctx, c, "production"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	after := ws.ResolveClosing("candles", "loves the lavender one")
	if after.Version != 2 {
		t.Errorf("version = %d, want the edited v2", after.Version)
	}
	if after.System() != "Write a one-word farewell." {
		t.Errorf("System() = %q, want the edited text", after.System())
	}
	if !strings.Contains(after.LastUser(), "loves the lavender one") {
		t.Errorf("LastUser() = %q, should still interpolate the transcript", after.LastUser())
	}
	if after.Fingerprint() == codeFP {
		t.Error("an edited prompt must not keep the code fingerprint")
	}
}

// TestRoundTripRejectsEditThatDropsAVariable: the guard has to hold against a real
// fetch, not just a direct Install call — this is the failure a curious edit in the
// UI actually produces.
func TestRoundTripRejectsEditThatDropsAVariable(t *testing.T) {
	t.Cleanup(prompt.Reset)
	store, c := newFakeLangfuse(t)
	ctx := context.Background()
	if _, err := obs.PushPrompts(ctx, c, "production"); err != nil {
		t.Fatalf("push: %v", err)
	}
	// The transcript placeholder is gone: the model would be asked to reference
	// something specific the respondent said, with nothing to reference.
	store.cur["voicesurvey/closing-line"] = struct {
		version int
		msgs    []prompt.Msg
	}{version: 2, msgs: []prompt.Msg{
		{Role: "system", Content: "Say goodbye warmly."},
		{Role: "user", Content: "Product: {{product}}"},
	}}

	err := obs.LoadPrompts(ctx, c, "production")
	if err == nil {
		t.Fatal("LoadPrompts must reject an edit that dropped a required variable")
	}
	if !strings.Contains(err.Error(), "transcript") {
		t.Errorf("error should name the dropped variable; got %v", err)
	}
	if ws.ClosingPrompt.Resolve().Version != 0 {
		t.Error("the rejected prompt must not be installed")
	}
}

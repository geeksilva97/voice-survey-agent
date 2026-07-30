package prompt

import (
	"strings"
	"testing"
)

// testDef registers a throwaway prompt. Names are unique per test because the
// registry is process-global and Register panics on a duplicate.
func testDef(t *testing.T, name string) *Handle {
	t.Helper()
	t.Cleanup(Reset)
	return Register(Def{
		Name: name,
		Vars: []string{"product"},
		Messages: []Msg{
			{Role: "system", Content: "You survey {{product}}."},
			{Role: "user", Content: "Reply: {{reply}}"},
		},
	})
}

func TestCompileSubstitutesAndLeavesUnknownsVisible(t *testing.T) {
	h := testDef(t, "test/compile")
	r := h.Resolve().Compile(map[string]string{"product": "candles"})
	if got := r.System(); got != "You survey candles." {
		t.Errorf("System() = %q, want the product substituted", got)
	}
	// An unsupplied placeholder stays literal rather than becoming "" — that is
	// what makes Missing able to report it instead of it vanishing silently.
	if got := r.LastUser(); got != "Reply: {{reply}}" {
		t.Errorf("LastUser() = %q, want the unknown placeholder left intact", got)
	}
	if missing := r.Missing(); len(missing) != 1 || missing[0] != "reply" {
		t.Errorf("Missing() = %v, want [reply]", missing)
	}
}

func TestCompileToleratesSpacedPlaceholders(t *testing.T) {
	t.Cleanup(Reset)
	h := Register(Def{Name: "test/spaced", Messages: []Msg{{Role: "system", Content: "Hi {{ name }}!"}}})
	if got := h.Resolve().Compile(map[string]string{"name": "Ava"}).System(); got != "Hi Ava!" {
		t.Errorf("System() = %q; the Langfuse editor lets a human type spaces inside the braces", got)
	}
}

// TestFingerprint locks the two properties the fingerprint needs to be useful for
// attributing score changes to prompt edits: stable across calls, and different
// the moment the text moves. Length keeps it readable in a trace attribute.
func TestFingerprint(t *testing.T) {
	h := testDef(t, "test/fingerprint")
	got := h.Resolve().Fingerprint()
	if got != h.Resolve().Fingerprint() {
		t.Error("Fingerprint is not stable across calls")
	}
	if len(got) != 12 {
		t.Errorf("len(Fingerprint()) = %d, want 12 (got %q)", len(got), got)
	}
	edited := Resolved{Messages: []Msg{{Role: "system", Content: "You survey {{product}}!"}}}
	if edited.Fingerprint() == got {
		t.Error("a one-character prompt edit must change the fingerprint")
	}
}

func TestRoleSplitting(t *testing.T) {
	t.Cleanup(Reset)
	h := Register(Def{Name: "test/roles", Messages: []Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "shot-q"},
		{Role: "assistant", Content: "shot-a"},
		{Role: "user", Content: "real"},
	}})
	r := h.Resolve()
	if r.System() != "sys" {
		t.Errorf("System() = %q", r.System())
	}
	if r.LastUser() != "real" {
		t.Errorf("LastUser() = %q, want the FINAL user turn, not the few-shot one", r.LastUser())
	}
	if chat := r.Chat(); len(chat) != 3 || chat[0].Content != "shot-q" {
		t.Errorf("Chat() = %v, want the few-shots plus the final turn, no system", chat)
	}
}

func TestResolveDefaultsToCode(t *testing.T) {
	h := testDef(t, "test/source-code")
	r := h.Resolve()
	if r.Source != string(ModeCode) {
		t.Errorf("Source = %q, want %q", r.Source, ModeCode)
	}
	if r.Version != 0 {
		t.Errorf("Version = %d, want 0 for a compiled-in prompt", r.Version)
	}
}

func TestInstallReplacesActivePrompt(t *testing.T) {
	h := testDef(t, "test/install")
	err := Install("test/install", 7, []Msg{{Role: "system", Content: "Rewritten for {{product}}."}})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	r := h.Resolve()
	if r.Version != 7 || r.Source != string(ModeLangfuse) {
		t.Errorf("Resolve() = v%d/%s, want v7/langfuse", r.Version, r.Source)
	}
	if got := r.Compile(map[string]string{"product": "soap"}).System(); got != "Rewritten for soap." {
		t.Errorf("System() = %q, want the installed text", got)
	}
	Reset()
	if h.Resolve().Version != 0 {
		t.Error("Reset should restore the compiled-in default")
	}
}

// TestInstallRejectsMissingVariable is the safety net for -prompts=langfuse: an
// edit that drops a placeholder the code supplies would otherwise ask the model to
// reason about nothing, and the answer would look perfectly plausible.
func TestInstallRejectsMissingVariable(t *testing.T) {
	h := testDef(t, "test/install-missing")
	err := Install("test/install-missing", 3, []Msg{{Role: "system", Content: "You survey things."}})
	if err == nil {
		t.Fatal("Install should reject an override that dropped {{product}}")
	}
	if !strings.Contains(err.Error(), "product") {
		t.Errorf("error should name the missing variable; got %v", err)
	}
	if h.Resolve().Version != 0 {
		t.Error("a rejected override must leave the compiled-in default active")
	}
}

func TestInstallRejectsUnknownAndEmpty(t *testing.T) {
	testDef(t, "test/install-guards")
	if err := Install("test/never-declared", 1, []Msg{{Role: "system", Content: "x"}}); err == nil {
		t.Error("Install should reject a name this binary doesn't declare")
	}
	if err := Install("test/install-guards", 1, nil); err == nil {
		t.Error("Install should reject an empty message list")
	}
}

func TestParseMode(t *testing.T) {
	for _, in := range []string{"code", "CODE", " langfuse "} {
		if _, err := ParseMode(in); err != nil {
			t.Errorf("ParseMode(%q) unexpected error: %v", in, err)
		}
	}
	if _, err := ParseMode("s3"); err == nil {
		t.Error("ParseMode should reject an unknown mode")
	}
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	t.Cleanup(Reset)
	Register(Def{Name: "test/dup", Messages: []Msg{{Role: "system", Content: "a"}}})
	defer func() {
		if recover() == nil {
			t.Error("Register should panic on a duplicate name")
		}
	}()
	Register(Def{Name: "test/dup", Messages: []Msg{{Role: "system", Content: "b"}}})
}

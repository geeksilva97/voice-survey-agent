package main

import (
	"strings"
	"testing"

	"voicesurvey/internal/prompt"
)

// This is the only test binary that imports every package declaring a prompt, so
// it is the only place the FULL registry can be checked. Anything asserting
// "every prompt in the app ..." belongs here.

// TestEveryDeclaredVarAppearsInItsPrompt catches the typo that Install's guard
// cannot: a Def whose Vars say "goalLine" while the text says "{{goalline}}". In
// code mode that ships a prompt with a literal placeholder in it; in langfuse mode
// the pushed prompt inherits the same mistake.
func TestEveryDeclaredVarAppearsInItsPrompt(t *testing.T) {
	for _, d := range prompt.All() {
		joined := ""
		for _, m := range d.Messages {
			joined += m.Content + "\n"
		}
		for _, v := range d.Vars {
			if !strings.Contains(joined, "{{"+v+"}}") {
				t.Errorf("%s declares var %q but its text never uses {{%s}}", d.Name, v, v)
			}
		}
	}
}

// TestNoUndeclaredPlaceholders is the mirror check: a {{placeholder}} in the text
// that no Var declares is one nothing will ever fill, so it reaches the model
// verbatim. Every placeholder must be accounted for.
func TestNoUndeclaredPlaceholders(t *testing.T) {
	for _, d := range prompt.All() {
		declared := map[string]bool{}
		for _, v := range d.Vars {
			declared[v] = true
		}
		for _, v := range (prompt.Resolved{Messages: d.Messages}).Missing() {
			if !declared[v] {
				t.Errorf("%s uses {{%s}} but does not declare it in Vars — nothing will substitute it", d.Name, v)
			}
		}
	}
}

// TestPromptNamesAreNamespaced keeps the Langfuse prompt list readable and makes
// it obvious which project a prompt belongs to.
func TestPromptNamesAreNamespaced(t *testing.T) {
	for _, d := range prompt.All() {
		if !strings.HasPrefix(d.Name, "voicesurvey/") {
			t.Errorf("prompt %q should be namespaced voicesurvey/...", d.Name)
		}
		if d.Description == "" {
			t.Errorf("prompt %q has no Description — it is pushed as the commit message, so the Langfuse history would be anonymous", d.Name)
		}
	}
}

// TestRegistryCountMatchesDefaultExpect keeps the -expect default honest: it is
// the guard against a new prompt whose package cmd/prompts forgot to import, and a
// stale number would make it useless.
func TestRegistryCountMatchesDefaultExpect(t *testing.T) {
	if got := len(prompt.All()); got != 5 {
		t.Errorf("registry holds %d prompts but the -expect default is 5 — update both together", got)
	}
}

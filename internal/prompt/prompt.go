// Package prompt is the seam between the prompts compiled into the binary and
// the prompts managed in Langfuse. Every LLM instruction in the app is declared
// here as a Def with a stable name, a default message list, and the set of
// variables its text MUST contain; call sites then resolve the ACTIVE version at
// use time instead of referencing a Go constant directly.
//
// Two modes, chosen once at startup (see Mode / SetMode):
//
//	ModeCode      the compiled-in defaults win. Fully offline, and what the
//	              evals and ./scripts/validate.sh exercise. The default.
//	ModeLangfuse  the defaults are replaced by whatever Langfuse serves for the
//	              configured label, installed once at boot by obs.LoadPrompts.
//	              For experimenting: edit a prompt in the UI, restart, re-run.
//
// Resolution happens in memory and never hits the network — an override is
// fetched exactly once at startup, so a prompt edit can never add latency to a
// live voice turn or change behavior halfway through a conversation.
//
// This package is a LEAF: it imports nothing from the rest of the app. That is
// what lets llm, ws and insight all declare their prompts here while obs (which
// imports llm) installs the overrides.
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Mode selects where an active prompt's text comes from.
type Mode string

const (
	// ModeCode serves the compiled-in defaults. Offline, reproducible, default.
	ModeCode Mode = "code"
	// ModeLangfuse serves what Langfuse returned for the configured label.
	ModeLangfuse Mode = "langfuse"
)

// ParseMode validates a -prompts flag value.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.TrimSpace(strings.ToLower(s))) {
	case ModeCode:
		return ModeCode, nil
	case ModeLangfuse:
		return ModeLangfuse, nil
	}
	return "", fmt.Errorf("unknown prompt mode %q (want %q or %q)", s, ModeCode, ModeLangfuse)
}

// Msg is one chat message. Deliberately a copy of llm.Msg rather than a reuse:
// llm imports this package, so the dependency cannot run the other way.
type Msg struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// Def declares one prompt as it ships in the binary.
type Def struct {
	// Name is the Langfuse prompt name. Namespaced so a project's prompt list
	// stays readable next to other apps.
	Name string
	// Description explains what the prompt is for; pushed to Langfuse as the
	// commit message on the first version so the UI isn't a wall of anonymous text.
	Description string
	// Messages is the default chat prompt: system first, then any few-shot
	// exchanges, then the templated final user turn.
	Messages []Msg
	// Vars lists the {{placeholders}} the prompt cannot work without. An override
	// missing any of them is REJECTED at load: a Langfuse edit that accidentally
	// drops {{transcript}} would otherwise silently ask the model to summarize
	// nothing, and the output would look plausible.
	Vars []string
	// Config is optional metadata pushed alongside the prompt (model, temperature).
	// It is informational only — the app's own flags still decide what runs.
	Config map[string]any
}

// Resolved is an active prompt: the messages to send, plus the provenance needed
// to attribute an output to the exact instructions that produced it.
type Resolved struct {
	Name string
	// Version is the Langfuse version number, or 0 when served from code.
	Version int
	// Source is "code" or "langfuse".
	Source   string
	Messages []Msg
}

// Fingerprint is a short content hash of the resolved messages — the same device
// as before this package existed, and still the only identity that works in
// ModeCode (where there is no version number). It changes exactly when the text
// changes, so a score movement can be attributed to a prompt edit rather than to
// noise, and it cannot drift out of sync the way a hand-bumped number would.
func (r Resolved) Fingerprint() string {
	h := sha256.New()
	for _, m := range r.Messages {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// varRe matches a {{name}} placeholder, the templating syntax Langfuse uses.
var varRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

// Compile substitutes {{vars}} throughout the messages and returns the result.
// Placeholders with no matching key are left untouched; use Missing to detect
// them rather than shipping a prompt with a literal "{{product}}" in it.
func (r Resolved) Compile(vars map[string]string) Resolved {
	out := r
	out.Messages = make([]Msg, len(r.Messages))
	for i, m := range r.Messages {
		m.Content = varRe.ReplaceAllStringFunc(m.Content, func(match string) string {
			key := varRe.FindStringSubmatch(match)[1]
			if v, ok := vars[key]; ok {
				return v
			}
			return match
		})
		out.Messages[i] = m
	}
	return out
}

// Missing lists placeholders still present after Compile — a prompt authored in
// Langfuse referencing a variable the code doesn't supply.
func (r Resolved) Missing() []string {
	seen := map[string]bool{}
	for _, m := range r.Messages {
		for _, hit := range varRe.FindAllStringSubmatch(m.Content, -1) {
			seen[hit[1]] = true
		}
	}
	return sortedKeys(seen)
}

// System joins every system message, in order. Most call sites use the app's
// Completer interface, which takes a (system, user) pair rather than a list.
func (r Resolved) System() string {
	var parts []string
	for _, m := range r.Messages {
		if m.Role == "system" {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

// LastUser returns the final user message — the templated turn a Completer sends
// as its user prompt.
func (r Resolved) LastUser() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			return r.Messages[i].Content
		}
	}
	return ""
}

// Chat returns every message except the leading system block — the few-shot
// exchanges plus the final user turn, which is what the classifier sends.
func (r Resolved) Chat() []Msg {
	out := make([]Msg, 0, len(r.Messages))
	for _, m := range r.Messages {
		if m.Role != "system" {
			out = append(out, m)
		}
	}
	return out
}

// Handle is a registered prompt. Declare one per prompt as a package-level var
// and call Resolve at use time — never capture the messages at init, or a
// Langfuse override installed at boot would be invisible.
type Handle struct{ name string }

// Name is the registered prompt name.
func (h *Handle) Name() string { return h.name }

// registry holds every declared prompt plus any installed overrides.
var registry = struct {
	sync.RWMutex
	mode      Mode
	defs      map[string]Def
	order     []string
	overrides map[string]Resolved
}{
	mode:      ModeCode,
	defs:      map[string]Def{},
	overrides: map[string]Resolved{},
}

// Register declares a prompt and returns its handle. Panics on a duplicate name
// or an empty message list: both are programming errors that would otherwise
// surface as a mysteriously empty prompt at runtime.
func Register(d Def) *Handle {
	registry.Lock()
	defer registry.Unlock()
	if d.Name == "" {
		panic("prompt.Register: empty name")
	}
	if len(d.Messages) == 0 {
		panic("prompt.Register: " + d.Name + " has no messages")
	}
	if _, dup := registry.defs[d.Name]; dup {
		panic("prompt.Register: duplicate name " + d.Name)
	}
	registry.defs[d.Name] = d
	registry.order = append(registry.order, d.Name)
	return &Handle{name: d.Name}
}

// Resolve returns the active prompt: the Langfuse override when one is installed
// for this name, otherwise the compiled-in default.
func (h *Handle) Resolve() Resolved {
	registry.RLock()
	defer registry.RUnlock()
	if r, ok := registry.overrides[h.name]; ok {
		return r
	}
	d := registry.defs[h.name]
	return Resolved{Name: d.Name, Source: string(ModeCode), Messages: d.Messages}
}

// Default returns the compiled-in declaration, ignoring any override. This is
// what gets pushed to Langfuse — the code is the source of truth for a push.
func (h *Handle) Default() Def {
	registry.RLock()
	defer registry.RUnlock()
	return registry.defs[h.name]
}

// All returns every declared prompt in registration order. Used by the push
// command; requires that every package declaring a prompt has been imported.
func All() []Def {
	registry.RLock()
	defer registry.RUnlock()
	out := make([]Def, 0, len(registry.order))
	for _, n := range registry.order {
		out = append(out, registry.defs[n])
	}
	return out
}

// ActiveMode reports where active prompts are currently coming from.
func ActiveMode() Mode {
	registry.RLock()
	defer registry.RUnlock()
	return registry.mode
}

// SetMode records the configured mode. It does not itself fetch anything —
// obs.LoadPrompts installs the overrides — but it makes the intent inspectable,
// and Installed() checks against it.
func SetMode(m Mode) {
	registry.Lock()
	defer registry.Unlock()
	registry.mode = m
}

// Install replaces a prompt's text with a version fetched from Langfuse, after
// checking the override still declares every variable the code will supply.
// Rejecting here (rather than warning later) is the whole safety story for
// ModeLangfuse: a prompt that lost a placeholder must stop the boot, not quietly
// produce confident nonsense on a live call.
func Install(name string, version int, msgs []Msg) error {
	registry.Lock()
	defer registry.Unlock()
	d, ok := registry.defs[name]
	if !ok {
		return fmt.Errorf("prompt %q is not declared in this binary", name)
	}
	if len(msgs) == 0 {
		return fmt.Errorf("prompt %q: Langfuse returned no messages", name)
	}
	joined := ""
	for _, m := range msgs {
		joined += m.Content + "\n"
	}
	var missing []string
	for _, v := range d.Vars {
		if !strings.Contains(joined, "{{"+v+"}}") {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("prompt %q v%d is missing required variable(s) %s — the code supplies them and the prompt would ignore the input; fix the prompt in Langfuse or push the code default",
			name, version, strings.Join(missing, ", "))
	}
	registry.overrides[name] = Resolved{
		Name:     name,
		Version:  version,
		Source:   string(ModeLangfuse),
		Messages: msgs,
	}
	return nil
}

// Installed lists the names that currently resolve to a Langfuse version, for
// the startup log line.
func Installed() []string {
	registry.RLock()
	defer registry.RUnlock()
	seen := map[string]bool{}
	for n := range registry.overrides {
		seen[n] = true
	}
	return sortedKeys(seen)
}

// Reset clears every installed override, restoring the compiled-in defaults.
// For tests.
func Reset() {
	registry.Lock()
	defer registry.Unlock()
	registry.overrides = map[string]Resolved{}
	registry.mode = ModeCode
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

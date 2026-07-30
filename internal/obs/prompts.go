// Langfuse Prompt Management: the push half (register the binary's prompts as
// versions) and the fetch half (serve a label's version back into the app).
// Same rationale as langfuse.go — raw net/http, no Go SDK exists.
//
// Endpoint semantics that shape this code:
//   - POST /api/public/v2/prompts creates a NEW VERSION on every call. Unlike
//     datasets it does not upsert, so a blind push on each run would pile up
//     identical versions and make the history useless. EnsurePrompt therefore
//     reads the label's current version first and posts only on real drift.
//   - GET /api/public/v2/prompts/{name}?label=production resolves a label to one
//     immutable version. Labels are the deploy mechanism: move "production" to
//     roll forward or back without touching the prompt text.
package obs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"voicesurvey/internal/prompt"
)

// DefaultPromptLabel is the label both halves use unless told otherwise. It is
// Langfuse's own convention for "what production should be running".
const DefaultPromptLabel = "production"

// promptResp is the subset of the Prompt object we consume.
type promptResp struct {
	Name    string       `json:"name"`
	Version int          `json:"version"`
	Type    string       `json:"type"`
	Prompt  []prompt.Msg `json:"prompt"`
	Labels  []string     `json:"labels"`
}

// GetPrompt resolves a label (or "" for DefaultPromptLabel) to one version.
// found is false when Langfuse has no such prompt yet, which is the normal
// first-run case for a push and a hard error for a fetch — the caller decides.
func (c *Client) GetPrompt(ctx context.Context, name, label string) (p promptResp, found bool, err error) {
	if label == "" {
		label = DefaultPromptLabel
	}
	path := fmt.Sprintf("/api/public/v2/prompts/%s?label=%s", url.PathEscape(name), url.QueryEscape(label))
	status, err := c.get(ctx, path, &p)
	if err != nil {
		if status == http.StatusNotFound {
			return promptResp{}, false, nil
		}
		return promptResp{}, false, err
	}
	if p.Type != "" && p.Type != "chat" {
		return promptResp{}, false, fmt.Errorf("prompt %q is type %q; this app only uses chat prompts", name, p.Type)
	}
	return p, true, nil
}

// PushResult reports what one EnsurePrompt call did, so the command can print a
// meaningful summary instead of "done".
type PushResult struct {
	Name    string
	Version int
	// Created is false when the label's current version already matched the
	// code — no new version was minted.
	Created bool
}

// EnsurePrompt makes the code's version of a prompt the one the label points at,
// creating a new version only when the text actually differs. Idempotent by
// content, which is what makes it safe to run on every deploy.
func (c *Client) EnsurePrompt(ctx context.Context, d prompt.Def, label string) (PushResult, error) {
	if label == "" {
		label = DefaultPromptLabel
	}
	cur, found, err := c.GetPrompt(ctx, d.Name, label)
	if err != nil {
		return PushResult{}, err
	}
	if found && sameMessages(cur.Prompt, d.Messages) {
		return PushResult{Name: d.Name, Version: cur.Version, Created: false}, nil
	}

	body := map[string]any{
		"type":   "chat",
		"name":   d.Name,
		"prompt": d.Messages,
		"labels": []string{label},
	}
	if d.Description != "" {
		body["commitMessage"] = d.Description
	}
	if len(d.Config) > 0 {
		body["config"] = d.Config
	}
	var out promptResp
	if _, err := c.post(ctx, "/api/public/v2/prompts", body, &out); err != nil {
		return PushResult{}, err
	}
	return PushResult{Name: d.Name, Version: out.Version, Created: true}, nil
}

// PushPrompts pushes every prompt declared in this binary. Requires that each
// package declaring one has been imported by the caller.
func PushPrompts(ctx context.Context, c *Client, label string) ([]PushResult, error) {
	var out []PushResult
	for _, d := range prompt.All() {
		r, err := c.EnsurePrompt(ctx, d, label)
		if err != nil {
			return out, fmt.Errorf("push %s: %w", d.Name, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// LoadPrompts fetches every declared prompt at the given label and installs it as
// the active version, replacing the compiled-in default.
//
// It fails on the FIRST problem rather than falling back to the code default,
// and deliberately so: the point of this mode is to experiment with what Langfuse
// serves, and a silent fallback would mean testing the old prompt while believing
// you were testing the new one. Missing prompt, unreachable API, or an override
// that dropped a required variable all stop the boot with a message naming the fix.
func LoadPrompts(ctx context.Context, c *Client, label string) error {
	if label == "" {
		label = DefaultPromptLabel
	}
	for _, d := range prompt.All() {
		p, found, err := c.GetPrompt(ctx, d.Name, label)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", d.Name, err)
		}
		if !found {
			return fmt.Errorf("prompt %q has no version labeled %q in Langfuse — run `go run ./cmd/prompts push` first", d.Name, label)
		}
		if err := prompt.Install(d.Name, p.Version, p.Prompt); err != nil {
			return err
		}
	}
	return nil
}

// SetupPrompts applies a -prompts flag value and returns a log-ready description
// of what the process will run on.
//
// Shared by cmd/server, cmd/eval and cmd/evalclosing so all three interpret the
// flag identically. That matters more than the saved lines: an eval that silently
// scored the compiled-in prompt while the server ran the Langfuse one would produce
// numbers for a prompt nobody is using.
func SetupPrompts(ctx context.Context, mode, label string) (string, error) {
	m, err := prompt.ParseMode(mode)
	if err != nil {
		return "", err
	}
	prompt.SetMode(m)
	if m == prompt.ModeCode {
		return fmt.Sprintf("compiled-in defaults (%d registered)", len(prompt.All())), nil
	}
	c, ok := NewClient()
	if !ok {
		return "", fmt.Errorf("-prompts=langfuse needs LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY")
	}
	if label == "" {
		label = DefaultPromptLabel
	}
	if err := LoadPrompts(ctx, c, label); err != nil {
		return "", err
	}
	return fmt.Sprintf("langfuse label %q -> %s", label, strings.Join(prompt.Installed(), ", ")), nil
}

// sameMessages compares role+content pairwise. Whitespace is trimmed on the
// content because the Langfuse editor is a text area and a stray trailing
// newline is not a prompt change worth a new version.
func sameMessages(a, b []prompt.Msg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Role != b[i].Role || strings.TrimSpace(a[i].Content) != strings.TrimSpace(b[i].Content) {
			return false
		}
	}
	return true
}

// get issues one authenticated GET. Mirrors post: the status comes back with the
// error so a 404 can be told apart from a real failure without string matching.
func (c *Client) get(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", c.auth)
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("GET %s: decode response: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

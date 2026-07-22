// Anthropic backend for the turn classifier. Uses the raw Messages API over
// net/http (no SDK dependency) so cmd/eval can compare a hosted Claude model
// against the local Ollama models on the EXACT same prompt (classifyPrompt).
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IsAnthropicModel reports whether a model name should be served by the
// Anthropic API rather than the local Ollama daemon.
func IsAnthropicModel(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"claude", "sonnet", "opus", "haiku"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// NewClassifier routes a model name to the right backend: Anthropic models need
// a key; everything else goes through Ollama (cloud models are proxied by it).
func NewClassifier(model, anthropicKey string) (Classifier, error) {
	if IsAnthropicModel(model) {
		if strings.TrimSpace(anthropicKey) == "" {
			return nil, fmt.Errorf("model %q needs an Anthropic API key", model)
		}
		return NewAnthropic(anthropicKey, model), nil
	}
	return New(model)
}

// NewCompleter routes a model name to a Completer (same rules as NewClassifier).
// Used to build the eval's ack-quality judge.
func NewCompleter(model, anthropicKey string) (Completer, error) {
	if IsAnthropicModel(model) {
		if strings.TrimSpace(anthropicKey) == "" {
			return nil, fmt.Errorf("model %q needs an Anthropic API key", model)
		}
		return NewAnthropic(anthropicKey, model), nil
	}
	return New(model)
}

// DefaultAnthropicEnvFile is where we look for a key if $ANTHROPIC_API_KEY is
// unset — the pepita project's .env on this machine.
func DefaultAnthropicEnvFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".env"
	}
	return filepath.Join(home, "projects", "pepita", ".env")
}

// LoadAnthropicKey returns $ANTHROPIC_API_KEY, falling back to parsing
// ANTHROPIC_API_KEY from envFile. The value is never logged.
func LoadAnthropicKey(envFile string) (string, error) {
	if k := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); k != "" {
		return k, nil
	}
	f, err := os.Open(envFile)
	if err != nil {
		return "", fmt.Errorf("no $ANTHROPIC_API_KEY and can't read %s: %w", envFile, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "ANTHROPIC_API_KEY") {
			continue
		}
		_, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("ANTHROPIC_API_KEY not found in %s", envFile)
}

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
)

// AnthropicClient classifies turns via a hosted Claude model.
type AnthropicClient struct {
	key   string
	model string
	hc    *http.Client
}

// NewAnthropic builds a classifier for the given model id (e.g. "claude-sonnet-5").
func NewAnthropic(key, model string) *AnthropicClient {
	return &AnthropicClient{key: key, model: model, hc: &http.Client{Timeout: 90 * time.Second}}
}

type anthReq struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system"`
	Messages  []anthMsg `json:"messages"`
	// temperature is intentionally omitted: newer models (e.g. Sonnet 5) reject
	// it as deprecated, and we rely on the JSON prefill for determinism anyway.
}

type anthMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthResp struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ClassifyTurn implements Classifier using the same shared prompt as Ollama.
func (a *AnthropicClient) ClassifyTurn(ctx context.Context, question, reply string) (Turn, error) {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return Turn{Intent: IntentUnintellig, Sufficient: false}, nil
	}
	system, msgs := classifyPrompt(question, reply)
	am := make([]anthMsg, 0, len(msgs))
	for _, m := range msgs {
		am = append(am, anthMsg{Role: m.Role, Content: m.Content})
	}
	// NB: no assistant prefill — newer models require the conversation to end on
	// a user turn. We rely on the system instruction + normalizeTurn's object
	// extraction to get clean JSON. max_tokens leaves room for the ack string.
	text, err := a.do(ctx, anthReq{
		Model:     a.model,
		MaxTokens: 200,
		System:    system + " Output ONLY a single JSON object, nothing else.",
		Messages:  am,
	})
	if err != nil {
		return Turn{}, err
	}
	return normalizeTurn(text), nil
}

// Complete implements Completer: a one-shot system+user call returning raw text.
func (a *AnthropicClient) Complete(ctx context.Context, system, user string) (string, error) {
	return a.do(ctx, anthReq{
		Model:     a.model,
		MaxTokens: 300,
		System:    system,
		Messages:  []anthMsg{{Role: "user", Content: user}},
	})
}

// do posts one Messages request and returns the concatenated text content.
func (a *AnthropicClient) do(ctx context.Context, r anthReq) (string, error) {
	body, _ := json.Marshal(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := a.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var ar anthResp
	if err := json.Unmarshal(raw, &ar); err != nil {
		return "", fmt.Errorf("anthropic decode: %w", err)
	}
	if ar.Error != nil {
		return "", fmt.Errorf("anthropic: %s", ar.Error.Message)
	}
	var text strings.Builder
	for _, c := range ar.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return text.String(), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

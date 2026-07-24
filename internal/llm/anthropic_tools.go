package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ---- Tool-use (agent loop) support ----
//
// This file adds the Messages-API tool-calling surface used by the EXPERIMENTAL
// agent-loop conversation driver (internal/ws/agentloop.go), where the model
// chooses actions instead of returning a label. It is deliberately separate from
// anthropic.go so the production classifier path is untouched: same client, same
// key handling, different request shape.
//
// Note the contrast with ClassifyTurn, which is one stateless call per turn:
// here the conversation history GROWS with every tool_use/tool_result pair.

// Tool is a tool definition sent to the model.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Block is a content block, in either direction. Only the fields relevant to a
// given Type are populated (text / tool_use / tool_result).
type Block struct {
	Type string `json:"type"`

	// type "text"
	Text string `json:"text,omitempty"`

	// type "tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type "tool_result"
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// ToolMsg is one conversation turn carrying content blocks.
type ToolMsg struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

// Usage reports per-call token spend, so the agent loop's cost can be compared
// against the single-call classifier.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ToolStep is one model response inside an agent loop.
type ToolStep struct {
	Blocks     []Block
	StopReason string
	Usage      Usage
}

// ToolCalls returns just the tool_use blocks from the step.
func (s ToolStep) ToolCalls() []Block {
	var out []Block
	for _, b := range s.Blocks {
		if b.Type == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

// Text returns the concatenated text blocks from the step (what the model said
// alongside, or instead of, calling a tool).
func (s ToolStep) Text() string {
	var b bytes.Buffer
	for _, blk := range s.Blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// ToolRunner is any backend that can take one step of an agent loop.
type ToolRunner interface {
	Step(ctx context.Context, system string, msgs []ToolMsg, tools []Tool) (ToolStep, error)
}

type toolReq struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system"`
	Messages  []ToolMsg `json:"messages"`
	Tools     []Tool    `json:"tools"`
	// Thinking is disabled and effort kept low: this runs on the critical path of
	// a live voice turn, where adaptive thinking (the default on current models)
	// would add latency the respondent hears as dead air. This gives the agent
	// loop its best shot at competing with the single-call classifier.
	Thinking     map[string]any `json:"thinking,omitempty"`
	OutputConfig map[string]any `json:"output_config,omitempty"`
}

type toolRespBody struct {
	Content    []Block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// NewToolRunner builds a tool-calling runner for an Anthropic model id.
func NewToolRunner(model, key string) (ToolRunner, error) {
	if !IsAnthropicModel(model) {
		return nil, fmt.Errorf("agent loop needs an Anthropic model, got %q", model)
	}
	if key == "" {
		return nil, fmt.Errorf("no Anthropic API key available")
	}
	return NewAnthropic(key, model), nil
}

// Step sends one request in an agent loop and returns the model's blocks.
func (a *AnthropicClient) Step(ctx context.Context, system string, msgs []ToolMsg, tools []Tool) (ToolStep, error) {
	body, _ := json.Marshal(toolReq{
		Model:        a.model,
		MaxTokens:    1024,
		System:       system,
		Messages:     msgs,
		Tools:        tools,
		Thinking:     map[string]any{"type": "disabled"},
		OutputConfig: map[string]any{"effort": "low"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL, bytes.NewReader(body))
	if err != nil {
		return ToolStep{}, err
	}
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := a.hc.Do(req)
	if err != nil {
		return ToolStep{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return ToolStep{}, fmt.Errorf("anthropic HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out toolRespBody
	if err := json.Unmarshal(raw, &out); err != nil {
		return ToolStep{}, fmt.Errorf("anthropic decode: %w", err)
	}
	if out.Error != nil {
		return ToolStep{}, fmt.Errorf("anthropic: %s", out.Error.Message)
	}
	return ToolStep{Blocks: out.Content, StopReason: out.StopReason, Usage: out.Usage}, nil
}

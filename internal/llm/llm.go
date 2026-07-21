// Package llm talks to a local Ollama daemon. It does two jobs:
//   - GenerateQuestions: one-shot creation of poll questions from a product.
//   - ClassifyTurn: a cheap per-turn read of what the respondent's reply means
//     (answered / wants to stop / off-topic) plus whether it's a sufficient
//     answer. This drives follow-ups and early bail-out.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ollama/ollama/api"
)

// Client wraps the Ollama API client and the chosen model.
type Client struct {
	api   *api.Client
	model string
}

// New connects to the local Ollama daemon (honoring OLLAMA_HOST).
func New(model string) (*Client, error) {
	c, err := api.ClientFromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("ollama client: %w", err)
	}
	return &Client{api: c, model: model}, nil
}

var stream = false

// chat sends a single-shot chat with an optional JSON schema/format and returns
// the assistant's full text.
func (c *Client) chat(ctx context.Context, format json.RawMessage, msgs []api.Message) (string, error) {
	var sb strings.Builder
	err := c.api.Chat(ctx, &api.ChatRequest{
		Model:    c.model,
		Messages: msgs,
		Stream:   &stream,
		Format:   format,
		Options:  map[string]any{"temperature": 0.4},
	}, func(r api.ChatResponse) error {
		sb.WriteString(r.Message.Content)
		return nil
	})
	if err != nil {
		return "", err
	}
	return sb.String(), nil
}

// ---- Question generation ----

type questionsOut struct {
	Questions []string `json:"questions"`
}

// GenerateQuestions asks the model for 3-5 concise, spoken-style poll questions
// about the given product. Retries once on malformed JSON.
func (c *Client) GenerateQuestions(ctx context.Context, product string) ([]string, error) {
	sys := "You write short opinion-poll questions for a VOICE survey. " +
		"The questions will be read aloud and answered by speaking, so keep each " +
		"one to a single, natural, conversational sentence. No numbering, no preamble. " +
		"Ask about the respondent's honest opinion, experience, and suggestions. " +
		"NEVER use placeholders or brackets like [Name] or [Restaurant Name]; if a " +
		"specific detail is unknown, phrase the question generally (e.g. 'our candles')."
	user := fmt.Sprintf("Product / topic: %s\n\n"+
		"Write 3 to 5 poll questions. Respond ONLY as JSON: "+
		`{"questions": ["...", "..."]}`, strings.TrimSpace(product))

	format := json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"string"}}},"required":["questions"]}`)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := c.chat(ctx, format, []api.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		})
		if err != nil {
			return nil, err
		}
		var out questionsOut
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			lastErr = fmt.Errorf("bad JSON from model: %w (%q)", err, raw)
			continue
		}
		qs := cleanQuestions(out.Questions)
		if len(qs) == 0 {
			lastErr = fmt.Errorf("model returned no questions")
			continue
		}
		if len(qs) > 5 {
			qs = qs[:5]
		}
		return qs, nil
	}
	return nil, lastErr
}

func cleanQuestions(in []string) []string {
	out := make([]string, 0, len(in))
	for _, q := range in {
		q = strings.TrimSpace(q)
		if q != "" {
			out = append(out, q)
		}
	}
	return out
}

// ---- Per-turn classification ----

// Intent is what the respondent's utterance is doing this turn.
type Intent string

const (
	IntentAnswer     Intent = "answer"      // engaging with the question
	IntentWantsStop  Intent = "wants_stop"  // wants to end the survey early
	IntentOffTopic   Intent = "off_topic"   // not related to the question
	IntentUnintellig Intent = "unintelligible"
)

// Turn is the classifier's read of one reply.
type Turn struct {
	Intent     Intent `json:"intent"`
	Sufficient bool   `json:"sufficient"` // did they actually answer the question?
}

var turnFormat = json.RawMessage(`{"type":"object","properties":{"intent":{"type":"string","enum":["answer","wants_stop","off_topic","unintelligible"]},"sufficient":{"type":"boolean"}},"required":["intent","sufficient"]}`)

// ClassifyTurn reads a respondent reply in the context of the current question.
func (c *Client) ClassifyTurn(ctx context.Context, question, reply string) (Turn, error) {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return Turn{Intent: IntentUnintellig, Sufficient: false}, nil
	}
	sys := "You classify a survey respondent's spoken reply. Decide the intent and " +
		"whether it is a sufficient answer to the question. " +
		"'wants_stop' means they want to end the survey (e.g. 'I have to go', 'I'm done', " +
		"'stop', 'no more questions'). 'off_topic' means unrelated to the question. " +
		"'answer' means they engaged with the question (even briefly). Be lenient: a short " +
		"but on-point reply is sufficient. Respond ONLY as JSON."
	user := fmt.Sprintf("Question: %s\nReply: %s", question, reply)

	raw, err := c.chat(ctx, turnFormat, []api.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: user},
	})
	if err != nil {
		return Turn{}, err
	}
	var t Turn
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		// Fail safe: treat as a sufficient answer so the survey keeps moving.
		return Turn{Intent: IntentAnswer, Sufficient: true}, nil
	}
	if t.Intent == "" {
		t.Intent = IntentAnswer
	}
	return t, nil
}

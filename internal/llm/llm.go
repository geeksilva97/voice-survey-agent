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
// the assistant's full text. temp controls sampling: use ~0 for classification
// (we want a stable, repeatable label) and a little higher for generation.
func (c *Client) chat(ctx context.Context, temp float64, format json.RawMessage, msgs []api.Message) (string, error) {
	var sb strings.Builder
	err := c.api.Chat(ctx, &api.ChatRequest{
		Model:    c.model,
		Messages: msgs,
		Stream:   &stream,
		Format:   format,
		Options:  map[string]any{"temperature": temp},
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
		raw, err := c.chat(ctx, 0.4, format, []api.Message{
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
	IntentRepeat     Intent = "repeat"      // didn't hear/understand; wants the question again
	IntentOffTopic   Intent = "off_topic"   // not related to the question
	IntentUnintellig Intent = "unintelligible"
)

// Clarity is a SECOND axis, orthogonal to intent: did we understand the reply's
// content precisely? "clear" = yes; "unclear" = it's a real utterance (usually an
// answer) but the exact wording/meaning is hard to pin down — non-native/calque
// phrasing, a parseable-but-garbled transcript, or an ambiguous reference. It
// lets the agent do a light, natural confirmation ("repair") without treating a
// valid answer as garbage.
type Clarity string

const (
	ClarityClear   Clarity = "clear"
	ClarityUnclear Clarity = "unclear"
)

// Turn is the classifier's read of one reply.
type Turn struct {
	Intent     Intent  `json:"intent"`
	Sufficient bool    `json:"sufficient"` // did they actually answer the question?
	Clarity    Clarity `json:"clarity"`    // did we understand the content precisely?
}

var turnFormat = json.RawMessage(`{"type":"object","properties":{"intent":{"type":"string","enum":["answer","wants_stop","repeat","off_topic","unintelligible"]},"sufficient":{"type":"boolean"},"clarity":{"type":"string","enum":["clear","unclear"]}},"required":["intent","sufficient","clarity"]}`)

// Msg is a provider-neutral chat message (role: system/user/assistant). It lets
// every backend classify with the SAME prompt so model comparisons are fair.
type Msg struct{ Role, Content string }

// Classifier is any backend that can label a respondent's turn. The local Ollama
// Client and the Anthropic client both implement it, so cmd/eval can compare
// models on identical inputs.
type Classifier interface {
	ClassifyTurn(ctx context.Context, question, reply string) (Turn, error)
}

// classifyPrompt returns the shared system prompt and the conversation messages
// (few-shot exchanges + the final user turn) for one classification. Keeping it
// in one place means every model is judged on the exact same instructions.
func classifyPrompt(question, reply string) (system string, msgs []Msg) {
	system = classifySystem
	shots := []Msg{
		{Role: "user", Content: "Question: Is there a specific type of drink you would like us to offer more often?\nReply: A banana vitamin would be awesome."},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"unclear"}`},
		{Role: "user", Content: "Question: What's one thing you'd improve at our coffee shop?\nReply: I don't know, maybe better chairs I guess"},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"clear"}`},
		{Role: "user", Content: "Question: What's one thing you'd like to see improved at our coffee shop?\nReply: Nothing that comes to my mind actually."},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"clear"}`},
		{Role: "user", Content: "Question: How was the service?\nReply: The waiter was very educated and gentle with us."},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"unclear"}`},
		{Role: "user", Content: "Question: What do you think of our scented candles?\nReply: Sorry, what was the question?"},
		{Role: "assistant", Content: `{"intent":"repeat","sufficient":false,"clarity":"clear"}`},
		{Role: "user", Content: "Question: How likely are you to recommend us?\nReply: I really have to run now, sorry."},
		{Role: "assistant", Content: `{"intent":"wants_stop","sufficient":false,"clarity":"clear"}`},
		{Role: "user", Content: "Question: What's your favorite scent?\nReply: What time do you close today?"},
		{Role: "assistant", Content: `{"intent":"off_topic","sufficient":false,"clarity":"clear"}`},
		{Role: "user", Content: "Question: What do you think of our candles?\nReply: (buzzing) (buzzing)"},
		{Role: "assistant", Content: `{"intent":"unintelligible","sufficient":false,"clarity":"unclear"}`},
	}
	user := fmt.Sprintf("Question: %s\nReply: %s", question, reply)
	msgs = append(shots, Msg{Role: "user", Content: user})
	return system, msgs
}

// normalizeTurn parses a model's JSON label, isolating the first JSON object
// (models sometimes add stray text or we prefill an opening brace) and failing
// safe to a clear, sufficient answer so a parse glitch never stalls the survey.
func normalizeTurn(raw string) Turn {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		if j := strings.IndexByte(raw[i:], '}'); j >= 0 {
			raw = raw[i : i+j+1]
		}
	}
	var t Turn
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return Turn{Intent: IntentAnswer, Sufficient: true, Clarity: ClarityClear}
	}
	if t.Intent == "" {
		t.Intent = IntentAnswer
	}
	if t.Clarity == "" {
		t.Clarity = ClarityClear // conservative: only confirm when explicitly unclear
	}
	return t
}

const classifySystem = "You classify a survey respondent's spoken reply on TWO axes: 'intent' " +
	"(what they're doing) and 'clarity' (how well you understood the content).\n" +
	"INTENT:\n" +
	"- 'wants_stop': they want to END THE WHOLE SURVEY now (e.g. 'I have to go', 'I'm done', " +
	"'stop', 'no more questions', 'I'm not interested'). This is about quitting the survey, " +
	"NOT about having nothing to say for one question.\n" +
	"- 'repeat': they did NOT hear or understand the question and want it repeated or " +
	"clarified (e.g. 'what was the question?', 'can you repeat that?', 'I didn't catch it', " +
	"'I don't understand').\n" +
	"- 'off_topic': the reply is real, readable speech but about something ENTIRELY " +
	"UNRELATED to the question (e.g. asking the time, the weather, or an aside directed " +
	"at someone else like 'hold on, not you').\n" +
	"- 'unintelligible': ONLY when there are no real words at all — pure noise, empty, " +
	"transcription artifacts like '(buzzing)', or random letters. If you can read actual " +
	"words that form any statement or suggestion, it is NOT unintelligible.\n" +
	"- 'answer': they engaged with the question in any way.\n" +
	"CRITICAL: This is an OPINION SURVEY. Almost ANY on-topic reply is an 'answer' with " +
	"sufficient=true — including short, uncertain, vague, grammatically broken, or " +
	"UNUSUAL/UNEXPECTED suggestions. An odd-sounding or oddly-worded suggestion that still " +
	"names a thing or preference (e.g. 'a banana vitamin would be awesome') is a valid " +
	"answer. Declining to suggest anything for THIS question ('nothing comes to mind', 'no, " +
	"it's all good', 'I can't think of anything', 'not really') is ALSO a valid 'answer' " +
	"(sufficient=true) — it is NOT 'wants_stop'. Only use 'off_topic' when the reply has " +
	"nothing to do with the question. When in doubt, choose 'answer' with sufficient=true.\n" +
	"CLARITY:\n" +
	"- 'clear': you understood the content precisely (this is the DEFAULT — use it for normal, " +
	"well-formed English, including short or negative answers like 'nothing comes to mind').\n" +
	"- 'unclear': it's clearly an utterance (usually an answer) but the exact meaning is hard " +
	"to pin down — non-native/calque phrasing or literal translations ('a banana vitamin', " +
	"'the price is a little salty', 'make publicity in the television'), a parseable-but-" +
	"garbled transcript, or an ambiguous reference. Be CONSERVATIVE: only mark 'unclear' when " +
	"you genuinely couldn't be sure what they meant; otherwise 'clear'.\n" +
	"Respond ONLY as JSON with intent, sufficient, and clarity."

// ClassifyTurn (Ollama backend) reads a respondent reply in the context of the
// current question. Runs at temperature 0 so the label is stable/repeatable.
func (c *Client) ClassifyTurn(ctx context.Context, question, reply string) (Turn, error) {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return Turn{Intent: IntentUnintellig, Sufficient: false}, nil
	}
	system, msgs := classifyPrompt(question, reply)
	am := make([]api.Message, 0, len(msgs)+1)
	am = append(am, api.Message{Role: "system", Content: system})
	for _, m := range msgs {
		am = append(am, api.Message{Role: m.Role, Content: m.Content})
	}
	raw, err := c.chat(ctx, 0, turnFormat, am)
	if err != nil {
		return Turn{}, err
	}
	return normalizeTurn(raw), nil
}

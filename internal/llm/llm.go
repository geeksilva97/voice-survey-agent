// Package llm talks to a local Ollama daemon. It does two jobs:
//   - GenerateSurvey: one-shot creation of poll questions AND a warm,
//     product-tailored opening line from a product description.
//   - ClassifyTurn: a cheap per-turn read of what the respondent's reply means
//     (answered / wants to stop / off-topic) plus whether it's a sufficient
//     answer. This drives follow-ups and early bail-out.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

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

// SurveyPlan is a freshly generated survey: its questions plus a warm,
// product-tailored opening line the agent speaks before the first question.
// Intro is best-effort — it may be empty, in which case the caller falls back
// to a fixed greeting.
type SurveyPlan struct {
	Questions []string
	Intro     string
}

type surveyOut struct {
	Intro     string   `json:"intro"`
	Questions []string `json:"questions"`
}

// GenerateSurvey asks the model, in ONE call, for 3-5 concise spoken-style poll
// questions about the product AND a short, warm opening line to greet the
// respondent. Retries once on malformed JSON. The intro is optional: a missing
// or oversized one comes back empty so the caller can fall back to a fixed line.
func (c *Client) GenerateSurvey(ctx context.Context, product string) (SurveyPlan, error) {
	sys := "You set up a spoken VOICE opinion survey. You produce two things.\n\n" +
		"1) INTRO: one warm, natural opening line the agent SAYS OUT LOUD before the " +
		"first question. Greet the respondent, mention there are just a few quick " +
		"questions about the product, reassure them there are no wrong answers " +
		"(you want their honest take), and end with a short hand-off like " +
		"\"Here's the first:\" or \"Let's start:\". Keep it to 1-2 short sentences, " +
		"conversational and spoken (contractions are good). Do NOT include any actual " +
		"question in the intro. Do NOT say a specific number of questions — say " +
		"\"a few\". No emoji.\n\n" +
		"2) QUESTIONS: 3 to 5 poll questions, each read aloud and answered by " +
		"speaking, so each is a single natural conversational sentence. No numbering, " +
		"no preamble. Ask about the respondent's honest opinion, experience, and " +
		"suggestions.\n\n" +
		"NEVER use placeholders or brackets like [Name] or [Restaurant Name]; if a " +
		"specific detail is unknown, phrase things generally (e.g. 'our candles')."
	user := fmt.Sprintf("Product / topic: %s\n\n"+
		"Respond ONLY as JSON: "+
		`{"intro": "...", "questions": ["...", "..."]}`, strings.TrimSpace(product))

	format := json.RawMessage(`{"type":"object","properties":{"intro":{"type":"string"},"questions":{"type":"array","items":{"type":"string"}}},"required":["intro","questions"]}`)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := c.chat(ctx, 0.4, format, []api.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		})
		if err != nil {
			return SurveyPlan{}, err
		}
		var out surveyOut
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
		return SurveyPlan{Questions: qs, Intro: cleanIntro(out.Intro)}, nil
	}
	return SurveyPlan{}, lastErr
}

// cleanIntro trims the generated opening line and drops it (returns "") if it's
// empty or implausibly long, so a rambling small-model intro can't hijack the
// greeting — the caller falls back to the fixed line instead.
func cleanIntro(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"`)
	s = strings.TrimSpace(s)
	if s == "" || len([]rune(s)) > 300 {
		return ""
	}
	return s
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
	IntentRepeat     Intent = "repeat"      // didn't HEAR it; wants the question read again
	IntentNeedsHelp  Intent = "needs_help"  // heard it but unsure how to answer / asks us to clarify
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
	// Ack is a short, warm, SPECIFIC spoken lead-in the agent says right before
	// the next question — it reflects what the respondent just said (a normal
	// answer), gently steers back (an off-topic aside), or, for a 'needs_help'
	// reply, reassures them and hints how to answer. It's what makes the
	// conversation feel human instead of a form. Empty when no lead-in fits.
	Ack string `json:"ack"`
}

var turnFormat = json.RawMessage(`{"type":"object","properties":{"intent":{"type":"string","enum":["answer","wants_stop","repeat","needs_help","off_topic","unintelligible"]},"sufficient":{"type":"boolean"},"clarity":{"type":"string","enum":["clear","unclear"]},"ack":{"type":"string"}},"required":["intent","sufficient","clarity","ack"]}`)

// Msg is a provider-neutral chat message (role: system/user/assistant). It lets
// every backend classify with the SAME prompt so model comparisons are fair.
type Msg struct{ Role, Content string }

// Classifier is any backend that can label a respondent's turn. The local Ollama
// Client and the Anthropic client both implement it, so cmd/eval can compare
// models on identical inputs.
type Classifier interface {
	ClassifyTurn(ctx context.Context, question, reply string) (Turn, error)
}

// Completer runs a one-shot system+user instruction and returns the raw text.
// It's used by the eval's ack-quality judge — a generative score that is
// separate from turn classification. Both backends implement it so the judge
// can be a local or a hosted model.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// Complete (Ollama backend) runs a plain, un-formatted completion at temp 0.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	return c.chat(ctx, 0, nil, []api.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	})
}

// classifyPrompt returns the shared system prompt and the conversation messages
// (few-shot exchanges + the final user turn) for one classification. Keeping it
// in one place means every model is judged on the exact same instructions.
func classifyPrompt(question, reply string) (system string, msgs []Msg) {
	system = classifySystem
	shots := []Msg{
		{Role: "user", Content: "Question: Is there a specific type of drink you would like us to offer more often?\nReply: A banana vitamin would be awesome."},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"unclear","ack":""}`},
		{Role: "user", Content: "Question: What's one thing you'd improve at our coffee shop?\nReply: I don't know, maybe better chairs I guess"},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"clear","ack":"Comfier seating — good call."}`},
		{Role: "user", Content: "Question: What's one thing you'd like to see improved at our coffee shop?\nReply: Nothing that comes to my mind actually."},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"clear","ack":"All good there, got it."}`},
		{Role: "user", Content: "Question: How was the service?\nReply: The waiter was very educated and gentle with us."},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"unclear","ack":""}`},
		{Role: "user", Content: "Question: What do you think of our scented candles?\nReply: Sorry, what was the question?"},
		{Role: "assistant", Content: `{"intent":"repeat","sufficient":false,"clarity":"clear","ack":""}`},
		{Role: "user", Content: "Question: How would you rate the quality of our coffee?\nReply: Do you expect some score or something?"},
		{Role: "assistant", Content: `{"intent":"needs_help","sufficient":false,"clarity":"clear","ack":"No need for a score — just your honest gut feeling."}`},
		{Role: "user", Content: "Question: What's one thing you'd improve about our candles?\nReply: Hmm, I'm not really sure what you're looking for here."},
		{Role: "assistant", Content: `{"intent":"needs_help","sufficient":false,"clarity":"clear","ack":"However you'd like to answer is fine — big or small."}`},
		{Role: "user", Content: "Question: What could we improve about the app?\nReply: No, I can't think of anything right now."},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"clear","ack":"All good there, noted."}`},
		{Role: "user", Content: "Question: How has our app been working for you lately?\nReply: Eh, I dunno, it's, like, mostly fine I guess, you know?"},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"clear","ack":"Mostly smooth — good to hear."}`},
		{Role: "user", Content: "Question: What would make you shop with us more?\nReply: You should make more advertising, I never see your publicity anywhere."},
		{Role: "assistant", Content: `{"intent":"answer","sufficient":true,"clarity":"unclear","ack":""}`},
		{Role: "user", Content: "Question: How likely are you to recommend us?\nReply: I really have to run now, sorry."},
		{Role: "assistant", Content: `{"intent":"wants_stop","sufficient":false,"clarity":"clear","ack":""}`},
		{Role: "user", Content: "Question: What's your favorite scent?\nReply: What time do you close today?"},
		{Role: "assistant", Content: `{"intent":"off_topic","sufficient":false,"clarity":"clear","ack":"No worries —"}`},
		{Role: "user", Content: "Question: How often do you burn candles at home?\nReply: Did you catch the game last night?"},
		{Role: "assistant", Content: `{"intent":"off_topic","sufficient":false,"clarity":"clear","ack":"Ha, no worries —"}`},
		{Role: "user", Content: "Question: What do you think of our candles?\nReply: (buzzing) (buzzing)"},
		{Role: "assistant", Content: `{"intent":"unintelligible","sufficient":false,"clarity":"unclear","ack":""}`},
		{Role: "user", Content: "Question: How satisfied are you with the quality of our furniture?\nReply: (coughing)"},
		{Role: "assistant", Content: `{"intent":"unintelligible","sufficient":false,"clarity":"unclear","ack":""}`},
	}
	user := fmt.Sprintf("Question: %s\nReply: %s", question, reply)
	msgs = append(shots, Msg{Role: "user", Content: user})
	return system, msgs
}

// nonSpeechAnnot matches a parenthesized or bracketed span, e.g. "(coughing)"
// or "[inaudible]" — how STT engines annotate NON-SPEECH sounds.
var nonSpeechAnnot = regexp.MustCompile(`[\(\[][^\)\]]*[\)\]]`)

// IsNonSpeechArtifact reports whether a transcript is ENTIRELY non-speech sound
// annotation (a cough, laugh, buzzing, [inaudible]) with no actual spoken words.
// Weak models sometimes see the word inside the parentheses ("coughing") and
// treat it as an answer, so we detect this deterministically and force an
// 'unintelligible' turn — the agent must never say "Got it" and advance on a
// cough. Requires at least one bracketed span and no alphanumeric text outside.
func IsNonSpeechArtifact(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || !nonSpeechAnnot.MatchString(t) {
		return false
	}
	for _, r := range nonSpeechAnnot.ReplaceAllString(t, "") {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
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
	// Guard the ack: it's spoken, so drop anything long/rambly (weak models
	// sometimes over-produce) rather than cut it mid-word, and strip a trailing
	// question mark (an ack is a lead-in, not a question).
	t.Ack = strings.TrimSpace(t.Ack)
	if len(t.Ack) > 120 {
		t.Ack = ""
	}
	t.Ack = strings.TrimRight(t.Ack, " ?")
	return t
}

const classifySystem = "You classify a survey respondent's spoken reply on TWO axes: 'intent' " +
	"(what they're doing) and 'clarity' (how well you understood the content).\n" +
	"INTENT:\n" +
	"- 'wants_stop': they want to END THE WHOLE SURVEY now (e.g. 'I have to go', 'I'm done', " +
	"'stop', 'no more questions', 'I'm not interested'). This is about quitting the survey, " +
	"NOT about having nothing to say for one question.\n" +
	"- 'repeat': they did NOT HEAR the question (audio problem) and want it read again as-is " +
	"(e.g. 'what was the question?', 'can you repeat that?', 'I didn't catch it', 'say that " +
	"again?'). Use this only when they missed the words, not when they heard but are unsure.\n" +
	"- 'needs_help': use ONLY when they do NOT give any answer of their own and instead ask YOU to " +
	"explain or clarify the QUESTION — a question aimed back at the agent about what is being asked " +
	"(e.g. 'what do you mean?', 'do you expect a score or something?', 'like a number, or...?', 'what " +
	"are you looking for?', 'how should I answer that?'). It is a request for guidance, not an answer. " +
	"Do NOT use 'needs_help' just because a reply is vague, rambling, uncertain, negative, or hard to " +
	"parse — if they say ANYTHING on-topic of their own (including 'I can't think of anything' or a " +
	"messy/broken suggestion), that is 'answer'. And if they simply didn't HEAR it, that is 'repeat'.\n" +
	"- 'off_topic': the reply is real, readable speech but about something ENTIRELY " +
	"UNRELATED to the question — e.g. asking the time or the weather, an aside directed " +
	"at someone else ('hold on, not you'), or chatting about an unrelated subject like " +
	"last night's game or sports. This holds EVEN when it's phrased as a statement, not a " +
	"question — an unrelated statement is still off_topic, not an 'answer'.\n" +
	"- 'unintelligible': there are no real SPOKEN words to act on — pure noise, empty, random " +
	"letters, OR a transcription artifact describing a NON-SPEECH sound rather than speech, usually " +
	"in parentheses/brackets: '(buzzing)', '(coughing)', '(laughs)', '(sneezes)', '(clears throat)', " +
	"'(background noise)', '(music playing)'. Those are sounds, not answers, even though they contain " +
	"a word. If you can read actual spoken words that form any statement, question, or suggestion, it " +
	"is NOT unintelligible.\n" +
	"- 'answer': they engaged with the question in any way.\n" +
	"CRITICAL: This is an OPINION SURVEY. Almost ANY on-topic reply is an 'answer' with " +
	"sufficient=true — including short, uncertain, vague, grammatically broken, or " +
	"UNUSUAL/UNEXPECTED suggestions. An odd-sounding or oddly-worded suggestion that still " +
	"names a thing or preference (e.g. 'a banana vitamin would be awesome') is a valid " +
	"answer. Declining to suggest anything for THIS question ('nothing comes to mind', 'no, " +
	"it's all good', 'I can't think of anything', 'not really') is ALSO a valid 'answer' " +
	"(sufficient=true) — it is NOT 'wants_stop'. Only use 'off_topic' when the reply has " +
	"nothing to do with the question, and only use 'needs_help' when they ask YOU to explain the " +
	"question instead of answering it. When in doubt, choose 'answer' with sufficient=true.\n" +
	"CLARITY:\n" +
	"- 'clear': you understood the content precisely (this is the DEFAULT — use it for normal, " +
	"well-formed English, including short or negative answers like 'nothing comes to mind').\n" +
	"- 'unclear': it's clearly an utterance (usually an answer) but the exact meaning is hard " +
	"to pin down — non-native/calque phrasing or literal translations ('a banana vitamin', " +
	"'the price is a little salty', 'make publicity in the television'), a parseable-but-" +
	"garbled transcript, or an ambiguous reference. Be CONSERVATIVE: only mark 'unclear' when " +
	"you genuinely couldn't be sure what they meant; otherwise 'clear'.\n" +
	"ACK: a SHORT, warm, spoken lead-in the agent will say right before the next question — " +
	"it makes the survey feel like a conversation, not a form. Rules:\n" +
	"- Keep it to a few words, one short phrase. It is a lead-in, NOT a question (never end with '?').\n" +
	"- Make it SPECIFIC to what they actually said — reflect their point back ('Comfier seating, got it.', " +
	"'Love that — something tropical.'). A generic 'Got it, thanks!' repeated every turn feels robotic, so " +
	"vary it and tie it to their words.\n" +
	"- For an 'off_topic' reply: lightly acknowledge the aside and pivot back warmly ('Ha, no worries —', " +
	"'No problem —', 'Right, anyway —'), WITHOUT engaging the tangent and WITHOUT promising to discuss it " +
	"later. The agent re-asks the question itself right after.\n" +
	"- For a 'needs_help' reply: reassure them and hint HOW to answer, addressing their specific confusion " +
	"('No need for a score — just your honest gut feeling.', 'However you'd like to answer is fine — big " +
	"or small.'). Keep it short and warm; do NOT restate the question (the agent re-poses it right after). " +
	"Never end with '?'.\n" +
	"- Leave ack EMPTY (\"\") for 'wants_stop', 'repeat', 'unintelligible', and for any 'unclear' answer " +
	"(those get their own handling — no lead-in).\n" +
	"- NEVER invent facts about the product or the respondent. If nothing specific fits, a brief neutral " +
	"nod ('Got it.') is fine, but prefer specificity.\n" +
	"Respond ONLY as JSON with intent, sufficient, clarity, and ack."

// ClassifyTurn (Ollama backend) reads a respondent reply in the context of the
// current question. Runs at temperature 0 so the label is stable/repeatable.
func (c *Client) ClassifyTurn(ctx context.Context, question, reply string) (Turn, error) {
	reply = strings.TrimSpace(reply)
	if reply == "" || IsNonSpeechArtifact(reply) {
		return Turn{Intent: IntentUnintellig, Sufficient: false, Clarity: ClarityUnclear}, nil
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

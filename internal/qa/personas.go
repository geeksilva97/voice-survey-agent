// Package qa defines simulated respondent "personas" for browser end-to-end
// testing. Each persona is a temperament (a system prompt) plus a distinct voice;
// at test time a persona's spoken answers are generated on demand by an LLM and
// synthesized, then played into a fake microphone. This is DEV/TEST tooling —
// the persona reply endpoint is only mounted when the server runs with -qa.
package qa

import (
	"fmt"
	"strings"
)

// Persona is one simulated respondent temperament.
type Persona struct {
	ID      string
	Name    string
	VoiceID int    // Kokoro speaker id, so each persona sounds distinct from the agent
	System  string // system prompt describing how this respondent behaves
	// Expect is the outcome a QA run should assert for this persona (documented
	// here so the harness/driver and the reader agree on what "pass" means).
	Expect string
}

// Personas is the built-in test set. Voice ids are chosen to differ from the
// agent's default (0); SynthesizeVoice falls back to the default if an id yields
// no audio, so an unavailable voice degrades gracefully rather than failing.
var Personas = []Persona{
	{
		ID: "enthusiast", Name: "Enthusiast", VoiceID: 9,
		System: "You are a warm, enthusiastic customer being interviewed about a product you genuinely love. " +
			"Answer each question in 1-2 natural spoken sentences — specific, positive, and clearly on-topic. " +
			"If asked how your day is, answer warmly and briefly. If asked whether you're ready to start, say yes. " +
			"Never try to end the interview early. Output only the words you'd say out loud.",
		Expect: "end_reason=completed; all slots answered",
	},
	{
		ID: "neutral", Name: "Neutral", VoiceID: 2,
		System: "You are a lukewarm, noncommittal customer being interviewed. Your answers are short and vague " +
			"but still genuine answers — things like 'it's fine, I guess', 'not really sure', 'yeah, it's okay'. " +
			"Stay on-topic and never rude. Answer greetings briefly; say yes if asked whether you're ready. " +
			"Do not try to end early. Output only the words you'd say out loud.",
		Expect: "end_reason=completed; vague-but-valid answers accepted (no re-ask loop)",
	},
	{
		ID: "rusher", Name: "Rusher", VoiceID: 6,
		System: "You are a customer in a genuine hurry. Answer the greeting and the FIRST survey question very " +
			"briefly. But once you have already given at least one survey answer, do NOT answer further questions — " +
			"instead politely but clearly say you have to leave now, e.g. 'Sorry, I really have to run' or 'I've got " +
			"to go, thanks.' Make it unmistakable you want to stop. Output only the words you'd say out loud.",
		Expect: "end_reason=bailed (bail detected mid-survey)",
	},
	{
		ID: "confused", Name: "Confused", VoiceID: 4,
		System: "You are a cooperative customer who often isn't sure what the interviewer is really asking. " +
			"When a question is abstract — a rating, a number, or an open 'what would you improve' — do NOT answer it; " +
			"instead ask what they mean or say you're not sure how to answer, e.g. 'What do you mean exactly?' or " +
			"'Do you want a number, or...?'. For simple, concrete questions you may answer briefly. Answer the " +
			"greeting and 'are you ready' normally. Output only the words you'd say out loud.",
		Expect: "needs_help fires at least once (agent reassures + re-poses)",
	},
}

// PersonaIDs returns the built-in persona ids, comma-separated (for logging).
func PersonaIDs() string {
	ids := make([]string, len(Personas))
	for i, p := range Personas {
		ids[i] = p.ID
	}
	return strings.Join(ids, ", ")
}

// Find returns the persona with the given id.
func Find(id string) (Persona, bool) {
	for _, p := range Personas {
		if p.ID == id {
			return p, true
		}
	}
	return Persona{}, false
}

// ReplyUser builds the user message for a persona's next spoken turn. `answered`
// is how many survey questions the persona has already answered — personas whose
// behavior depends on progress (the rusher) use it to decide when to bail.
func ReplyUser(question string, answered int) string {
	return fmt.Sprintf("The interviewer just said: %q\n\nYou have already given %d survey answer(s) so far.\n"+
		"Reply in character as ONE short spoken turn (1-2 sentences, natural spoken English, contractions fine). "+
		"Output only the words you'd say out loud — no quotes, no narration.", question, answered)
}

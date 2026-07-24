package ws

// EXPERIMENTAL: an agent-loop conversation driver, as the alternative to the
// production classifier path. Here the model does NOT return a label for the Go
// router to act on — it calls tools, and those tools ARE the actions: record an
// answer, ask a question, say something, hang up. Termination moves from the
// state machine to the model (end_call).
//
// It deliberately shares this package's transport (STT, streamed TTS, the
// WebSocket protocol, the select loop and the silence timer) with the classifier
// path, so an A/B comparison isolates exactly one variable: who decides.
//
// What stays in Go even here, because it cannot move:
//   - the event loop and the silence clock (absence-of-input is not an input, so
//     no model can be invoked to notice it)
//   - question text fidelity: ask_question speaks the survey's EXACT wording, or
//     a paraphrasing model would quietly corrupt the instrument
//
// See docs/AGENT-LOOP-EXPERIMENT.md for the measured comparison.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"voicesurvey/internal/llm"
	"voicesurvey/internal/survey"
)

const (
	// maxAgentSteps caps model round trips per respondent turn. Each step is a
	// full inference on the critical path of a live voice turn, so this is a
	// latency ceiling as much as a runaway guard.
	maxAgentSteps = 5
	// agentStepTimeout bounds one model call.
	agentStepTimeout = 20 * time.Second
)

// agentState is the per-conversation bookkeeping for the agent path.
type agentState struct {
	msgs []llm.ToolMsg // GROWS every turn — unlike the stateless classifier

	steps    int // total model round trips this session
	turns    int // respondent turns processed
	inTok    int
	outTok   int
	modelMS  int64 // cumulative wall time spent inside model calls
	maxTurnS int   // worst-case steps in a single turn

	// endClaim records what the model asserted when it called end_call, so QA can
	// compare the model's belief against the actual slot state.
	endClaim string
}

// terminal is an action that ends the agent's turn and yields control (either to
// the respondent, or to the end of the call).
type terminal struct {
	kind     string // "say" | "ask" | "end"
	text     string // spoken line, or the preamble for "ask"
	slot     int
	reason   string
	toolName string
}

// ---- tool definitions ----

var agentTools = []llm.Tool{
	{
		Name: "record_answer",
		Description: "Record the respondent's answer to a specific survey question. " +
			"Call this once for EVERY question the respondent just answered — if a single reply " +
			"answers two questions, call it twice in the same turn. Does not speak to the respondent.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"slot":{"type":"integer","description":"1-based index of the question being answered"},` +
			`"answer":{"type":"string","description":"the respondent's answer, in their own words"}},` +
			`"required":["slot","answer"]}`),
	},
	{
		Name: "ask_question",
		Description: "Ask a survey question out loud and then listen. The question is spoken with its " +
			"EXACT survey wording — you supply only an optional short spoken lead-in before it. " +
			"Ends your turn.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"slot":{"type":"integer","description":"1-based index of the question to ask"},` +
			`"preamble":{"type":"string","description":"optional short warm lead-in spoken before the question; \"\" for none"}},` +
			`"required":["slot"]}`),
	},
	{
		Name: "say",
		Description: "Say something to the respondent and then listen — a greeting, a reassurance, a " +
			"clarifying probe, or a reply to something they asked. Use this when you are NOT asking a " +
			"survey question. Ends your turn.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"text":{"type":"string","description":"exactly what to say out loud"}},` +
			`"required":["text"]}`),
	},
	{
		Name: "end_call",
		Description: "Speak a closing line and hang up. Call this when every question has been answered " +
			"or skipped, or when the respondent wants to stop.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"farewell":{"type":"string","description":"the closing line to say out loud"},` +
			`"reason":{"type":"string","enum":["completed","bailed"],"description":"completed = the survey finished; bailed = the respondent chose to stop early"}},` +
			`"required":["farewell","reason"]}`),
	},
}

// agentSystem builds the static system prompt: the agent's persona, the verbatim
// question list, and the rules. Kept stable for the whole session so the prompt
// prefix stays cacheable; changing state travels in the per-turn user message.
func (cv *conversation) agentSystem() string {
	var b strings.Builder
	name := strings.TrimSpace(cv.h.AgentName)
	if name == "" {
		name = "Ava"
	}
	fmt.Fprintf(&b, "You are %s, a warm, personable voice-survey host running a SPOKEN survey over the phone. "+
		"Everything you say is synthesized to audio, so keep every line short and easy to listen to.\n\n", name)

	fmt.Fprintf(&b, "PRODUCT: %s\n", cv.poll.Product)
	if p := strings.TrimSpace(cv.poll.Purpose); p != "" {
		fmt.Fprintf(&b, "PURPOSE: %s\n", p)
	}
	b.WriteString("\nTHE QUESTIONS (ask each one; ask_question always speaks the exact wording below):\n")
	for i, q := range cv.sv.Questions {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, q.Text)
	}

	b.WriteString("\nHOW THE CALL GOES:\n" +
		"1. Open with a brief, human hello — introduce yourself and ask how their day is going. Do not " +
		"mention the survey yet.\n" +
		"2. When they reply, react briefly to what they actually said, then say how many questions you " +
		"have and what they're for, and check they're ready.\n" +
		"3. Work through the questions. Acknowledge each answer specifically before moving on.\n" +
		"4. When every question is answered or genuinely exhausted, end the call.\n\n")

	b.WriteString("RULES:\n" +
		"- ALWAYS act by calling a tool. Never reply with plain text: the respondent only hears what a " +
		"tool speaks.\n" +
		"- This is an OPINION survey. Almost any on-topic reply is a valid answer — short, vague, " +
		"uncertain, or 'nothing comes to mind' all count. Record it and move on.\n" +
		"- If one reply answers several questions, call record_answer for each of them.\n" +
		"- Probe at most ONCE per question. If they still don't give you anything usable, move on — " +
		"never ask the same question a third time.\n" +
		"- If they say they have to go, end the call immediately with reason 'bailed'.\n" +
		"- If they ask you to repeat a question, ask it again with an empty preamble.\n" +
		"- Never invent facts about the product or the respondent.\n")
	return b.String()
}

// agentStateLine renders the slot state for the per-turn user message. This is
// the agent path's substitute for the classifier path's cursor: the model has to
// be TOLD where it is, because nothing else is tracking that for it.
func (cv *conversation) agentStateLine() string {
	var open, done []string
	for i, q := range cv.sv.Questions {
		n := fmt.Sprintf("%d", i+1)
		switch q.Status {
		case survey.Answered:
			done = append(done, n)
		case survey.Skipped:
			done = append(done, n+"(skipped)")
		default:
			open = append(open, n)
		}
	}
	o, d := strings.Join(open, ","), strings.Join(done, ",")
	if o == "" {
		o = "none"
	}
	if d == "" {
		d = "none"
	}
	return fmt.Sprintf("[survey state] answered/skipped: %s | still open: %s", d, o)
}

// ---- driver ----

// agentOpen kicks off the conversation on the agent path.
func (cv *conversation) agentOpen() {
	cv.ag = &agentState{}
	cv.agentPush("user", cv.agentStateLine()+
		"\n[session start] The respondent just picked up. Open the call.")
	cv.agentTurn()
}

// handleUtteranceAgent transcribes a reply and lets the model decide what to do
// with it. Returns true when the conversation has ended.
func (cv *conversation) handleUtteranceAgent(pcm []byte) bool {
	text := cv.h.Speech.Transcribe(pcm, inputSampleRate)
	cv.send(outMsg{Type: "transcript", Text: text})

	// The deterministic non-speech guard is worth keeping on both paths: a model
	// that sees the word inside "(coughing)" will happily record it as an answer.
	if llm.IsNonSpeechArtifact(text) || strings.TrimSpace(text) == "" {
		text = "(unintelligible noise — no words)"
	}
	cv.ag.turns++
	cv.agentPush("user", cv.agentStateLine()+"\nThe respondent said: "+quote(text))
	return cv.agentTurn()
}

// agentTurn runs the agent loop until a tool yields control. Returns true when
// the conversation has ended.
func (cv *conversation) agentTurn() bool {
	stepsThisTurn := 0
	for i := 0; i < maxAgentSteps; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), agentStepTimeout)
		start := time.Now()
		step, err := cv.h.Agent.Step(ctx, cv.agentSystem(), cv.ag.msgs, agentTools)
		elapsed := time.Since(start)
		cancel()

		cv.ag.steps++
		stepsThisTurn++
		cv.ag.modelMS += elapsed.Milliseconds()
		cv.ag.inTok += step.Usage.InputTokens
		cv.ag.outTok += step.Usage.OutputTokens
		if stepsThisTurn > cv.ag.maxTurnS {
			cv.ag.maxTurnS = stepsThisTurn
		}
		if err != nil {
			log.Printf("agent step error: %v", err)
			return cv.agentFailSafe()
		}
		log.Printf("agent step %d (turn step %d): %dms in=%d out=%d stop=%s tools=%d",
			cv.ag.steps, stepsThisTurn, elapsed.Milliseconds(),
			step.Usage.InputTokens, step.Usage.OutputTokens, step.StopReason, len(step.ToolCalls()))

		cv.ag.msgs = append(cv.ag.msgs, llm.ToolMsg{Role: "assistant", Content: step.Blocks})

		calls := step.ToolCalls()
		if len(calls) == 0 {
			// The model answered in plain text instead of calling a tool. The
			// respondent can't hear text, so this turn produced nothing — speak it
			// rather than going silent, and note it as a protocol miss.
			log.Printf("agent returned text with no tool call (stop=%s)", step.StopReason)
			if t := strings.TrimSpace(step.Text()); t != "" {
				cv.qaTool("no_tool_text")
				cv.speak(t, "followup")
				return false
			}
			return cv.agentFailSafe()
		}

		// Execute every tool call in the response; results all go back in ONE user
		// message. Non-terminal tools (record_answer) run first so a terminal tool
		// in the same batch sees their effect.
		var results []llm.Block
		var term *terminal
		for _, c := range calls {
			res, t := cv.agentExec(c)
			results = append(results, res)
			if t != nil {
				term = t
			}
		}
		cv.agentPushBlocks("user", results)

		if term != nil {
			return cv.agentApply(*term)
		}
	}
	// Step budget spent without yielding control — the agent-loop analogue of a
	// runaway. Fail forward the same way the classifier path does.
	log.Printf("agent exhausted %d steps without speaking; failing safe", maxAgentSteps)
	return cv.agentFailSafe()
}

// agentExec runs one tool call, returning the tool_result block and (for a
// terminal tool) the action to apply once every call in the batch is executed.
func (cv *conversation) agentExec(c llm.Block) (llm.Block, *terminal) {
	cv.qaTool(c.Name)
	res := func(s string, isErr bool) llm.Block {
		return llm.Block{Type: "tool_result", ToolUseID: c.ID, Content: s, IsError: isErr}
	}

	switch c.Name {
	case "record_answer":
		var in struct {
			Slot   int    `json:"slot"`
			Answer string `json:"answer"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			return res("could not parse arguments: "+err.Error(), true), nil
		}
		if in.Slot < 1 || in.Slot > len(cv.sv.Questions) {
			return res(fmt.Sprintf("no such question %d (there are %d)", in.Slot, len(cv.sv.Questions)), true), nil
		}
		cv.sv.Fill(in.Slot-1, in.Answer)
		cv.persist()
		return res("recorded. "+cv.agentStateLine(), false), nil

	case "ask_question":
		var in struct {
			Slot     int    `json:"slot"`
			Preamble string `json:"preamble"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			return res("could not parse arguments: "+err.Error(), true), nil
		}
		if in.Slot < 1 || in.Slot > len(cv.sv.Questions) {
			return res(fmt.Sprintf("no such question %d (there are %d)", in.Slot, len(cv.sv.Questions)), true), nil
		}
		return res("asked; now listening for their reply", false),
			&terminal{kind: "ask", slot: in.Slot, text: in.Preamble, toolName: c.Name}

	case "say":
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			return res("could not parse arguments: "+err.Error(), true), nil
		}
		// Guard against a degenerate `say`. Observed in the A/B: the model once
		// called say with a lone `"` and the agent dutifully synthesized a stray
		// quote mark at the respondent. Anything with no letters or digits is not
		// speech, so refuse it and let the model try again — the classifier path
		// gets this free, because it never speaks model output that isn't either a
		// survey question or a length-checked ack.
		if !hasSpeech(in.Text) {
			return res("that isn't speakable text — call say with a real sentence, or use another tool", true), nil
		}
		return res("said; now listening for their reply", false),
			&terminal{kind: "say", text: in.Text, toolName: c.Name}

	case "end_call":
		var in struct {
			Farewell string `json:"farewell"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			return res("could not parse arguments: "+err.Error(), true), nil
		}
		return res("call ended", false),
			&terminal{kind: "end", text: in.Farewell, reason: in.Reason, toolName: c.Name}
	}
	return res("unknown tool "+c.Name, true), nil
}

// agentApply performs a terminal action: speak and listen, or speak and hang up.
func (cv *conversation) agentApply(t terminal) bool {
	switch t.kind {
	case "ask":
		q := &cv.sv.Questions[t.slot-1]
		if q.Status == survey.Unasked {
			q.Status = survey.Asked
		}
		cv.speakPaced(t.text, q.Text, t.slot, len(cv.sv.Questions))
		return false

	case "say":
		cv.speak(t.text, "followup")
		return false

	case "end":
		// The model has asserted the call is over. Record BOTH its claim and what
		// the slots actually say — the gap between them is the whole question this
		// experiment exists to answer.
		claimed := t.reason
		actual, open := cv.slotTally()
		cv.ag.endClaim = claimed
		log.Printf("agent end_call: claimed=%s | slots answered/skipped=%d open=%d", claimed, actual, open)
		if open > 0 && claimed == "completed" {
			log.Printf("MISMATCH: model claimed 'completed' with %d question(s) still open", open)
		}
		reason := survey.Bailed
		if claimed == "completed" {
			reason = survey.Completed
		}
		if reason == survey.Bailed {
			cv.sv.Bail()
		}
		// Deliberately NOT forcing the open slots closed: leaving them as-is means
		// data/<id>.json preserves "end_reason=completed with 2 slots never asked"
		// if that's what happened, instead of laundering it into a clean record.
		cv.persist()
		line := strings.TrimSpace(t.text)
		if line == "" {
			line = defaultClose
		}
		cv.speak(line, "closing")
		cv.agentReport()
		cv.finalize(reason)
		return true
	}
	return false
}

// agentFailSafe keeps the call moving when the model errors, stalls, or answers
// without acting — the agent path's equivalent of normalizeTurn failing open.
// Unlike the classifier's fail-open (which is a struct default), this one has to
// re-derive intent from Go: with no label to fall back on, the only safe move is
// to re-ask an open question or wrap up.
func (cv *conversation) agentFailSafe() bool {
	for i, q := range cv.sv.Questions {
		if q.Status != survey.Answered && q.Status != survey.Skipped {
			cv.sv.Questions[i].Status = survey.Asked
			cv.speakPaced("", q.Text, i+1, len(cv.sv.Questions))
			return false
		}
	}
	cv.persist()
	cv.speak(defaultClose, "closing")
	cv.agentReport()
	cv.finalize(survey.Completed)
	return true
}

// slotTally returns (answered-or-skipped, still-open).
func (cv *conversation) slotTally() (int, int) {
	var done, open int
	for _, q := range cv.sv.Questions {
		if q.Status == survey.Answered || q.Status == survey.Skipped {
			done++
		} else {
			open++
		}
	}
	return done, open
}

// agentReport logs the session's cost/latency profile for the A/B comparison.
func (cv *conversation) agentReport() {
	if cv.ag == nil {
		return
	}
	done, open := cv.slotTally()
	perTurn := 0.0
	if cv.ag.turns > 0 {
		perTurn = float64(cv.ag.steps) / float64(cv.ag.turns)
	}
	avgMS := int64(0)
	if cv.ag.steps > 0 {
		avgMS = cv.ag.modelMS / int64(cv.ag.steps)
	}
	log.Printf("AGENT REPORT poll=%s turns=%d steps=%d steps/turn=%.2f worst_turn_steps=%d "+
		"model_ms_total=%d model_ms_avg=%d in_tok=%d out_tok=%d slots_done=%d slots_open=%d end_claim=%s",
		cv.poll.ID, cv.ag.turns, cv.ag.steps, perTurn, cv.ag.maxTurnS,
		cv.ag.modelMS, avgMS, cv.ag.inTok, cv.ag.outTok, done, open, cv.ag.endClaim)
}

// ---- small helpers ----

func (cv *conversation) agentPush(role, text string) {
	cv.agentPushBlocks(role, []llm.Block{{Type: "text", Text: text}})
}

func (cv *conversation) agentPushBlocks(role string, blocks []llm.Block) {
	cv.ag.msgs = append(cv.ag.msgs, llm.ToolMsg{Role: role, Content: blocks})
}

// qaTool mirrors the tool the model just chose to the client, so the browser E2E
// harness can assert on real agent decisions the way it asserts on classifier
// intents. QA mode only.
func (cv *conversation) qaTool(name string) {
	if !cv.h.QA {
		return
	}
	cv.send(outMsg{Type: "qa_intent", Kind: "agent", Intent: name})
}

func quote(s string) string { return `"` + strings.TrimSpace(s) + `"` }

// hasSpeech reports whether a string contains anything a voice could actually
// say — at least one letter or digit. Punctuation alone is not speech.
func hasSpeech(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

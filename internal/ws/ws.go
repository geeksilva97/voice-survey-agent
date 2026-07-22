// Package ws hosts the voice conversation over a single WebSocket. The server
// is authoritative: it decides what the agent says, when to listen, when to
// follow up, and when to end (happy-path completion, early bail-out, or
// silence). The browser only captures speech (client-side VAD) and plays audio.
//
// Protocol (text frames = JSON control, binary frames = audio):
//
//	client -> server:
//	  {"type":"ready"}          respondent page loaded, mic granted
//	  {"type":"playback_done"}  agent audio finished; server may start listening
//	  {"type":"barge_in"}       user started speaking over the agent (Phase 5)
//	  <binary>                  PCM16 mono 16kHz utterance (on VAD speech end)
//
//	server -> client:
//	  {"type":"agent_say","text":...,"kind":...,"index":i,"total":n}  (+ binary WAV after)
//	  {"type":"transcript","text":...}   what STT heard
//	  {"type":"cancel"}                  stop/clear playback (barge-in ack)
//	  {"type":"done","reason":...}       conversation ended
package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"voicesurvey/internal/llm"
	"voicesurvey/internal/session"
	"voicesurvey/internal/speech"
	"voicesurvey/internal/survey"
)

// Tuning knobs for turn-taking timeouts.
const (
	inputSampleRate   = 16000            // browser captures/sends 16kHz mono
	silenceWindow     = 12 * time.Second // how long to wait for a reply before nudging
	maxSilenceStrikes = 2                // nudges before ending on silence
	maxReasks         = 2                // times we re-read a question before moving on
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true }, // PoC: allow any origin
}

// Handler wires the shared engines into the websocket route. LLM is the turn
// classifier — any backend (local Ollama or Anthropic) via llm.Classifier.
// Closer authors the personalized closing sign-off (optional: nil falls back to
// a fixed farewell).
type Handler struct {
	Store  *session.Store
	Speech *speech.Engine
	LLM    llm.Classifier
	Closer llm.Completer
	// Greeting opens each session with a short "how's your day" exchange before
	// the survey (a warm human hello), reusing the classifier to read the reply.
	Greeting bool
	// AgentName is the voice agent's name, used in the fixed fallback greeting
	// when the poll has no LLM-authored greeting variants.
	AgentName string
	// Pacing delivers a turn as two beats — a short acknowledgment, a brief
	// pause, then the question — instead of one breath, so the agent connects to
	// the next question like a person. Off restores single-utterance delivery.
	Pacing bool
	// QA mirrors each per-turn classifier decision to the client as a
	// {"type":"qa_intent"} frame so the browser E2E harness can assert on the
	// real intents that fired (needs_help, wants_stop, …) instead of scraping the
	// transcript. DEV/TEST ONLY — set from the server's -qa flag; never in prod.
	QA bool
}

// outMsg is a server->client control frame.
type outMsg struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Index  int    `json:"index,omitempty"`
	Total  int    `json:"total,omitempty"`
	Reason string `json:"reason,omitempty"`
	// QA-only (type "qa_intent"): the classifier's decision for the turn just
	// processed. Emitted only when the handler runs with QA=true so the browser
	// E2E harness can assert on real intents. Ignored by the production client.
	Intent     string `json:"intent,omitempty"`
	Clarity    string `json:"clarity,omitempty"`
	Sufficient bool   `json:"sufficient,omitempty"`
}

// event is something the read loop observed.
type eventKind int

const (
	evUtterance eventKind = iota
	evPlaybackDone
	evBargeIn
	evUserSpeaking
	evClosed
)

type event struct {
	kind eventKind
	pcm  []byte
}

// Serve upgrades the connection and runs the conversation for ?poll=<id>.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("poll")
	poll, ok := h.Store.Get(id)
	if !ok {
		http.Error(w, "unknown poll", http.StatusNotFound)
		return
	}
	// Restart-on-connect: each new session starts a FRESH run of the same poll,
	// so the same /poll/<id> link can be re-taken (reload the page to restart).
	poll.Survey = survey.New(poll.Questions)
	poll.EndReason = survey.NotEnded
	poll.EndedAt = nil
	h.Store.Save(poll)

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()

	conv := &conversation{h: h, poll: poll, c: c, sv: poll.Survey}
	conv.run()
}

// conversation drives one respondent session.
type conversation struct {
	h    *Handler
	poll *session.Poll
	c    *websocket.Conn
	sv   *survey.Survey

	speaking   bool // true while we expect the client to be playing audio
	strikes    int  // consecutive silence nudges
	reasks     int  // times we've re-read the current question
	inGreeting    bool // awaiting the reply to the opening "how's your day" small-talk
	awaitingStart bool // greeting done + framed; awaiting a "ready?" go-ahead before Q1

	// Conversational repair: when an answer is understood-but-unclear (calque /
	// heavy ESL / ambiguous), the agent confirms ONCE before advancing. Capped
	// per question and fail-open so it can never loop.
	confirmed       bool   // already did a repair for the current question
	awaitingConfirm bool   // last agent turn was a repair; next reply resolves it
	tentative       string // the unclear answer we're confirming
}

func (cv *conversation) run() {
	events := make(chan event, 8)
	go cv.readLoop(events)

	// Opening. With the greeting pre-layer on, a short "how's your day" exchange
	// comes first; otherwise we open straight into the intro + first question.
	if cv.h.Greeting {
		cv.openGreeting()
	} else {
		cv.greetAndAskFirst()
	}

	timer := time.NewTimer(silenceWindow)
	timer.Stop() // only armed while listening
	defer timer.Stop()

	// stopSilence/resetSilence drain timer.C so a value that fired while we were
	// busy in handleUtterance can't trigger a spurious reprompt after we reset.
	stopSilence := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	resetSilence := func() {
		stopSilence()
		timer.Reset(silenceWindow)
	}
	arm := func() {
		if !cv.speaking {
			resetSilence()
		}
	}

	for {
		select {
		case ev, alive := <-events:
			if !alive {
				return
			}
			switch ev.kind {
			case evClosed:
				return

			case evPlaybackDone:
				// Agent finished talking; begin listening + silence countdown.
				cv.speaking = false
				resetSilence()

			case evBargeIn:
				// User talked over the agent: tell client to stop playback,
				// then listen. (Phase 5; harmless if it fires otherwise.)
				cv.send(outMsg{Type: "cancel"})
				cv.speaking = false
				resetSilence()

			case evUserSpeaking:
				// The respondent is actively talking. Restart the silence clock
				// so a long, pause-filled answer doesn't trip the "still there?"
				// nudge before they finish and VAD emits the utterance.
				cv.strikes = 0
				if !cv.speaking {
					resetSilence()
				}

			case evUtterance:
				stopSilence()
				cv.strikes = 0
				if done := cv.handleUtterance(ev.pcm); done {
					return
				}
				arm()
			}

		case <-timer.C:
			if cv.speaking {
				continue
			}
			cv.strikes++
			if cv.strikes >= maxSilenceStrikes {
				cv.endSurveyBySilence()
				return
			}
			cv.speak("Are you still there? Take your time — whenever you're ready.", "reprompt")
			// speak() sets speaking=true; playback_done will re-arm the timer.
		}
	}
}

// handleUtterance transcribes a reply, classifies it, updates the survey, and
// speaks the next thing. Returns true when the conversation has ended.
func (cv *conversation) handleUtterance(pcm []byte) bool {
	text := cv.h.Speech.Transcribe(pcm, inputSampleRate)
	cv.send(outMsg{Type: "transcript", Text: text})

	// The opening small-talk reply is handled separately — it's not a survey slot.
	if cv.inGreeting {
		return cv.handleGreeting(text)
	}
	// After framing the survey the agent asked "ready?"; this reply is the
	// go-ahead (or an early bail) — still not a survey slot.
	if cv.awaitingStart {
		return cv.handleStart(text)
	}

	q, ok := cv.sv.Current()
	if !ok {
		return cv.finishCompleted("")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	turn, err := cv.h.LLM.ClassifyTurn(ctx, q.Text, text)
	if err != nil {
		log.Printf("classify error: %v", err)
		turn = llm.Turn{Intent: llm.IntentAnswer, Sufficient: true, Clarity: llm.ClarityClear} // keep moving
	}
	cv.qaSignal("survey", turn)

	// Resolving a repair: the previous agent turn asked "did I get that right?".
	// This reply is the confirmation/correction. Fail-open: capture and advance,
	// never repair the same question twice.
	if cv.awaitingConfirm {
		cv.awaitingConfirm = false
		if turn.Intent == llm.IntentWantsStop { // they can still bail mid-repair
			cv.sv.Bail()
			cv.persist()
			cv.speak("No problem at all — thanks so much for the time you gave us. Take care!", "closing")
			cv.finalize(survey.Bailed)
			return true
		}
		// A bare "yes/right/exactly" keeps the original answer; anything more is
		// treated as the corrected answer.
		ans := cv.tentative
		if !isAffirmation(text) {
			ans = text
		}
		cv.sv.RecordAnswer(ans)
		cv.persist()
		return cv.askNextOrFinish("")
	}

	// Early bail-out (Phase 4): respondent wants to stop the WHOLE survey.
	if turn.Intent == llm.IntentWantsStop {
		cv.sv.Bail()
		cv.persist()
		cv.speak("No problem at all — thanks so much for the time you gave us. Take care!", "closing")
		cv.finalize(survey.Bailed)
		return true
	}

	// "Repeat / I didn't understand": re-read the current question verbatim
	// (doesn't consume a follow-up or advance). Capped so it can't loop forever.
	if turn.Intent == llm.IntentRepeat {
		if cv.reasks < maxReasks {
			cv.reasks++
			idx, total := cv.sv.Progress()
			msg := "Sure, here it is again. " + q.Text
			cv.emit(outMsg{Type: "agent_say", Text: msg, Kind: "question", Index: idx, Total: total}, msg)
			return false
		}
		// Asked too many times — skip this one and move on.
		cv.sv.CaptureAndAdvance("")
		cv.persist()
		return cv.askNextOrFinish("")
	}

	// "Needs help": they heard the question but don't know how to answer it (or
	// asked us to clarify). Reassure + hint how to answer (the classifier's ack),
	// then re-pose the SAME question — don't advance, so they get a real second
	// shot. Shares the re-ask budget so a confused respondent can't loop forever.
	if turn.Intent == llm.IntentNeedsHelp {
		if cv.reasks < maxReasks {
			cv.reasks++
			idx, total := cv.sv.Progress()
			msg := helpPrompt(turn.Ack, q.Text)
			cv.emit(outMsg{Type: "agent_say", Text: msg, Kind: "question", Index: idx, Total: total}, msg)
			return false
		}
		// Still no answer after helping — don't fabricate one; skip honestly.
		cv.sv.CaptureAndAdvance("")
		cv.persist()
		return cv.askNextOrFinish("")
	}

	// A sufficient, on-topic answer.
	if turn.Intent == llm.IntentAnswer && turn.Sufficient {
		// Understood-but-unclear (calque / heavy ESL / ambiguous): confirm ONCE,
		// echoing their own words, before advancing. Natural "repair", not a
		// verbatim re-ask. Capped per question via cv.confirmed.
		if turn.Clarity == llm.ClarityUnclear && !cv.confirmed {
			cv.confirmed = true
			cv.tentative = text
			cv.awaitingConfirm = true
			cv.speak(repairPrompt(text), "confirm")
			return false
		}
		cv.sv.RecordAnswer(text)
		cv.persist()
		// Acknowledge what they said, then move on — this warm, specific lead-in
		// is what makes the survey feel like a conversation instead of a form.
		return cv.askNextOrFinish(turn.Ack)
	}

	// Off-topic / unintelligible: acknowledge + steer back to the question once.
	// A vague-but-on-topic answer (answer, insufficient) gets a gentle probe.
	if cv.sv.FollowUp() {
		cv.speak(followUpPrompt(turn.Intent, turn.Ack, q.Text), "followup")
		return false
	}
	// Follow-up budget spent. Don't fabricate an answer from an off-topic aside
	// or noise — skip the slot (records it Skipped) so results stay honest. A
	// thin but on-topic answer is still worth keeping.
	if turn.Intent == llm.IntentOffTopic || turn.Intent == llm.IntentUnintellig {
		cv.sv.CaptureAndAdvance("") // empty → Skipped, not a bogus answer
	} else {
		cv.sv.CaptureAndAdvance(text)
	}
	cv.persist()
	return cv.askNextOrFinish("")
}

// askNextOrFinish advances to the next unanswered question, prepending an
// optional spoken lead-in (an acknowledgment of the previous answer). When no
// questions remain it speaks the closing line, carrying the lead-in along.
func (cv *conversation) askNextOrFinish(lead string) bool {
	if done, reason := cv.sv.Done(); done {
		if reason == survey.Completed {
			return cv.finishCompleted(lead)
		}
	}
	q, ok := cv.sv.Current()
	if !ok {
		return cv.finishCompleted(lead)
	}
	cv.reasks = 0        // new question — reset the re-ask counter
	cv.confirmed = false // ...and the one-repair-per-question budget
	cv.awaitingConfirm = false
	cv.sv.MarkAsked()
	idx, total := cv.sv.Progress()
	cv.speakPaced(lead, q.Text, idx, total)
	return false
}

// defaultClose is the fixed, always-safe farewell used when no personalized
// sign-off is available (no Closer wired, LLM error, or an implausible result).
const defaultClose = "That's everything I wanted to ask. Thank you so much for sharing your thoughts — it really helps. Goodbye!"

func (cv *conversation) finishCompleted(lead string) bool {
	// Try a warm sign-off that references what the respondent actually said —
	// the most human moment available, and free of latency worry since the call
	// is ending. On any failure, fall back to the fixed close (carrying the
	// last-turn acknowledgment as a lead-in). A personalized close already
	// acknowledges, so we deliberately drop `lead` in that path to avoid
	// double-thanking.
	if closing := cv.personalClose(); closing != "" {
		cv.speak(closing, "closing")
	} else {
		cv.speak(withLead(lead, defaultClose), "closing")
	}
	cv.finalize(survey.Completed)
	return true
}

// personalClose asks the Closer model for a short farewell that references a
// genuine highlight from the answers. Returns "" (→ caller uses defaultClose)
// when there's no Closer, nothing was actually answered, the call errors, or the
// result fails the sanity check.
func (cv *conversation) personalClose() string {
	if cv.h.Closer == nil {
		return ""
	}
	transcript := closeTranscript(cv.sv)
	if transcript == "" {
		return "" // nothing captured to reference — a personalized line would be hollow
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := cv.h.Closer.Complete(ctx, closingSystem, "Product: "+cv.poll.Product+"\n\nWhat the respondent said:\n"+transcript+"\n\nWrite the closing line.")
	if err != nil {
		log.Printf("personalized close degraded, using fixed line: %v", err)
		return ""
	}
	return sanitizeClosing(raw)
}

const closingSystem = "You are a friendly voice-survey agent wrapping up a short spoken survey. " +
	"Write ONE warm closing line to SAY OUT LOUD. Requirements: reference ONE " +
	"specific, genuine thing the respondent actually said (their idea, not their " +
	"exact words); then thank them and say goodbye. 1-2 short sentences, natural " +
	"spoken English (contractions welcome), under 35 words. No lists, no emoji, no " +
	"questions, no placeholders. Never invent anything they did not say. Output " +
	"only the spoken line, nothing else."

// closeTranscript renders the answered slots as a compact Q/A transcript for the
// closing prompt. Skipped/empty slots are omitted so the model can't reference a
// question the respondent never actually engaged with.
func closeTranscript(sv *survey.Survey) string {
	var b strings.Builder
	for _, q := range sv.Questions {
		a := strings.TrimSpace(q.Answer)
		if q.Status != survey.Answered || a == "" {
			continue
		}
		b.WriteString("Q: ")
		b.WriteString(strings.TrimSpace(q.Text))
		b.WriteString("\nA: ")
		b.WriteString(a)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// sanitizeClosing trims the model's farewell and rejects (returns "") anything
// implausible — empty, multi-paragraph, or too long — so a misbehaving model
// can never speak junk at the end. Surrounding quotes are stripped.
func sanitizeClosing(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"`)
	s = strings.TrimSpace(s)
	// Collapse any stray newlines into single spaces (it must be one spoken line).
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || len([]rune(s)) > 240 {
		return ""
	}
	return s
}

// introLine returns the LLM-authored opening if present, else a fixed greeting.
func introLine(intro, product string) string {
	if s := strings.TrimSpace(intro); s != "" {
		return s
	}
	return "Hi! A few quick questions about " + product + ". There are no wrong answers. Here's the first:"
}

// withLead prepends a spoken acknowledgment to the next line, if present.
func withLead(lead, text string) string {
	if lead = strings.TrimSpace(lead); lead == "" {
		return text
	}
	return lead + " " + text
}

func (cv *conversation) endSurveyBySilence() {
	cv.sv.TimeOut()
	cv.persist()
	cv.speak("It seems you've stepped away, so I'll wrap up here. Thanks, and take care!", "closing")
	cv.finalize(survey.Silence)
}

func (cv *conversation) greetAndAskFirst() {
	q, ok := cv.sv.Current()
	if !ok {
		cv.finishCompleted("")
		return
	}
	// One combined opening turn: intro + first question. Half-duplex means each
	// agent turn is exactly ONE audio clip, so we must not send the greeting and
	// the first question as two separate clips (the second would cut off the first).
	cv.sv.MarkAsked()
	idx, total := cv.sv.Progress()
	// Opening line + first question in ONE clip. The greeting is LLM-authored at
	// poll creation (product-aware, warm); we fall back to a fixed line if it's
	// missing. TTS is streamed sentence-by-sentence, so the first words arrive fast.
	opening := withLead(introLine(cv.poll.Intro, cv.poll.Product), q.Text)
	cv.emit(outMsg{Type: "agent_say", Text: opening, Kind: "question", Index: idx, Total: total}, opening)
}

// greetingQuestion is the small-talk opener the agent asks before the survey.
// It's kept short — a normal human hello — and doubles as the "question" context
// we hand the classifier when reading the reply.
const greetingQuestion = "How's your day going so far?"

// openGreeting speaks a short, human hello — the agent introduces herself by
// name (time-of-day aware) and asks how the person's day is going. It does NOT
// mention the survey or the product yet: a real person says hi and listens
// before getting to business. Touches no survey state.
func (cv *conversation) openGreeting() {
	cv.inGreeting = true
	line := greetingLine(cv.h.AgentName, timeOfDay(time.Now()))
	cv.emit(outMsg{Type: "agent_say", Text: line, Kind: "greeting"}, line)
}

// greetingTemplates are curated spoken openers — just a warm, human hello: the
// agent's name, a time-aware salutation, and a "how are you". No agenda, no
// product: that comes AFTER she's heard how they're doing (see composeGreetingLead),
// the way a person eases into a conversation. We use hand-written templates
// rather than LLM generation because the offline question-gen model (3B) proved
// too weak to self-introduce reliably. %[1]s = agent name, %[2]s = time of day
// ("morning"/"afternoon"/"evening"). Picked at random per session so it varies.
// Keep every variant ending on a "how are you" so the reply is a wellbeing
// answer the classifier can read.
var greetingTemplates = []string{
	"Hi there! I'm %[1]s. How's your %[2]s going so far?",
	"Hey, %[1]s here — good %[2]s! How are you doing today?",
	"Good %[2]s! My name's %[1]s. How's your day treating you?",
	"Hi! I'm %[1]s. How's everything going this %[2]s?",
}

// greetingLine fills a random greeting template with the agent name and the
// current time of day.
func greetingLine(name, tod string) string {
	if name = strings.TrimSpace(name); name == "" {
		name = "Ava"
	}
	return fmt.Sprintf(greetingTemplates[rand.Intn(len(greetingTemplates))], name, tod)
}

// timeOfDay buckets a clock time into a spoken salutation word.
func timeOfDay(t time.Time) string {
	switch h := t.Hour(); {
	case h < 12:
		return "morning"
	case h < 18:
		return "afternoon"
	default:
		return "evening"
	}
}

// handleGreeting reads the reply to the opening small-talk. It first runs the
// turn classifier once to catch an early bail ("actually I don't have time"),
// then authors Ava's spoken reply + hand-off into the survey. It's a single
// exchange — we never loop on the greeting, so the survey starts promptly.
func (cv *conversation) handleGreeting(text string) bool {
	cv.inGreeting = false

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	turn, err := cv.h.LLM.ClassifyTurn(ctx, greetingQuestion, text)
	if err != nil {
		log.Printf("greeting classify error: %v", err)
		turn = llm.Turn{Intent: llm.IntentAnswer}
	}
	cv.qaSignal("greeting", turn)

	// They can bail right at hello ("actually I don't have time").
	if turn.Intent == llm.IntentWantsStop {
		cv.sv.Bail()
		cv.persist()
		cv.speak("No problem at all — thanks so much for the time you gave us. Take care!", "closing")
		cv.finalize(survey.Bailed)
		return true
	}

	// Author a reply that reacts to what they said, frames the survey (count +
	// purpose), and ends by ASKING if they're ready — a human consent beat. We
	// don't ask the first question yet; we wait for their go-ahead (handleStart).
	cv.awaitingStart = true
	lead := cv.composeGreetingLead(text, strings.TrimSpace(turn.Ack))

	// Pace it like the survey turns: the reaction ("Glad to hear it!") is its own
	// beat, then a pause, then the framing + "ready?" — instead of one breath.
	if reaction, framing, ok := splitGreetingBeats(lead); cv.h.Pacing && ok {
		cv.speakTwoBeats(
			outMsg{Type: "agent_say", Text: reaction, Kind: "greeting"}, reaction,
			outMsg{Type: "agent_add", Text: framing, Kind: "greeting"}, framing,
		)
		return false
	}
	cv.speak(lead, "greeting")
	return false
}

// splitGreetingBeats divides the authored greeting reply into a reaction beat and
// a framing beat so they can be delivered as two paced bubbles. It cuts at the
// first sentence that opens with a transition connective ("So,", "Alright,", …)
// — the seam the greeting prompt is built around — so all the reaction lands in
// beat 1 and the framing + "ready?" in beat 2. Falls back to first-sentence /
// rest, and returns ok=false when it can't split cleanly (→ single beat).
func splitGreetingBeats(lead string) (reaction, framing string, ok bool) {
	sents := splitSentences(strings.TrimSpace(lead))
	if len(sents) < 2 {
		return "", "", false
	}
	cut := -1
	for i := 1; i < len(sents); i++ {
		if startsWithTransition(sents[i]) {
			cut = i
			break
		}
	}
	if cut == -1 {
		cut = 1 // no transition marker → first sentence is the reaction
	}
	reaction = strings.TrimSpace(strings.Join(sents[:cut], " "))
	framing = strings.TrimSpace(strings.Join(sents[cut:], " "))
	if reaction == "" || framing == "" {
		return "", "", false
	}
	return reaction, framing, true
}

// startsWithTransition reports whether a sentence opens with a bridging
// connective that marks the pivot from small-talk into the survey framing.
func startsWithTransition(s string) bool {
	s = strings.TrimSpace(s)
	for _, t := range []string{"So", "Alright", "Okay", "OK", "Now", "Right", "Well"} {
		if strings.HasPrefix(s, t+" ") || strings.HasPrefix(s, t+",") || strings.HasPrefix(s, t+" —") {
			return true
		}
	}
	return false
}

// handleStart reads the reply to the "ready?" check. A bail still ends the call;
// anything else is taken as the go-ahead, and we open the survey with a short
// warm lead. Like the greeting, it's a single exchange — no looping.
func (cv *conversation) handleStart(text string) bool {
	cv.awaitingStart = false

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	turn, err := cv.h.LLM.ClassifyTurn(ctx, "Are you ready to start the questions?", text)
	if err != nil {
		log.Printf("start classify error: %v", err)
		turn = llm.Turn{Intent: llm.IntentAnswer}
	}
	cv.qaSignal("start", turn)
	if turn.Intent == llm.IntentWantsStop {
		cv.sv.Bail()
		cv.persist()
		cv.speak("No problem at all — thanks so much for the time you gave us. Take care!", "closing")
		cv.finalize(survey.Bailed)
		return true
	}

	lead := strings.TrimSpace(turn.Ack)
	if lead == "" {
		lead = "Great —"
	}
	return cv.startSurvey(lead)
}

// composeGreetingLead writes Ava's spoken response to the small-talk answer plus
// a natural hand-off into the survey. It uses the Closer completer (the same
// "brain" that authors the closing) so she genuinely engages — answering a
// "how about you?", acknowledging a busy day — instead of steamrolling into the
// questions. Falls back to a warm ack + fixed framing line when no completer is
// wired or the model returns something implausible.
func (cv *conversation) composeGreetingLead(reply, ackFallback string) string {
	product := strings.TrimSpace(cv.poll.Product)
	purpose := strings.TrimSpace(cv.poll.Purpose)
	_, count := cv.sv.Progress() // total number of questions
	if cv.h.Closer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		user := fmt.Sprintf("Time of day: %s.\nThey just said: %q\n\nWrite your spoken reply, ending with the hand-off into the survey.",
			timeOfDay(time.Now()), strings.TrimSpace(reply))
		raw, err := cv.h.Closer.Complete(ctx, greetingReplySystem(cv.h.AgentName, product, purpose, count), user)
		if err != nil {
			log.Printf("greeting response degraded, using fixed framing: %v", err)
		} else if s := sanitizeSpoken(raw, 320); s != "" {
			return s
		}
	}

	// Fallback: a warm ack + a fixed framing line that still sets expectations
	// (how many questions, what it's for) so the survey never starts cold.
	if ackFallback == "" {
		ackFallback = "Thanks — glad you're here."
	}
	return withLead(ackFallback, fixedFraming(product, purpose, count))
}

// fixedFraming is the deterministic "here's what we'll do" line used when no
// completer is wired or the model misbehaves. It states the question count and,
// when given, the survey's purpose, then asks if they're ready to start.
func fixedFraming(product, purpose string, count int) string {
	var b strings.Builder
	b.WriteString("So — I've got ")
	if count > 0 {
		b.WriteString(fmt.Sprintf("%s quick question%s", numberWord(count), plural(count)))
	} else {
		b.WriteString("a few quick questions")
	}
	if product != "" {
		b.WriteString(" about " + product)
	}
	if purpose != "" {
		b.WriteString(", to " + purpose)
	}
	b.WriteString(". Sound good?")
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// numberWord spells small counts for natural speech; larger ones fall back to
// digits.
func numberWord(n int) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	if n >= 0 && n <= 10 {
		return words[n]
	}
	return fmt.Sprintf("%d", n)
}

// greetingReplySystem builds the prompt for Ava's small-talk reply. She must
// react to what they actually said FIRST (reciprocate, acknowledge a busy day),
// then set expectations (how many questions, what it's for) and hand off WITHOUT
// asking the first question herself — the verbatim question is appended after.
func greetingReplySystem(name, product, purpose string, count int) string {
	if name = strings.TrimSpace(name); name == "" {
		name = "Ava"
	}
	about := "the product"
	if product != "" {
		about = product
	}
	countClause := "a few quick questions"
	if count > 0 {
		countClause = fmt.Sprintf("%s question%s", numberWord(count), plural(count))
	}
	purposeClause := "."
	if purpose != "" {
		purposeClause = ", and weave in the goal as a SHORT phrase (compress it, don't recite it word-for-word): " + purpose + "."
	}
	return fmt.Sprintf("You are %[1]s, a warm, personable voice-survey host. You just said hello and asked "+
		"how the person's day is going, and they replied. Write %[1]s's SHORT spoken reply and hand-off. "+
		"This is SPOKEN aloud, so keep it tight and easy to listen to — a wall of text is painful to hear. "+
		"STRUCTURE, in this order: (1) ONE brief, genuine reaction to what they ACTUALLY said — if they "+
		"asked how you are, answer in a few words; if they're busy, a quick nod. Don't overdo it. "+
		"(2) ONE smooth transition into the survey — pick a single connective like \"So,\" and NEVER stack "+
		"two (no \"so... okay, let's get into it\"). (3) In that sentence, say you've got %[3]s about %[2]s%[4]s "+
		"then END BY ASKING IF THEY'RE READY to start (e.g. \"sound good?\", \"ready when you are?\"). "+
		"HARD LIMITS: at most 2 short sentences plus the ready-check, under 35 words total, and never "+
		"repeat a word like \"quick\" twice. Do NOT ask any of the actual survey questions and do NOT start "+
		"answering them — only check they're ready. Natural spoken English, contractions welcome. No "+
		"lists, no emoji, no placeholders, no stage directions. Output only the spoken words.", name, about, countClause, purposeClause)
}

// startSurvey speaks the first question, led by the authored greeting response
// (which already reacted and framed the survey — no second "hi").
func (cv *conversation) startSurvey(lead string) bool {
	q, ok := cv.sv.Current()
	if !ok {
		return cv.finishCompleted(lead)
	}
	cv.sv.MarkAsked()
	idx, total := cv.sv.Progress()
	cv.speakPaced(lead, q.Text, idx, total)
	return false
}

// sanitizeSpoken trims a model-authored spoken line and rejects (returns "")
// anything implausible — empty or too long — so a misbehaving model can never
// speak junk. Surrounding quotes are stripped and stray newlines collapsed.
// Unlike sanitizeClosing it tolerates a question mark (Ava may reciprocate).
func sanitizeSpoken(raw string, maxRunes int) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, `"'`)
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || len([]rune(s)) > maxRunes {
		return ""
	}
	return s
}

// ---- transport helpers ----

func (cv *conversation) speakQ(text string, idx, total int) {
	cv.emit(outMsg{Type: "agent_say", Text: text, Kind: "question", Index: idx, Total: total}, text)
}

func (cv *conversation) speak(text, kind string) {
	cv.emit(outMsg{Type: "agent_say", Text: text, Kind: kind}, text)
}

// emit sends the control frame, then STREAMS the audio sentence-by-sentence:
// each sentence is synthesized and sent as its own binary frame so the client
// can start playing the first words while later sentences are still rendering.
// A trailing tts_end tells the client the turn's audio is complete.
// Sets speaking=true so the silence timer stays paused until playback_done.
func (cv *conversation) emit(msg outMsg, ttsText string) {
	cv.speaking = true
	cv.send(msg)
	cv.streamTTS(ttsText)
	cv.send(outMsg{Type: "tts_end"})
}

// streamTTS synthesizes text sentence-by-sentence and writes each as its own
// binary frame. It does NOT bracket the turn (no tts_end) — callers that build
// multi-beat turns (speakPaced) stream several segments before one tts_end.
func (cv *conversation) streamTTS(text string) {
	for _, chunk := range splitSentences(text) {
		wav, err := cv.h.Speech.Synthesize(chunk)
		if err != nil {
			log.Printf("tts error: %v", err)
			continue
		}
		if err := cv.c.WriteMessage(websocket.BinaryMessage, wav); err != nil {
			cv.speaking = false
			return
		}
	}
}

// pacingPauseMS is the beat between the acknowledgment and the question. The
// research band is ~250–500ms: long enough to read as a natural breath, short
// enough to stay well under the ~700ms mark where a silence starts to signal a
// dispreferred/negative response.
const pacingPauseMS = 400

// speakPaced delivers a question as TWO beats — a short acknowledgment, a brief
// pause, then the question — so the agent connects to the next question like a
// person instead of reading ack+question in one breath. The whole thing is ONE
// turn from the client's view: a single tts_end at the end, so the mic re-arms
// only after both beats drain (never mid-turn). The pause is a silent-PCM buffer
// (Kokoro has no SSML). An empty ack — or pacing disabled — collapses to a
// single beat with no pause, so weak-model/no-ack turns stay clean.
func (cv *conversation) speakPaced(ack, question string, idx, total int) {
	ack = strings.TrimSpace(ack)
	if !cv.h.Pacing || ack == "" {
		cv.speakQ(withLead(ack, question), idx, total)
		return
	}
	// Beat 1 = the acknowledgment (its own bubble, no progress); beat 2 = the
	// question (second bubble, carries the progress so the bar advances there).
	cv.speakTwoBeats(
		outMsg{Type: "agent_say", Text: ack, Kind: "ack"}, ack,
		outMsg{Type: "agent_add", Text: question, Kind: "question", Index: idx, Total: total}, question,
	)
}

// speakTwoBeats delivers a turn as two spoken beats with a pause between, as ONE
// turn (a single trailing tts_end) so the mic re-arms only after both beats
// drain — never mid-turn. The two control frames let each beat set its own
// kind/progress. The pause is a silent-PCM buffer (Kokoro has no SSML), inside
// the same continuous playback the client already handles.
func (cv *conversation) speakTwoBeats(firstMsg outMsg, firstText string, secondMsg outMsg, secondText string) {
	cv.speaking = true
	cv.send(firstMsg)
	cv.streamTTS(firstText)
	if wav := cv.h.Speech.Silence(pacingPauseMS); len(wav) > 0 {
		if err := cv.c.WriteMessage(websocket.BinaryMessage, wav); err != nil {
			cv.speaking = false
			return
		}
	}
	cv.send(secondMsg)
	cv.streamTTS(secondText)
	cv.send(outMsg{Type: "tts_end"})
}

// splitSentences breaks text at sentence terminators so TTS can stream. The
// first (often short) chunk renders fast, cutting time-to-first-audio.
func splitSentences(text string) []string {
	var out []string
	var b strings.Builder
	for _, r := range text {
		b.WriteRune(r)
		if r == '.' || r == '!' || r == '?' {
			if s := strings.TrimSpace(b.String()); s != "" {
				out = append(out, s)
			}
			b.Reset()
		}
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

func (cv *conversation) send(msg outMsg) {
	if err := cv.c.WriteJSON(msg); err != nil {
		log.Printf("write error: %v", err)
	}
}

// qaSignal mirrors a per-turn classifier decision to the client, but ONLY in QA
// mode (-qa). The browser E2E harness collects these ("qa_intent" frames) so
// persona tests can assert on the intents that actually fired — needs_help,
// wants_stop, and so on — rather than eyeballing the transcript. `phase` marks
// where in the flow the turn was classified ("greeting", "start", "survey").
// Never emitted in production, where cv.h.QA is false.
func (cv *conversation) qaSignal(phase string, turn llm.Turn) {
	if !cv.h.QA {
		return
	}
	cv.send(outMsg{
		Type:       "qa_intent",
		Kind:       phase,
		Intent:     string(turn.Intent),
		Clarity:    string(turn.Clarity),
		Sufficient: turn.Sufficient,
	})
}

func (cv *conversation) finalize(reason survey.EndReason) {
	now := time.Now()
	cv.poll.EndReason = reason
	cv.poll.EndedAt = &now
	cv.h.Store.Save(cv.poll)
	cv.send(outMsg{Type: "done", Reason: string(reason)})
}

func (cv *conversation) persist() { cv.h.Store.Save(cv.poll) }

// readLoop pumps websocket frames into the event channel.
func (cv *conversation) readLoop(events chan<- event) {
	defer close(events)
	for {
		mt, data, err := cv.c.ReadMessage()
		if err != nil {
			events <- event{kind: evClosed}
			return
		}
		if mt == websocket.BinaryMessage {
			events <- event{kind: evUtterance, pcm: data}
			continue
		}
		var m struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "playback_done":
			events <- event{kind: evPlaybackDone}
		case "barge_in":
			events <- event{kind: evBargeIn}
		case "speaking":
			events <- event{kind: evUserSpeaking}
		case "ready":
			// no-op: we already greeted on connect
		}
	}
}

// repairPrompt confirms an understood-but-unclear answer by echoing the
// respondent's own transcribed words — natural conversational repair. We echo
// their words (not a decoded guess) so it works on any model and invites a
// correction if we misheard.
func repairPrompt(heard string) string {
	heard = strings.TrimSpace(heard)
	return "Sorry, I want to make sure I got that right — you said “" + heard +
		"”. Did I understand you correctly, or could you say it another way?"
}

// isAffirmation reports whether a repair reply confirms the original answer (so
// we keep it) rather than correcting it (so we record the new text). The
// discriminator is the FIRST word: a yes-token affirms (even if elaborated,
// "yeah, that's what I meant"); a negation or a fresh restatement corrects.
func isAffirmation(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, ".!,? ")
	if s == "" {
		return false
	}
	// Multi-word confirmations that don't start with a yes-token.
	switch s {
	case "that's right", "thats right", "that's it", "thats it",
		"uh huh", "uhhuh", "mhm", "mm hmm", "mmhmm":
		return true
	}
	switch firstWord(s) {
	case "no", "nope", "nah", "not", "actually", "wait", "instead":
		return false // explicit correction
	case "yes", "yeah", "yep", "yup", "right", "correct", "exactly", "sure", "ok", "okay":
		return true // affirmation, possibly elaborated
	}
	return false // a restatement with no yes-token → treat as a correction
}

// firstWord returns the leading run of letters/digits (lowercased input).
func firstWord(s string) string {
	for i, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return s[:i]
		}
	}
	return s
}

// followUpPrompt builds the agent's line when a reply wasn't a usable answer.
// For an off-topic aside we lead with the classifier's acknowledgment (a warm,
// specific steer-back like "Ha, no worries —") and re-pose the question, instead
// of a robotic "let me ask again". Falls back to a neutral redirect if the model
// gave no ack.
func followUpPrompt(intent llm.Intent, ack, question string) string {
	switch intent {
	case llm.IntentOffTopic:
		if lead := strings.TrimSpace(ack); lead != "" {
			return lead + " " + question
		}
		return "No problem — back to it: " + question
	case llm.IntentUnintellig:
		return "Sorry, I didn't quite catch that. Here's the question again: " + question
	default: // a vague but on-topic answer
		return "Could you tell me a little more about that?"
	}
}

// helpPrompt builds the spoken turn when the respondent needs help answering.
// The classifier's ack carries a warm, question-specific reassurance/hint; we
// lead with it and re-pose the question so they always hear the question again.
// If the model gave no ack (weaker models under-produce), a neutral reassurance
// keeps the turn helpful rather than a bare re-read.
func helpPrompt(ack, question string) string {
	if lead := strings.TrimSpace(ack); lead != "" {
		return lead + " " + question
	}
	return "However you'd like to answer is totally fine — just your honest take. " + question
}

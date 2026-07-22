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
	"log"
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
}

// outMsg is a server->client control frame.
type outMsg struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Index  int    `json:"index,omitempty"`
	Total  int    `json:"total,omitempty"`
	Reason string `json:"reason,omitempty"`
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

	speaking bool // true while we expect the client to be playing audio
	strikes  int  // consecutive silence nudges
	reasks   int  // times we've re-read the current question

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

	// Opening line + first question.
	cv.greetAndAskFirst()

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
	cv.speakQ(withLead(lead, q.Text), idx, total)
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
	for _, chunk := range splitSentences(ttsText) {
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

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
	"time"

	"github.com/gorilla/websocket"

	"voicesurvey/internal/llm"
	"voicesurvey/internal/session"
	"voicesurvey/internal/speech"
	"voicesurvey/internal/survey"
)

// Tuning knobs for turn-taking timeouts.
const (
	inputSampleRate = 16000           // browser captures/sends 16kHz mono
	silenceWindow   = 9 * time.Second // how long to wait for a reply before nudging
	maxSilenceStrikes = 2             // nudges before ending on silence
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true }, // PoC: allow any origin
}

// Handler wires the shared engines into the websocket route.
type Handler struct {
	Store  *session.Store
	Speech *speech.Engine
	LLM    *llm.Client
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
}

func (cv *conversation) run() {
	events := make(chan event, 8)
	go cv.readLoop(events)

	// Opening line + first question.
	cv.greetAndAskFirst()

	timer := time.NewTimer(silenceWindow)
	timer.Stop() // only armed while listening
	defer timer.Stop()

	arm := func() {
		if !cv.speaking {
			timer.Reset(silenceWindow)
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
				timer.Reset(silenceWindow)

			case evBargeIn:
				// User talked over the agent: tell client to stop playback,
				// then listen. (Phase 5; harmless if it fires otherwise.)
				cv.send(outMsg{Type: "cancel"})
				cv.speaking = false
				timer.Reset(silenceWindow)

			case evUtterance:
				timer.Stop()
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
		return cv.finishCompleted()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	turn, err := cv.h.LLM.ClassifyTurn(ctx, q.Text, text)
	if err != nil {
		log.Printf("classify error: %v", err)
		turn = llm.Turn{Intent: llm.IntentAnswer, Sufficient: true} // keep moving
	}

	// Early bail-out (Phase 4): respondent wants to stop.
	if turn.Intent == llm.IntentWantsStop {
		cv.sv.Bail()
		cv.persist()
		cv.speak("No problem at all — thanks so much for the time you gave us. Take care!", "closing")
		cv.finalize(survey.Bailed)
		return true
	}

	// A sufficient, on-topic answer: capture and advance.
	if turn.Intent == llm.IntentAnswer && turn.Sufficient {
		cv.sv.RecordAnswer(text)
		cv.persist()
		return cv.askNextOrFinish()
	}

	// Weak / off-topic / unintelligible: probe once, then move on.
	if cv.sv.FollowUp() {
		cv.speak(followUpPrompt(turn.Intent), "followup")
		return false
	}
	// Follow-up budget spent: capture whatever we got and advance.
	cv.sv.CaptureAndAdvance(text)
	cv.persist()
	return cv.askNextOrFinish()
}

func (cv *conversation) askNextOrFinish() bool {
	if done, reason := cv.sv.Done(); done {
		if reason == survey.Completed {
			return cv.finishCompleted()
		}
	}
	q, ok := cv.sv.Current()
	if !ok {
		return cv.finishCompleted()
	}
	cv.sv.MarkAsked()
	idx, total := cv.sv.Progress()
	cv.speakQ(q.Text, idx, total)
	return false
}

func (cv *conversation) finishCompleted() bool {
	cv.speak("That's everything I wanted to ask. Thank you so much for sharing your thoughts — it really helps. Goodbye!", "closing")
	cv.finalize(survey.Completed)
	return true
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
		cv.finishCompleted()
		return
	}
	// One combined opening turn: intro + first question. Half-duplex means each
	// agent turn is exactly ONE audio clip, so we must not send the greeting and
	// the first question as two separate clips (the second would cut off the first).
	cv.sv.MarkAsked()
	idx, total := cv.sv.Progress()
	intro := "Hi! Thanks for taking a moment. I've got a few quick questions about " +
		cv.poll.Product + ". There are no wrong answers — just share whatever comes to mind. " +
		"Here's my first question: " + q.Text
	cv.emit(outMsg{Type: "agent_say", Text: intro, Kind: "question", Index: idx, Total: total}, intro)
}

// ---- transport helpers ----

func (cv *conversation) speakQ(text string, idx, total int) {
	cv.emit(outMsg{Type: "agent_say", Text: text, Kind: "question", Index: idx, Total: total}, text)
}

func (cv *conversation) speak(text, kind string) {
	cv.emit(outMsg{Type: "agent_say", Text: text, Kind: kind}, text)
}

// emit sends the control frame, synthesizes the audio, and sends it as a binary
// frame. Sets speaking=true so the silence timer stays paused until playback_done.
func (cv *conversation) emit(msg outMsg, ttsText string) {
	cv.speaking = true
	cv.send(msg)
	wav, err := cv.h.Speech.Synthesize(ttsText)
	if err != nil {
		log.Printf("tts error: %v", err)
		cv.speaking = false
		return
	}
	if err := cv.c.WriteMessage(websocket.BinaryMessage, wav); err != nil {
		cv.speaking = false
	}
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
		case "ready":
			// no-op: we already greeted on connect
		}
	}
}

func followUpPrompt(intent llm.Intent) string {
	switch intent {
	case llm.IntentOffTopic:
		return "Got it — and coming back to my question, what are your thoughts?"
	case llm.IntentUnintellig:
		return "Sorry, I didn't quite catch that. Could you say it again?"
	default:
		return "Could you tell me a little more about that?"
	}
}

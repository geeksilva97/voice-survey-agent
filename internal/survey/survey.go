// Package survey is the slot state machine that decides the conversation flow
// and, crucially, when it ends. Per the research thesis, the state machine —
// not the LLM's gut feeling — owns termination: the survey is done when every
// question slot is filled or skipped.
package survey

import "strings"

// Status of a single question slot.
type Status string

const (
	Unasked  Status = "unasked"
	Asked    Status = "asked"
	Answered Status = "answered"
	Skipped  Status = "skipped"
)

// Question is one slot in the survey.
type Question struct {
	Text   string `json:"text"`
	Status Status `json:"status"`
	Answer string `json:"answer"`
}

// EndReason explains why the survey terminated.
type EndReason string

const (
	NotEnded  EndReason = ""
	Completed EndReason = "completed"
	Bailed    EndReason = "bailed"
	Silence   EndReason = "silence"
)

// maxFollowUps caps clarifying probes per question so the agent can never loop
// forever on a vague answer (the classic conversational-survey failure mode).
const maxFollowUps = 1

// Survey holds the ordered slots and cursor state for one respondent.
type Survey struct {
	Questions []Question `json:"questions"`
	idx       int        // index of the current question
	followUps int        // follow-ups spent on the current question
	end       EndReason
}

// New builds a survey from generated question texts.
func New(questions []string) *Survey {
	qs := make([]Question, 0, len(questions))
	for _, q := range questions {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		qs = append(qs, Question{Text: q, Status: Unasked})
	}
	return &Survey{Questions: qs}
}

// Current returns the question awaiting an answer, or false if none remain.
func (s *Survey) Current() (*Question, bool) {
	if s.idx < 0 || s.idx >= len(s.Questions) {
		return nil, false
	}
	return &s.Questions[s.idx], true
}

// MarkAsked flags the current question as spoken to the respondent.
func (s *Survey) MarkAsked() {
	if q, ok := s.Current(); ok && q.Status == Unasked {
		q.Status = Asked
	}
}

// RecordAnswer stores a sufficient answer and advances to the next slot.
func (s *Survey) RecordAnswer(answer string) {
	if q, ok := s.Current(); ok {
		q.Answer = strings.TrimSpace(answer)
		q.Status = Answered
	}
	s.advance()
}

// CanFollowUp reports whether another clarifying probe is allowed on the
// current question (i.e. we haven't hit the cap yet).
func (s *Survey) CanFollowUp() bool { return s.followUps < maxFollowUps }

// FollowUp consumes one probe on the current question. Returns false if the
// cap is already reached, in which case the caller should capture whatever was
// said and move on rather than probe again.
func (s *Survey) FollowUp() bool {
	if !s.CanFollowUp() {
		return false
	}
	s.followUps++
	return true
}

// CaptureAndAdvance stores a (possibly weak) answer and moves on. Used when the
// follow-up cap is hit — better to move on than loop.
func (s *Survey) CaptureAndAdvance(answer string) {
	if q, ok := s.Current(); ok {
		if a := strings.TrimSpace(answer); a != "" {
			q.Answer = a
			q.Status = Answered
		} else {
			q.Status = Skipped
		}
	}
	s.advance()
}

// TryFillOther lets a volunteered answer fill a *different* upcoming question
// (respondent answered ahead). Returns true if it matched and filled a slot.
// The matcher is supplied by the caller (e.g. an LLM or keyword check).
func (s *Survey) TryFillOther(match func(question string) (answer string, ok bool)) bool {
	filled := false
	for i := range s.Questions {
		if i == s.idx || s.Questions[i].Status == Answered {
			continue
		}
		if ans, ok := match(s.Questions[i].Text); ok {
			s.Questions[i].Answer = strings.TrimSpace(ans)
			s.Questions[i].Status = Answered
			filled = true
		}
	}
	return filled
}

// Bail terminates the survey early because the respondent wants to stop.
func (s *Survey) Bail() { s.end = Bailed }

// TimeOut terminates the survey because the respondent went silent.
func (s *Survey) TimeOut() { s.end = Silence }

// Done reports whether the survey has ended and why.
func (s *Survey) Done() (bool, EndReason) {
	if s.end != NotEnded {
		return true, s.end
	}
	if s.remaining() == 0 {
		return true, Completed
	}
	return false, NotEnded
}

// Progress returns (current 1-based index, total) for UI display.
func (s *Survey) Progress() (int, int) {
	return s.idx + 1, len(s.Questions)
}

// advance moves the cursor to the next slot that still needs an answer.
func (s *Survey) advance() {
	s.followUps = 0
	for s.idx < len(s.Questions) {
		s.idx++
		if s.idx < len(s.Questions) && s.Questions[s.idx].Status != Answered && s.Questions[s.idx].Status != Skipped {
			return
		}
	}
}

// remaining counts slots still needing an answer.
func (s *Survey) remaining() int {
	n := 0
	for _, q := range s.Questions {
		if q.Status != Answered && q.Status != Skipped {
			n++
		}
	}
	return n
}

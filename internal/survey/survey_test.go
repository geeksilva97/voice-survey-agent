package survey

import "testing"

func TestHappyPathCompletes(t *testing.T) {
	s := New([]string{"Q1", "Q2", "Q3"})
	if done, _ := s.Done(); done {
		t.Fatal("should not be done at start")
	}
	for i := 0; i < 3; i++ {
		q, ok := s.Current()
		if !ok {
			t.Fatalf("expected a current question at step %d", i)
		}
		s.MarkAsked()
		s.RecordAnswer("answer for " + q.Text)
	}
	done, reason := s.Done()
	if !done || reason != Completed {
		t.Fatalf("expected completed, got done=%v reason=%q", done, reason)
	}
	for _, q := range s.Questions {
		if q.Status != Answered || q.Answer == "" {
			t.Fatalf("question %q not answered: %+v", q.Text, q)
		}
	}
}

func TestFollowUpCapThenAdvance(t *testing.T) {
	s := New([]string{"Q1", "Q2"})
	// First question: one follow-up allowed, then must advance on capture.
	if !s.CanFollowUp() {
		t.Fatal("should allow a follow-up initially")
	}
	if !s.FollowUp() {
		t.Fatal("first follow-up should succeed")
	}
	if s.CanFollowUp() {
		t.Fatal("second follow-up should be denied (cap=1)")
	}
	s.CaptureAndAdvance("weak answer")
	if q, _ := s.Current(); q == nil || q.Text != "Q2" {
		t.Fatalf("expected to advance to Q2, got %+v", q)
	}
	// Follow-up budget must reset on the new question.
	if !s.CanFollowUp() {
		t.Fatal("follow-up budget should reset after advancing")
	}
}

func TestBailAndSilenceEnd(t *testing.T) {
	s := New([]string{"Q1", "Q2"})
	s.Bail()
	if done, reason := s.Done(); !done || reason != Bailed {
		t.Fatalf("expected bailed, got done=%v reason=%q", done, reason)
	}

	s2 := New([]string{"Q1"})
	s2.TimeOut()
	if done, reason := s2.Done(); !done || reason != Silence {
		t.Fatalf("expected silence, got done=%v reason=%q", done, reason)
	}
}

// Fill is the agent-loop driver's entry point: the model names the slot, so a
// single reply can close several questions and the cursor must keep up.
func TestFillByIndexAdvancesCursor(t *testing.T) {
	s := New([]string{"scent", "price", "packaging"})

	// One reply answers Q1 and Q3 at once — the thing the classifier path cannot
	// express, because its verdict is always about the current question.
	if !s.Fill(0, "love the lavender") {
		t.Fatal("Fill(0) should succeed")
	}
	if !s.Fill(2, "nicer boxes") {
		t.Fatal("Fill(2) should succeed")
	}
	// Cursor was on Q1; filling it must move to the next slot that still needs an
	// answer — Q2 — not to the already-filled Q3.
	if q, _ := s.Current(); q == nil || q.Text != "price" {
		t.Fatalf("expected cursor on 'price', got %+v", q)
	}
	if s.Questions[2].Status != Answered || s.Questions[2].Answer != "nicer boxes" {
		t.Fatalf("out-of-order fill did not stick: %+v", s.Questions[2])
	}

	// An empty answer records honest absence, not a hollow answer.
	s.Fill(1, "   ")
	if got := s.Questions[1].Status; got != Skipped {
		t.Fatalf("empty answer should mark Skipped, got %q", got)
	}
	if s.Questions[1].Answer != "" {
		t.Fatalf("skipped slot should carry no answer, got %q", s.Questions[1].Answer)
	}
	if done, reason := s.Done(); !done || reason != Completed {
		t.Fatalf("all slots resolved should complete, got done=%v reason=%q", done, reason)
	}

	// Out-of-range is refused rather than panicking — the index comes from a model.
	if s.Fill(-1, "x") || s.Fill(99, "x") {
		t.Fatal("out-of-range Fill should return false")
	}
}

func TestVolunteeredAnswerFillsOtherSlot(t *testing.T) {
	s := New([]string{"scent", "price", "packaging"})
	// While on Q1 (scent), respondent also volunteers a price opinion.
	filled := s.TryFillOther(func(q string) (string, bool) {
		if q == "price" {
			return "a bit expensive", true
		}
		return "", false
	})
	if !filled {
		t.Fatal("expected the price slot to be filled")
	}
	// Answer Q1, advance should skip the already-filled price slot -> packaging.
	s.RecordAnswer("love the lavender")
	if q, _ := s.Current(); q == nil || q.Text != "packaging" {
		t.Fatalf("expected to skip filled 'price' and land on 'packaging', got %+v", q)
	}
}

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

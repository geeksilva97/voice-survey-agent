package obs

import (
	"context"
	"errors"
	"testing"
	"time"

	"voicesurvey/internal/llm"
)

// Init without credentials must be a clean no-op: disabled, no error, and a
// callable shutdown — the PoC stays fully offline when Langfuse isn't set up.
func TestInitDisabledWithoutKeys(t *testing.T) {
	t.Setenv("LANGFUSE_PUBLIC_KEY", "")
	t.Setenv("LANGFUSE_SECRET_KEY", "")
	shutdown, enabled, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if enabled {
		t.Fatal("tracing enabled without credentials")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown: %v", err)
	}
}

// Init must actually SUCCEED when credentials are present — building the
// exporter, resource and provider for real. The disabled-path test above passes
// even when Init is broken, which is how a resource schema-URL conflict once
// slipped through: it made Init fail on every credentialed start.
func TestInitEnabledWithKeys(t *testing.T) {
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-lf-test")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-lf-test")
	t.Setenv("LANGFUSE_HOST", "http://127.0.0.1:1") // never dialed: export is async
	shutdown, enabled, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init with credentials: %v", err)
	}
	if !enabled {
		t.Fatal("Init reported disabled despite credentials")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(ctx)
		provider = nil
	})

	// With a provider installed, spans must carry a real trace id — that id is
	// what links an eval case to its Langfuse dataset run item.
	var ref TraceRef
	ctx := WithTraceRef(context.Background(), &ref)
	c := TraceClassifier(stubClassifier{turn: llm.Turn{Intent: llm.IntentAnswer}}, "m")
	if _, err := c.ClassifyTurn(ctx, "Q?", "R."); err != nil {
		t.Fatalf("ClassifyTurn: %v", err)
	}
	if len(ref.ID()) != 32 {
		t.Errorf("captured trace id = %q, want a 32-char hex id", ref.ID())
	}
}

// Flush is a no-op with tracing off and must not panic on a nil provider.
func TestFlushWithoutProvider(t *testing.T) {
	provider = nil
	if err := Flush(context.Background()); err != nil {
		t.Fatalf("Flush with no provider: %v", err)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	ctx := WithSession(context.Background(), "poll-123")
	if got := sessionID(ctx); got != "poll-123" {
		t.Fatalf("sessionID = %q, want %q", got, "poll-123")
	}
	if got := sessionID(context.Background()); got != "" {
		t.Fatalf("sessionID on untagged ctx = %q, want empty", got)
	}
}

type stubClassifier struct {
	turn llm.Turn
	err  error
}

func (s stubClassifier) ClassifyTurn(context.Context, string, string) (llm.Turn, error) {
	return s.turn, s.err
}

// The wrapper must be a pure pass-through for both the Turn and the error,
// even when no tracer provider is installed (noop global tracer).
func TestTraceClassifierPassThrough(t *testing.T) {
	want := llm.Turn{Intent: llm.IntentAnswer, Sufficient: true, Clarity: llm.ClarityClear, Ack: "Got it."}
	c := TraceClassifier(stubClassifier{turn: want}, "test-model")
	got, err := c.ClassifyTurn(WithSession(context.Background(), "s1"), "Q?", "R.")
	if err != nil {
		t.Fatalf("ClassifyTurn: %v", err)
	}
	if got != want {
		t.Fatalf("Turn = %+v, want %+v", got, want)
	}

	wantErr := errors.New("boom")
	c = TraceClassifier(stubClassifier{err: wantErr}, "test-model")
	if _, err := c.ClassifyTurn(context.Background(), "Q?", "R."); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

type stubCompleter struct {
	out string
	err error
}

func (s stubCompleter) Complete(context.Context, string, string) (string, error) {
	return s.out, s.err
}

// Like the classifier wrapper, the completer wrapper must not alter the text or
// the error — the greeting reply and closing line are user-facing speech.
func TestTraceCompleterPassThrough(t *testing.T) {
	c := TraceCompleter(stubCompleter{out: "Thanks so much!"}, "m")
	got, err := c.Complete(WithLabel(context.Background(), "closing_line"), "sys", "usr")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "Thanks so much!" {
		t.Errorf("output = %q, want %q", got, "Thanks so much!")
	}

	wantErr := errors.New("upstream down")
	c = TraceCompleter(stubCompleter{err: wantErr}, "m")
	if _, err := c.Complete(context.Background(), "sys", "usr"); !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want %v", err, wantErr)
	}
}

// The label names the span, so one shared Completer doing several jobs stays
// distinguishable in Langfuse; absent a label it falls back to a generic name.
func TestLabel(t *testing.T) {
	if got := label(WithLabel(context.Background(), "closing_line"), "completion"); got != "closing_line" {
		t.Errorf("label = %q, want closing_line", got)
	}
	if got := label(context.Background(), "completion"); got != "completion" {
		t.Errorf("label fallback = %q, want completion", got)
	}
}

// StartOp must be usable with tracing off: every method is a no-op and nothing
// panics, so the speech call sites need no conditionals.
func TestStartOpNoTracing(t *testing.T) {
	provider = nil
	op := StartOp(context.Background(), "stt")
	op.In("hello").Out("hello").Bytes("audio.pcm_bytes", 1234).Fail(nil).End()
	op2 := StartOp(WithSession(context.Background(), "s1"), "tts")
	op2.Fail(errors.New("synth failed")).End()
}

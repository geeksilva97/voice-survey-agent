// Package obs is the optional observability layer: OpenTelemetry traces
// exported to Langfuse over OTLP/HTTP. Langfuse has no Go SDK; its official
// path for Go is exactly this — the standard OTel SDK pointed at Langfuse's
// /api/public/otel endpoint, authenticated with Basic auth built from the
// project's public/secret key pair.
//
// Tracing is enabled only when both LANGFUSE_PUBLIC_KEY and
// LANGFUSE_SECRET_KEY are set (LANGFUSE_HOST optional, defaults to Langfuse
// Cloud). Absent keys mean everything here is a no-op, so the PoC stays fully
// offline-capable. The secret key is never logged.
package obs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"voicesurvey/internal/llm"
	"voicesurvey/internal/prompt"
)

const defaultHost = "https://cloud.langfuse.com"

// Host returns the Langfuse base URL traces are sent to (for logging).
func Host() string {
	if h := strings.TrimSpace(os.Getenv("LANGFUSE_HOST")); h != "" {
		return strings.TrimRight(h, "/")
	}
	return defaultHost
}

// Init wires the global OTel tracer provider to Langfuse when credentials are
// present. Returns a shutdown func (flushes pending spans) and whether tracing
// is enabled. With no credentials it returns a no-op shutdown and false — the
// global tracer stays a noop and instrumented code costs nothing.
func Init(ctx context.Context) (shutdown func(context.Context) error, enabled bool, err error) {
	noop := func(context.Context) error { return nil }
	pk := strings.TrimSpace(os.Getenv("LANGFUSE_PUBLIC_KEY"))
	sk := strings.TrimSpace(os.Getenv("LANGFUSE_SECRET_KEY"))
	if pk == "" || sk == "" {
		return noop, false, nil
	}

	auth := base64.StdEncoding.EncodeToString([]byte(pk + ":" + sk))
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(Host()+"/api/public/otel/v1/traces"),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": "Basic " + auth,
			// Langfuse's current OTLP ingestion contract; without it the endpoint
			// falls back to older mapping behavior.
			"x-langfuse-ingestion-version": "4",
		}),
	)
	if err != nil {
		return noop, false, err
	}

	// NewSchemaless, not NewWithAttributes: pinning our own semconv schema URL
	// conflicts with whatever the SDK's default resource carries, and Merge fails
	// the whole init on a schema mismatch. Schemaless attributes merge cleanly and
	// survive SDK upgrades.
	res, err := sdkresource.Merge(sdkresource.Default(),
		sdkresource.NewSchemaless(semconv.ServiceName("voicesurvey")))
	if err != nil {
		return noop, false, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	provider = tp
	return tp.Shutdown, true, nil
}

// provider is kept so Flush can force an export without tearing the pipeline
// down. Set once by Init; nil means tracing is off.
var provider *sdktrace.TracerProvider

// Flush blocks until buffered spans have been handed to Langfuse. Needed before
// linking traces to dataset run items: the batch exporter is asynchronous, and
// the run-item API rejects a trace it hasn't ingested yet. No-op when tracing is
// off.
func Flush(ctx context.Context) error {
	if provider == nil {
		return nil
	}
	return provider.ForceFlush(ctx)
}

// TraceRef captures the trace id a traced call produced, so the caller can link
// that trace to something else (a dataset run item) afterwards. Attach one per
// call with WithTraceRef.
type TraceRef struct{ id string }

// ID is the captured trace id, or "" if the call wasn't traced.
func (r *TraceRef) ID() string {
	if r == nil {
		return ""
	}
	return r.id
}

type traceRefKey struct{}

// WithTraceRef arranges for the next traced call on this context to record its
// trace id into ref. One ref per call — it holds a single id, and the eval gives
// each case its own.
func WithTraceRef(ctx context.Context, ref *TraceRef) context.Context {
	return context.WithValue(ctx, traceRefKey{}, ref)
}

func traceRef(ctx context.Context) *TraceRef {
	r, _ := ctx.Value(traceRefKey{}).(*TraceRef)
	return r
}

// sessionKey carries the conversation's session id through a context so spans
// created anywhere in the turn can be grouped per conversation in Langfuse.
type sessionKey struct{}

// WithSession tags a context with the conversation's session id.
func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}

func sessionID(ctx context.Context) string {
	id, _ := ctx.Value(sessionKey{}).(string)
	return id
}

// labelKey carries a human-readable name for the next traced completion, so one
// shared Completer used for several jobs (the Closer authors both the greeting
// reply and the closing line) doesn't collapse into one indistinguishable span.
type labelKey struct{}

// WithLabel names the operation the next traced call performs. Without it a
// completion span is just "completion".
func WithLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, labelKey{}, label)
}

func label(ctx context.Context, fallback string) string {
	if l, _ := ctx.Value(labelKey{}).(string); l != "" {
		return l
	}
	return fallback
}

// promptKey carries the prompt driving the next traced call. The classifier
// stamps its own directly (it has exactly one prompt); completions are generic,
// so the call site supplies theirs.
type promptKey struct{}

// WithPrompt records WHICH prompt produced the next traced completion. Without
// it a prompt edit is invisible in Langfuse: old and new output land in one
// undifferentiated pile with no axis to split them.
func WithPrompt(ctx context.Context, r prompt.Resolved) context.Context {
	return context.WithValue(ctx, promptKey{}, r)
}

func tracedPrompt(ctx context.Context) (prompt.Resolved, bool) {
	r, ok := ctx.Value(promptKey{}).(prompt.Resolved)
	return r, ok
}

// setPromptAttrs records the prompt's identity on a generation span, two ways.
//
// The fingerprint always goes on as metadata: it is the only identity that exists
// in ModeCode, and it is content-addressed, so it can't drift. When the prompt
// came from Langfuse we ALSO set the native link attributes, which is what makes
// the generation show up attached to the managed prompt in the UI (and gives the
// per-version metrics for free). Prompt linking only works on generation-type
// observations, which every call site here is.
func setPromptAttrs(span trace.Span, r prompt.Resolved) {
	span.SetAttributes(
		attribute.String("langfuse.trace.metadata.prompt_name", r.Name),
		attribute.String("langfuse.trace.metadata.prompt_source", r.Source),
		attribute.String("langfuse.trace.metadata.prompt_version", r.Fingerprint()),
	)
	if r.Version > 0 {
		span.SetAttributes(
			attribute.String("langfuse.observation.prompt.name", r.Name),
			attribute.Int("langfuse.observation.prompt.version", r.Version),
		)
	}
}

// TraceCompleter wraps an llm.Completer so one-shot generations (the greeting
// reply, the closing sign-off, the results insight pass) show up as generations
// in Langfuse. Like TraceClassifier it is a pure pass-through: the text and the
// error are returned untouched. Name each call site with WithLabel.
func TraceCompleter(inner llm.Completer, model string) llm.Completer {
	return &tracedCompleter{inner: inner, model: model}
}

type tracedCompleter struct {
	inner llm.Completer
	model string
}

func (t *tracedCompleter) Complete(ctx context.Context, system, user string) (string, error) {
	name := label(ctx, "completion")
	ctx, span := otel.Tracer("voicesurvey").Start(ctx, name)
	defer span.End()
	if ref := traceRef(ctx); ref != nil {
		if sc := span.SpanContext(); sc.HasTraceID() {
			ref.id = sc.TraceID().String()
		}
	}

	input, _ := json.Marshal(map[string]string{"system": system, "user": user})
	span.SetAttributes(
		attribute.String("gen_ai.request.model", t.model),
		attribute.String("langfuse.observation.type", "generation"),
		attribute.String("langfuse.observation.model.name", t.model),
		attribute.String("langfuse.observation.input", string(input)),
		attribute.String("langfuse.trace.name", name),
	)
	if sid := sessionID(ctx); sid != "" {
		span.SetAttributes(attribute.String("langfuse.session.id", sid))
	}
	if r, ok := tracedPrompt(ctx); ok {
		setPromptAttrs(span, r)
	}

	out, err := t.inner.Complete(ctx, system, user)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return out, err
	}
	span.SetAttributes(attribute.String("langfuse.observation.output", out))
	return out, nil
}

// Operation traces a step that takes no context of its own — the speech engine's
// STT and TTS calls, whose signatures are (bytes)->string and (string)->bytes.
// Latency is the point here: synthesis and transcription time is dead air on a
// live call, and it is invisible in the LLM spans.
//
// Always returns a usable value, so call sites need no nil checks; with tracing
// off every method is a no-op on OTel's noop span.
type Operation struct{ span trace.Span }

// StartOp begins a traced operation. Call End when it finishes.
func StartOp(ctx context.Context, name string) *Operation {
	_, span := otel.Tracer("voicesurvey").Start(ctx, name)
	span.SetAttributes(
		attribute.String("langfuse.observation.type", "span"),
		attribute.String("langfuse.trace.name", name),
	)
	if sid := sessionID(ctx); sid != "" {
		span.SetAttributes(attribute.String("langfuse.session.id", sid))
	}
	return &Operation{span: span}
}

// In records what went into the operation (a transcript, or a byte count).
func (o *Operation) In(v string) *Operation {
	o.span.SetAttributes(attribute.String("langfuse.observation.input", v))
	return o
}

// Out records what came back.
func (o *Operation) Out(v string) *Operation {
	o.span.SetAttributes(attribute.String("langfuse.observation.output", v))
	return o
}

// Bytes records a payload size, so audio volume can be read next to latency.
func (o *Operation) Bytes(key string, n int) *Operation {
	o.span.SetAttributes(attribute.Int(key, n))
	return o
}

// Fail marks the operation as errored.
func (o *Operation) Fail(err error) *Operation {
	if err != nil {
		o.span.RecordError(err)
		o.span.SetStatus(codes.Error, err.Error())
	}
	return o
}

// End closes the span, fixing its duration.
func (o *Operation) End() { o.span.End() }

// EvalCase is the labeled expectation for one eval case. Attached to a context,
// it turns the classifier span into a scored eval observation: the span records
// what was expected next to what the model produced, plus the run it belongs to.
// This is how an eval run becomes queryable history in Langfuse — group by
// run/prompt version, filter to intent_correct=false, read the failures.
type EvalCase struct {
	Run             string // run name, shared by every case in one eval invocation
	ExpectedIntent  string
	ExpectedClarity string // empty when this case doesn't score clarity
}

type evalKey struct{}

// WithEvalCase tags a context as belonging to an eval run rather than a real
// conversation. Spans created under it are tagged "eval" so they can be excluded
// from production dashboards.
func WithEvalCase(ctx context.Context, c EvalCase) context.Context {
	return context.WithValue(ctx, evalKey{}, c)
}

func evalCase(ctx context.Context) (EvalCase, bool) {
	c, ok := ctx.Value(evalKey{}).(EvalCase)
	return c, ok
}

// TraceClassifier wraps any llm.Classifier with a span per ClassifyTurn. The
// span carries the question/reply as input, the full Turn as output, and the
// langfuse.* / gen_ai.* attributes Langfuse maps to its generation view. The
// wrapper is a pure pass-through: it never changes the Turn or the error.
func TraceClassifier(inner llm.Classifier, model string) llm.Classifier {
	return &tracedClassifier{inner: inner, model: model}
}

type tracedClassifier struct {
	inner llm.Classifier
	model string
}

func (t *tracedClassifier) ClassifyTurn(ctx context.Context, question, reply string) (llm.Turn, error) {
	ctx, span := otel.Tracer("voicesurvey").Start(ctx, "classify_turn")
	defer span.End()
	if ref := traceRef(ctx); ref != nil {
		if sc := span.SpanContext(); sc.HasTraceID() {
			ref.id = sc.TraceID().String()
		}
	}

	input, _ := json.Marshal(map[string]string{"question": question, "reply": reply})
	span.SetAttributes(
		attribute.String("gen_ai.request.model", t.model),
		attribute.String("langfuse.observation.type", "generation"),
		attribute.String("langfuse.observation.model.name", t.model),
		attribute.String("langfuse.observation.input", string(input)),
		attribute.String("langfuse.trace.name", "classify_turn"),
	)
	// Which prompt produced this decision. Filter/group by it in Langfuse to see
	// whether a score moved because of a prompt edit.
	setPromptAttrs(span, llm.ClassifyPrompt.Resolve())
	if sid := sessionID(ctx); sid != "" {
		span.SetAttributes(attribute.String("langfuse.session.id", sid))
	}
	ec, isEval := evalCase(ctx)
	if isEval {
		span.SetAttributes(
			attribute.StringSlice("langfuse.trace.tags", []string{"eval"}),
			attribute.String("langfuse.trace.metadata.eval_run", ec.Run),
			attribute.String("langfuse.trace.metadata.expected_intent", ec.ExpectedIntent),
		)
		if ec.ExpectedClarity != "" {
			span.SetAttributes(attribute.String("langfuse.trace.metadata.expected_clarity", ec.ExpectedClarity))
		}
	}

	turn, err := t.inner.ClassifyTurn(ctx, question, reply)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return turn, err
	}
	output, _ := json.Marshal(turn)
	span.SetAttributes(
		attribute.String("langfuse.observation.output", string(output)),
		attribute.String("classify.intent", string(turn.Intent)),
		attribute.String("classify.clarity", string(turn.Clarity)),
		attribute.Bool("classify.sufficient", turn.Sufficient),
	)
	// On an eval case the span also carries the verdict, so Langfuse can be
	// filtered straight to the misses instead of re-deriving them in the UI.
	if isEval {
		span.SetAttributes(
			attribute.Bool("langfuse.trace.metadata.intent_correct", string(turn.Intent) == ec.ExpectedIntent),
		)
		if ec.ExpectedClarity != "" {
			span.SetAttributes(
				attribute.Bool("langfuse.trace.metadata.clarity_correct", string(turn.Clarity) == ec.ExpectedClarity),
			)
		}
	}
	return turn, nil
}

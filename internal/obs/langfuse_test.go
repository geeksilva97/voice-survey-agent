package obs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recorded is one request the fake Langfuse received.
type recorded struct {
	path string
	auth string
	body map[string]any
}

// fakeLangfuse stands in for the real API so the client's paths, auth header and
// payload shapes are verified for real — no credentials and no network needed.
// respond lets a test drive status codes per call (used for the 404 retry path).
func fakeLangfuse(t *testing.T, respond func(n int, path string) (int, string)) (*Client, *[]recorded) {
	t.Helper()
	var got []recorded
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, recorded{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		n++
		status, payload := http.StatusOK, `{"id":"x","datasetRunId":"run-1"}`
		if respond != nil {
			if s, p := respond(n, r.URL.Path); s != 0 {
				status, payload = s, p
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	c := &Client{
		base: srv.URL,
		auth: "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-lf-test:sk-lf-test")),
		hc:   srv.Client(),
		// no sleeping in tests
	}
	return c, &got
}

func TestNewClientDisabledWithoutKeys(t *testing.T) {
	t.Setenv("LANGFUSE_PUBLIC_KEY", "")
	t.Setenv("LANGFUSE_SECRET_KEY", "")
	if _, ok := NewClient(); ok {
		t.Fatal("NewClient reported ok without credentials")
	}
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-lf-1")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-lf-1")
	c, ok := NewClient()
	if !ok {
		t.Fatal("NewClient not ok with credentials set")
	}
	// The secret must only ever appear inside the encoded Basic header.
	if strings.Contains(c.base, "sk-lf-1") || !strings.HasPrefix(c.auth, "Basic ") {
		t.Fatalf("unexpected client shape: base=%q", c.base)
	}
}

// The endpoint paths and required field names are the part most likely to rot,
// so pin them: they came from the live OpenAPI spec, not from guesswork.
func TestEndpointsAndPayloads(t *testing.T) {
	c, got := fakeLangfuse(t, nil)
	ctx := context.Background()

	if err := c.EnsureDataset(ctx, "turn-classifier", "desc"); err != nil {
		t.Fatalf("EnsureDataset: %v", err)
	}
	if err := c.UpsertItem(ctx, "turn-classifier", "item-1",
		map[string]string{"question": "Q?", "reply": "R."},
		map[string]string{"intent": "answer"}); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	runID, err := c.AddRunItem(ctx, "run@1", "item-1", "trace-abc", "desc", map[string]any{"model": "m"})
	if err != nil {
		t.Fatalf("AddRunItem: %v", err)
	}
	if runID != "run-1" {
		t.Errorf("AddRunItem returned run id %q, want %q", runID, "run-1")
	}

	want := []struct {
		path   string
		fields []string
	}{
		{"/api/public/v2/datasets", []string{"name", "description"}},
		{"/api/public/dataset-items", []string{"datasetName", "id", "input", "expectedOutput", "status"}},
		{"/api/public/dataset-run-items", []string{"runName", "datasetItemId", "traceId", "runDescription", "metadata"}},
	}
	if len(*got) != len(want) {
		t.Fatalf("got %d requests, want %d", len(*got), len(want))
	}
	for i, w := range want {
		r := (*got)[i]
		if r.path != w.path {
			t.Errorf("request %d path = %q, want %q", i, r.path, w.path)
		}
		if !strings.HasPrefix(r.auth, "Basic ") {
			t.Errorf("request %d missing Basic auth", i)
		}
		for _, f := range w.fields {
			if _, present := r.body[f]; !present {
				t.Errorf("request %d (%s) missing field %q; body=%v", i, r.path, f, r.body)
			}
		}
	}
}

// A run item whose trace Langfuse hasn't ingested yet answers 404. That is the
// one retryable failure (a 404 created nothing, so retrying can't duplicate the
// non-idempotent run item).
func TestAddRunItemRetriesOn404(t *testing.T) {
	c, got := fakeLangfuse(t, func(n int, _ string) (int, string) {
		if n <= 2 {
			return http.StatusNotFound, `{"message":"Trace not found"}`
		}
		return 0, ""
	})
	runID, err := c.AddRunItem(context.Background(), "run@1", "item-1", "trace-abc", "", nil)
	if err != nil {
		t.Fatalf("AddRunItem should have recovered after 404s: %v", err)
	}
	if runID != "run-1" {
		t.Errorf("run id = %q, want run-1", runID)
	}
	if len(*got) != 3 {
		t.Errorf("made %d attempts, want 3 (two 404s then success)", len(*got))
	}
}

// Any non-404 error is a real failure and must NOT be retried — the endpoint
// mints its own ids, so a blind retry would create a duplicate run item.
func TestAddRunItemDoesNotRetryOtherErrors(t *testing.T) {
	c, got := fakeLangfuse(t, func(int, string) (int, string) {
		return http.StatusInternalServerError, `{"message":"boom"}`
	})
	if _, err := c.AddRunItem(context.Background(), "run@1", "item-1", "trace-abc", "", nil); err == nil {
		t.Fatal("expected an error on 500")
	}
	if len(*got) != 1 {
		t.Errorf("made %d attempts on a 500, want exactly 1", len(*got))
	}
}

// The API enforces "exactly one subject"; catching it locally turns a confusing
// server-side rejection into a clear programming error.
func TestScoreValidation(t *testing.T) {
	bad := map[string]Score{
		"no subject":   {Name: "n", Value: 1},
		"two subjects": {Name: "n", Value: 1, TraceID: "t", DatasetRunID: "r"},
		"no name":      {Name: " ", Value: 1, TraceID: "t"},
		"bad boolean":  {Name: "n", Value: 0.5, DataType: "BOOLEAN", TraceID: "t"},
	}
	for label, s := range bad {
		if err := s.validate(); err == nil {
			t.Errorf("%s: validate() = nil, want an error", label)
		}
	}
	ok := []Score{
		{Name: "intent_correct", Value: 1, DataType: "BOOLEAN", TraceID: "t"},
		{Name: "intent_accuracy", Value: 0.951, DataType: "NUMERIC", DatasetRunID: "r"},
	}
	for _, s := range ok {
		if err := s.validate(); err != nil {
			t.Errorf("validate(%s) = %v, want nil", s.Name, err)
		}
	}
}

// A trace score and a run score must target different fields; the API rejects
// the wrong combination, so verify what actually goes on the wire.
func TestScoreTargeting(t *testing.T) {
	c, got := fakeLangfuse(t, nil)
	ctx := context.Background()
	if err := c.Score(ctx, Score{ID: "s1", Name: "intent_correct", Value: 1, DataType: "BOOLEAN", TraceID: "t-1"}); err != nil {
		t.Fatalf("trace score: %v", err)
	}
	if err := c.Score(ctx, Score{ID: "s2", Name: "intent_accuracy", Value: 0.95, DataType: "NUMERIC", DatasetRunID: "r-1"}); err != nil {
		t.Fatalf("run score: %v", err)
	}
	if len(*got) != 2 {
		t.Fatalf("got %d requests, want 2", len(*got))
	}
	traceBody, runBody := (*got)[0].body, (*got)[1].body
	if traceBody["traceId"] != "t-1" {
		t.Errorf("trace score traceId = %v, want t-1", traceBody["traceId"])
	}
	if _, present := traceBody["datasetRunId"]; present {
		t.Error("trace score must not carry datasetRunId")
	}
	if runBody["datasetRunId"] != "r-1" {
		t.Errorf("run score datasetRunId = %v, want r-1", runBody["datasetRunId"])
	}
	if _, present := runBody["traceId"]; present {
		t.Error("run score must not carry traceId")
	}
}

// Dataset item ids and score ids are content-addressed so re-running the eval
// upserts instead of duplicating.
func TestStableID(t *testing.T) {
	a := StableID("turn-classifier", "Q?", "R.")
	if a != StableID("turn-classifier", "Q?", "R.") {
		t.Error("StableID is not stable for identical input")
	}
	if a == StableID("turn-classifier", "Q?", "R!") {
		t.Error("StableID collided on different input")
	}
	// The API caps ids at 255 chars; ours are far shorter and fixed-width.
	if len(a) != 32 {
		t.Errorf("len(StableID) = %d, want 32", len(a))
	}
	// Field separation: ("a","bc") must not hash the same as ("ab","c").
	if StableID("a", "bc") == StableID("ab", "c") {
		t.Error("StableID must separate its parts")
	}
}

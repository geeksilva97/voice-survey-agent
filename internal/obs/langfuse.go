// Langfuse public REST API client. Traces go out over OTLP (obs.go); this file
// covers the parts OTel has no concept of — datasets, experiment runs, and
// scores — because Langfuse ships no Go SDK. Raw net/http on purpose, matching
// internal/llm's Anthropic client: one fewer dependency, and the payloads are
// small enough that a generated client would be pure overhead.
//
// Endpoint semantics that shape this code:
//   - datasets upsert by NAME, dataset items upsert by ID → re-pushing the
//     corpus every run is safe and creates no duplicates.
//   - dataset RUN ITEMS are not idempotent (the server mints the id), so they
//     are posted exactly once and never blind-retried.
//   - scores take a client-supplied id, so they upsert too.
package obs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client talks to the Langfuse public API with Basic auth (public key as user,
// secret key as password). The secret is held only in the encoded header and is
// never logged or surfaced.
type Client struct {
	base string
	auth string
	hc   *http.Client
	// retryDelay is the base backoff for the one retryable case (a run item whose
	// trace Langfuse hasn't ingested yet). A field so tests can drive the retry
	// path without sleeping.
	retryDelay time.Duration
}

// NewClient builds a client from LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY
// (LANGFUSE_HOST optional). ok is false when credentials are absent, which is
// the normal offline case — callers skip the whole export path.
func NewClient() (c *Client, ok bool) {
	pk := strings.TrimSpace(os.Getenv("LANGFUSE_PUBLIC_KEY"))
	sk := strings.TrimSpace(os.Getenv("LANGFUSE_SECRET_KEY"))
	if pk == "" || sk == "" {
		return nil, false
	}
	return &Client{
		base:       Host(),
		auth:       "Basic " + base64.StdEncoding.EncodeToString([]byte(pk+":"+sk)),
		hc:         &http.Client{Timeout: 30 * time.Second},
		retryDelay: 2 * time.Second,
	}, true
}

// post sends one JSON request and decodes the response into out (may be nil).
// It returns the HTTP status alongside the error so callers can react to a 404
// (the "trace not yet ingested" case) without string-matching the message.
func (c *Client) post(ctx context.Context, path string, body, out any) (int, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(buf))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("POST %s: %s: %s", path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("POST %s: decode response: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// EnsureDataset creates the dataset, or updates its description if it already
// exists — the endpoint upserts on (project, name), so this is safe every run.
func (c *Client) EnsureDataset(ctx context.Context, name, description string) error {
	_, err := c.post(ctx, "/api/public/v2/datasets", map[string]any{
		"name":        name,
		"description": description,
	}, nil)
	return err
}

// UpsertItem writes one labeled case. id must be stable for the case (we use a
// content hash), which makes re-pushing the corpus idempotent. An id may not be
// reused across datasets.
func (c *Client) UpsertItem(ctx context.Context, dataset, id string, input, expected any) error {
	_, err := c.post(ctx, "/api/public/dataset-items", map[string]any{
		"datasetName":    dataset,
		"id":             id,
		"input":          input,
		"expectedOutput": expected,
		"status":         "ACTIVE",
	}, nil)
	return err
}

// runItemResp is the subset of DatasetRunItem we need: the run id, which is the
// subject for run-level (aggregate) scores.
type runItemResp struct {
	DatasetRunID string `json:"datasetRunId"`
}

// AddRunItem attaches one dataset item + the trace it produced to a named run,
// creating the run on first call. Returns the run id for run-level scores.
//
// The trace must already be ingested or the API answers 404, and OTLP export is
// asynchronous — so a 404 is retried with backoff. That is safe specifically
// because a 404 means nothing was created; this endpoint is otherwise NOT
// idempotent, so no other status is ever retried.
func (c *Client) AddRunItem(ctx context.Context, runName, itemID, traceID, runDescription string, meta any) (string, error) {
	body := map[string]any{
		"runName":       runName,
		"datasetItemId": itemID,
		"traceId":       traceID,
	}
	if runDescription != "" {
		body["runDescription"] = runDescription
	}
	if meta != nil {
		body["metadata"] = meta
	}
	var out runItemResp
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		status, err := c.post(ctx, "/api/public/dataset-run-items", body, &out)
		if err == nil {
			return out.DatasetRunID, nil
		}
		lastErr = err
		if status != http.StatusNotFound {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt+1) * c.retryDelay):
		}
	}
	return "", fmt.Errorf("trace %s never became visible: %w", traceID, lastErr)
}

// Score is one numeric or boolean judgment. Exactly ONE subject must be set —
// TraceID (optionally with ObservationID), or DatasetRunID — which the API
// enforces; Validate catches it before the round-trip.
type Score struct {
	Name         string
	Value        float64
	DataType     string // NUMERIC or BOOLEAN (BOOLEAN requires Value 0 or 1)
	Comment      string
	TraceID      string
	DatasetRunID string
	// ID makes the write idempotent: the same id upserts instead of duplicating.
	// Leave empty to let the server mint one.
	ID string
}

func (s Score) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("score name is required")
	}
	subjects := 0
	if s.TraceID != "" {
		subjects++
	}
	if s.DatasetRunID != "" {
		subjects++
	}
	if subjects != 1 {
		return fmt.Errorf("score %q: need exactly one of TraceID or DatasetRunID (got %d)", s.Name, subjects)
	}
	if s.DataType == "BOOLEAN" && s.Value != 0 && s.Value != 1 {
		return fmt.Errorf("score %q: BOOLEAN value must be 0 or 1, got %v", s.Name, s.Value)
	}
	return nil
}

// Score writes one score. Scores are how a run's numbers become chartable
// history in Langfuse — the aggregate metrics land on the run, per-case verdicts
// on individual traces.
func (c *Client) Score(ctx context.Context, s Score) error {
	if err := s.validate(); err != nil {
		return err
	}
	body := map[string]any{"name": s.Name, "value": s.Value}
	if s.DataType != "" {
		body["dataType"] = s.DataType
	}
	if s.Comment != "" {
		body["comment"] = s.Comment
	}
	if s.ID != "" {
		body["id"] = s.ID
	}
	if s.TraceID != "" {
		body["traceId"] = s.TraceID
	} else {
		body["datasetRunId"] = s.DatasetRunID
	}
	_, err := c.post(ctx, "/api/public/scores", body, nil)
	return err
}

// StableID derives a deterministic id from its parts. Used for dataset item ids
// (so the corpus upserts) and score ids (so re-runs overwrite rather than pile
// up duplicates).
func StableID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

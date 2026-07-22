// Package session stores poll definitions and their captured answers. It keeps
// everything in memory and also persists each poll to data/<id>.json so the
// results view survives a restart and the demo has durable proof of capture.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"voicesurvey/internal/insight"
	"voicesurvey/internal/survey"
)

// Poll is one created survey: its questions and (once answered) results.
type Poll struct {
	ID        string           `json:"id"`
	Product   string           `json:"product"`
	Questions []string         `json:"questions"`
	// Intro is a warm, product-tailored opening line authored by the LLM at
	// creation time and spoken before the first question. Empty falls back to a
	// fixed greeting at runtime.
	Intro     string           `json:"intro,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	Survey    *survey.Survey   `json:"survey,omitempty"`
	EndReason survey.EndReason `json:"end_reason,omitempty"`
	EndedAt   *time.Time       `json:"ended_at,omitempty"`
	// Insights caches the LLM scoring pass (product sentiment, per-answer
	// usefulness/confidence, summary). Computed on demand and persisted so a
	// re-open doesn't re-run the model. nil until the insights endpoint is hit.
	Insights *insight.Result `json:"insights,omitempty"`
}

// Store is a concurrency-safe collection of polls with JSON persistence.
type Store struct {
	mu   sync.RWMutex
	dir  string
	byID map[string]*Poll
}

// NewStore creates the store and its data directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, byID: map[string]*Poll{}}, nil
}

// Create registers a new poll from a product description, question set, and an
// optional LLM-authored opening line.
func (s *Store) Create(product string, questions []string, intro string) *Poll {
	p := &Poll{
		ID:        newID(),
		Product:   product,
		Questions: questions,
		Intro:     intro,
		CreatedAt: time.Now(),
		Survey:    survey.New(questions),
	}
	s.mu.Lock()
	s.byID[p.ID] = p
	s.mu.Unlock()
	s.persist(p)
	return p
}

// Get returns a poll by id.
func (s *Store) Get(id string) (*Poll, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	return p, ok
}

// GetOrLoad returns a poll by id, lazily loading it from data/<id>.json when it
// isn't already in memory. This lets the results/insights views open a poll
// captured in an earlier server run (the store is otherwise memory-only). The
// id is validated as a plain hex token so it can't escape the data dir.
func (s *Store) GetOrLoad(id string) (*Poll, bool) {
	if p, ok := s.Get(id); ok {
		return p, true
	}
	if !validID(id) {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return nil, false
	}
	var p Poll
	if err := json.Unmarshal(b, &p); err != nil || p.ID == "" {
		return nil, false
	}
	s.mu.Lock()
	// Re-check under the write lock in case a concurrent caller loaded it first.
	if existing, ok := s.byID[p.ID]; ok {
		s.mu.Unlock()
		return existing, true
	}
	s.byID[p.ID] = &p
	s.mu.Unlock()
	return &p, true
}

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'f') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// Save flushes a poll to disk (call after answers change or it ends).
func (s *Store) Save(p *Poll) { s.persist(p) }

func (s *Store) persist(p *Poll) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(s.dir, p.ID+".json"), b, 0o644)
}

func newID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

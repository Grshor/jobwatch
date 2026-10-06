// Package store: JSON-file state — the dedup set with verdict history. The
// feed is small (hundreds of vacancies a week): a file with a mutex beats a
// database for v0.1. Writes are atomic (tmp+rename).
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Entry struct {
	Seen    time.Time `json:"seen"`
	Verdict string    `json:"verdict,omitempty"`
}

type State struct {
	mu   sync.Mutex
	path string
	Seen map[string]Entry `json:"seen"`
}

// Open loads (or initializes) the state file.
func Open(path string) (*State, error) {
	s := &State{path: path, Seen: map[string]Entry{}}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, s); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// FirstSeen reports whether id is new; marks it seen either way.
func (s *State) FirstSeen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.Seen[id]
	if !ok {
		s.Seen[id] = Entry{Seen: time.Now()}
	}
	return !ok
}

// Mark records the verdict for an already-seen id.
func (s *State) Mark(id, verdict string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.Seen[id]
	e.Verdict = verdict
	s.Seen[id] = e
}

// Save writes the state atomically. Caller decides frequency.
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Prune drops entries older than ttl to keep the file bounded.
func (s *State) Prune(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.Seen {
		if time.Since(e.Seen) > ttl {
			delete(s.Seen, id)
		}
	}
}

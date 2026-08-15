// Package store handles JSON file persistence for Capabilities and Runs.
// It depends only on the domain package.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"computer-use/domain"
)

// ErrNotFound is returned when a capability or run is not found.
var ErrNotFound = errors.New("not found")

// Store persists Capabilities and Runs to a directory as JSON files.
type Store struct {
	mu  sync.RWMutex
	dir string
}

// New creates a new Store rooted at dir. The directory is created if it doesn't exist.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "capabilities"), 0755); err != nil {
		return nil, fmt.Errorf("store: create capabilities dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0755); err != nil {
		return nil, fmt.Errorf("store: create runs dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// SaveCapability writes a Capability to disk as <dir>/capabilities/<id>.json.
func (s *Store) SaveCapability(cap *domain.Capability) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(cap, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal capability: %w", err)
	}
	path := filepath.Join(s.dir, "capabilities", cap.ID+".json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("store: write capability: %w", err)
	}
	return nil
}

// GetCapability loads a Capability by ID from disk.
func (s *Store) GetCapability(id string) (*domain.Capability, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := filepath.Join(s.dir, "capabilities", id+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("capability %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read capability: %w", err)
	}
	var cap domain.Capability
	if err := json.Unmarshal(data, &cap); err != nil {
		return nil, fmt.Errorf("store: unmarshal capability: %w", err)
	}
	return &cap, nil
}

// SaveRun writes a Run audit log to disk as <dir>/runs/<run_id>.json.
func (s *Store) SaveRun(run *domain.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal run: %w", err)
	}
	path := filepath.Join(s.dir, "runs", run.RunID+".json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("store: write run: %w", err)
	}
	return nil
}

// GetRun loads a Run by ID from disk.
func (s *Store) GetRun(id string) (*domain.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := filepath.Join(s.dir, "runs", id+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("run %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read run: %w", err)
	}
	var run domain.Run
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("store: unmarshal run: %w", err)
	}
	return &run, nil
}

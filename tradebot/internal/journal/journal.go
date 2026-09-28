// Package journal is an append-only JSONL audit log of every decision.
package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Journal struct {
	mu   sync.Mutex
	path string
}

func Open(stateDir string) (*Journal, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	return &Journal{path: filepath.Join(stateDir, "journal.jsonl")}, nil
}

// Log appends one event. Errors are returned but callers usually continue.
func (j *Journal) Log(kind string, data any) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	b, err := json.Marshal(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "kind": kind, "data": data})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

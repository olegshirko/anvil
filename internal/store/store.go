package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
)

// State holds per-instance metadata persisted as JSON.
type State struct {
	// DiskFormatted indicates whether the runtime data disk has been formatted.
	DiskFormatted bool `json:"disk_formatted"`
	// DiskRuntime is the container runtime the disk was provisioned for.
	DiskRuntime string `json:"disk_runtime"`
	// Kubernetes stores serialized Kubernetes configuration.
	Kubernetes string `json:"kubernetes,omitempty"`
}

// Fetch reads the state from disk. A missing file yields an empty State.
func Fetch(path string) (State, error) {
	var s State
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, fmt.Errorf("cannot read state file: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("cannot parse state file: %w", err)
	}
	return s, nil
}

// Persist writes the state to disk.
func Persist(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("cannot create state directory: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("cannot write state file: %w", err)
	}
	return nil
}

// Mutate loads the state, applies fn, and writes it back.
func Mutate(path string, fn func(*State)) error {
	s, err := Fetch(path)
	if err != nil {
		logrus.Debugf("state load error: %v", err)
	}
	fn(&s)
	if err := Persist(path, s); err != nil {
		return fmt.Errorf("cannot persist state: %w", err)
	}
	return nil
}

// Clear removes the state file. If removal fails, it overwrites with defaults.
func Clear(path string) error {
	if err := os.Remove(path); err != nil {
		return Mutate(path, func(s *State) { *s = State{} })
	}
	return nil
}

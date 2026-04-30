package embedded

import (
	"embed"
	"fmt"
	"sync"
)

// content holds all embedded static assets.
//
//go:embed defaults/*.yaml images/*.yaml k3s/*.json network/*
var content embed.FS

// Assets provides cached access to embedded files.
type Assets struct {
	mu    sync.RWMutex
	cache map[string][]byte
}

// NewAssets creates an empty asset cache backed by the embedded filesystem.
func NewAssets() *Assets {
	return &Assets{cache: make(map[string][]byte)}
}

// Load retrieves the contents of an embedded file. Results are cached in memory.
func (a *Assets) Load(name string) ([]byte, error) {
	a.mu.RLock()
	if b, ok := a.cache[name]; ok {
		a.mu.RUnlock()
		return b, nil
	}
	a.mu.RUnlock()

	b, err := content.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("embedded asset %q not found: %w", name, err)
	}

	a.mu.Lock()
	a.cache[name] = b
	a.mu.Unlock()
	return b, nil
}

// LoadString retrieves an embedded file as a string.
func (a *Assets) LoadString(name string) (string, error) {
	b, err := a.Load(name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// MustLoadString is like LoadString but panics on error. Use only for assets
// that are guaranteed to exist at compile time.
func (a *Assets) MustLoadString(name string) string {
	s, err := a.LoadString(name)
	if err != nil {
		panic(err)
	}
	return s
}

// --- package-level helpers for backward compatibility during migration ---

var defaultAssets = NewAssets()

// Read loads an embedded file using the default asset cache.
func Read(name string) ([]byte, error) {
	return defaultAssets.Load(name)
}

// ReadString loads an embedded file as a string using the default asset cache.
func ReadString(name string) (string, error) {
	return defaultAssets.LoadString(name)
}

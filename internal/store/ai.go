package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// aiKeys is the API keys for the translation providers, kept in a file of its
// own rather than in the settings.
//
// The reason is the settings file's journey: it is sent to the browser on every
// page load, because the page needs the directories and the concurrency to draw
// itself. A key that rides along would be readable by anything that can reach
// the UI — including, when the server is bound to a local network, a phone that
// is not this one. Nothing here is ever put in a response; the API reports only
// whether a key is set.
type aiKeys struct {
	mu   sync.Mutex
	path string
	keys map[string]string
}

func (s *Store) keys() *aiKeys {
	s.keysOnce.Do(func() {
		s.aiKeys = &aiKeys{
			path: filepath.Join(s.dir, "ai-keys.json"),
			keys: map[string]string{},
		}
		if data, err := os.ReadFile(s.aiKeys.path); err == nil {
			_ = json.Unmarshal(data, &s.aiKeys.keys)
		}
		if s.aiKeys.keys == nil {
			s.aiKeys.keys = map[string]string{}
		}
	})
	return s.aiKeys
}

// AIKey returns the stored key for a provider, or "" when none is set.
func (s *Store) AIKey(provider string) string {
	k := s.keys()
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.keys[strings.ToLower(strings.TrimSpace(provider))]
}

// HasAIKey reports whether a provider has a key, without handing the key out.
func (s *Store) HasAIKey(provider string) bool { return s.AIKey(provider) != "" }

// SetAIKey stores or clears a provider's key. An empty key forgets it, which is
// how a user takes a key back out after pasting the wrong one.
func (s *Store) SetAIKey(provider, key string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil
	}
	k := s.keys()
	k.mu.Lock()
	defer k.mu.Unlock()

	key = strings.TrimSpace(key)
	if key == "" {
		delete(k.keys, provider)
	} else {
		k.keys[provider] = key
	}
	return k.saveLocked()
}

// AIKeyProviders lists the providers a key is stored for, so the settings panel
// can show which ones are ready without revealing any of them.
func (s *Store) AIKeyProviders() []string {
	k := s.keys()
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]string, 0, len(k.keys))
	for p := range k.keys {
		out = append(out, p)
	}
	return out
}

// saveLocked writes the key file with owner-only permissions.
//
// The mode is set on the temporary file before anything is written into it, so
// there is no moment where the keys are readable by anyone else — which is the
// whole point of keeping them out of the settings file.
func (k *aiKeys) saveLocked() error {
	data, err := json.MarshalIndent(k.keys, "", "  ")
	if err != nil {
		return err
	}
	tmp := k.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, k.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

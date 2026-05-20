package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Meta is the persisted shape of .meta/<munged>.json (local + remote copies).
type Meta struct {
	Munged         string                 `json:"munged"`
	Path           string                 `json:"path"`
	IsGit          bool                   `json:"is_git"`
	Owner          string                 `json:"owner"`
	RemoteState    string                 `json:"remote_state"`
	Streaming      bool                   `json:"streaming"`
	ActiveBranch   string                 `json:"active_branch,omitempty"`
	OriginCwd      string                 `json:"origin_cwd"`
	LastMirrorPush time.Time              `json:"last_mirror_push,omitempty"`
	LastError      string                 `json:"last_error,omitempty"`
	Sessions       map[string]SessionMeta `json:"sessions,omitempty"`
}

// MetaStore loads and saves Meta files under a local meta directory.
type MetaStore struct {
	dir string
	mu  sync.Mutex
}

func NewMetaStore(dir string) *MetaStore { return &MetaStore{dir: dir} }

func (s *MetaStore) Dir() string { return s.dir }

func (s *MetaStore) path(munged string) string {
	return filepath.Join(s.dir, munged+".json")
}

// Load reads a single meta file. Returns (nil, nil) when the file doesn't exist.
func (s *MetaStore) Load(munged string) (*Meta, error) {
	b, err := os.ReadFile(s.path(munged))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("meta: read %s: %w", munged, err)
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("meta: parse %s: %w", munged, err)
	}
	return &m, nil
}

// LoadAll returns every meta file in the store.
func (s *MetaStore) LoadAll() ([]*Meta, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]*Meta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		munged := e.Name()[:len(e.Name())-len(".json")]
		m, err := s.Load(munged)
		if err != nil {
			continue
		}
		if m != nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// Save writes meta to disk atomically (write to .tmp + rename).
func (s *MetaStore) Save(m *Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	final := s.path(m.Munged)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

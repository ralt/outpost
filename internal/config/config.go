package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/ini.v1"
)

type Config struct {
	Paths   Paths
	Remote  Remote
	Sync    Sync
	Logging Logging

	// SourcePath is the absolute path the config was loaded from, or "" if defaults only.
	SourcePath string
}

type Paths struct {
	Backing       string
	Mount         string
	ControlSocket string
}

type Remote struct {
	Host              string
	Port              int
	IdentityFile      string
	KnownHostsFile    string
	ClaudeBin         string
	KeepaliveInterval time.Duration
	ContinuePrompt    string
	AllowedTools      string
}

type Sync struct {
	OnConflict         string
	SendUntracked      bool
	BackgroundInterval time.Duration
	DiscoveryDebounce  time.Duration
}

type Logging struct {
	Level  string
	Format string
}

// DefaultPath returns the path that Load uses when none is supplied.
func DefaultPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "outpost", "config.ini")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "outpost", "config.ini")
}

// Defaults returns a Config populated with every documented default.
func Defaults() Config {
	home, _ := os.UserHomeDir()
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		dataDir = filepath.Join(home, ".local", "share")
	}
	runDir := os.Getenv("XDG_RUNTIME_DIR")
	if runDir == "" {
		runDir = filepath.Join("/run/user", fmt.Sprintf("%d", os.Getuid()))
	}
	return Config{
		Paths: Paths{
			Backing:       filepath.Join(dataDir, "outpost", "data"),
			Mount:         filepath.Join(home, ".claude"),
			ControlSocket: filepath.Join(runDir, "outpost.sock"),
		},
		Remote: Remote{
			Port:              22,
			KnownHostsFile:    filepath.Join(home, ".ssh", "known_hosts"),
			ClaudeBin:         "claude",
			KeepaliveInterval: 30 * time.Second,
			ContinuePrompt:    "continue",
			AllowedTools:      "Bash,Read,Edit,Write,Glob,Grep,WebFetch",
		},
		Sync: Sync{
			OnConflict:         "abort",
			SendUntracked:      true,
			BackgroundInterval: 1 * time.Hour,
			DiscoveryDebounce:  5 * time.Second,
		},
		Logging: Logging{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load reads the INI file at path (or DefaultPath when empty), applies defaults,
// and validates. A missing file is fine — returns Defaults().
func Load(path string) (Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	cfg := Defaults()
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	cfg.SourcePath = path

	f, err := ini.LoadSources(ini.LoadOptions{IgnoreInlineComment: true}, path)
	if err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}

	if s, ok := getSection(f, "paths"); ok {
		setStr(s, "backing", &cfg.Paths.Backing)
		setStr(s, "mount", &cfg.Paths.Mount)
		setStr(s, "control_socket", &cfg.Paths.ControlSocket)
	}
	if s, ok := getSection(f, "remote"); ok {
		setStr(s, "host", &cfg.Remote.Host)
		setInt(s, "port", &cfg.Remote.Port)
		setStr(s, "identity_file", &cfg.Remote.IdentityFile)
		setStr(s, "known_hosts_file", &cfg.Remote.KnownHostsFile)
		setStr(s, "claude_bin", &cfg.Remote.ClaudeBin)
		if err := setDur(s, "keepalive_interval", &cfg.Remote.KeepaliveInterval); err != nil {
			return cfg, err
		}
		setStr(s, "continue_prompt", &cfg.Remote.ContinuePrompt)
		setStr(s, "allowed_tools", &cfg.Remote.AllowedTools)
	}
	if s, ok := getSection(f, "sync"); ok {
		setStr(s, "on_conflict", &cfg.Sync.OnConflict)
		setBool(s, "send_untracked", &cfg.Sync.SendUntracked)
		if err := setDur(s, "background_interval", &cfg.Sync.BackgroundInterval); err != nil {
			return cfg, err
		}
		if err := setDur(s, "discovery_debounce", &cfg.Sync.DiscoveryDebounce); err != nil {
			return cfg, err
		}
	}
	if s, ok := getSection(f, "logging"); ok {
		setStr(s, "level", &cfg.Logging.Level)
		setStr(s, "format", &cfg.Logging.Format)
	}

	cfg.expandPaths()
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func getSection(f *ini.File, name string) (*ini.Section, bool) {
	if !f.HasSection(name) {
		return nil, false
	}
	s, err := f.GetSection(name)
	if err != nil {
		return nil, false
	}
	return s, true
}

func setStr(s *ini.Section, key string, dst *string) {
	if !s.HasKey(key) {
		return
	}
	*dst = s.Key(key).String()
}

func setInt(s *ini.Section, key string, dst *int) {
	if !s.HasKey(key) {
		return
	}
	if v, err := s.Key(key).Int(); err == nil {
		*dst = v
	}
}

func setBool(s *ini.Section, key string, dst *bool) {
	if !s.HasKey(key) {
		return
	}
	if v, err := s.Key(key).Bool(); err == nil {
		*dst = v
	}
}

func setDur(s *ini.Section, key string, dst *time.Duration) error {
	if !s.HasKey(key) {
		return nil
	}
	raw := s.Key(key).String()
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("config key %s: %w", key, err)
	}
	*dst = d
	return nil
}

func (c *Config) expandPaths() {
	c.Paths.Backing = expand(c.Paths.Backing)
	c.Paths.Mount = expand(c.Paths.Mount)
	c.Paths.ControlSocket = expand(c.Paths.ControlSocket)
	c.Remote.IdentityFile = expand(c.Remote.IdentityFile)
	c.Remote.KnownHostsFile = expand(c.Remote.KnownHostsFile)
}

func expand(p string) string {
	if p == "" {
		return p
	}
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	p = os.ExpandEnv(p)
	return p
}

func (c *Config) validate() error {
	if c.Paths.Backing == "" || c.Paths.Mount == "" {
		return errors.New("paths.backing and paths.mount must both be set")
	}
	if abs1, err := filepath.Abs(c.Paths.Backing); err == nil {
		c.Paths.Backing = filepath.Clean(abs1)
	}
	if abs2, err := filepath.Abs(c.Paths.Mount); err == nil {
		c.Paths.Mount = filepath.Clean(abs2)
	}
	if c.Paths.Backing == c.Paths.Mount {
		return errors.New("paths.backing and paths.mount must differ")
	}
	if pathContains(c.Paths.Backing, c.Paths.Mount) || pathContains(c.Paths.Mount, c.Paths.Backing) {
		return errors.New("paths.backing and paths.mount must not nest")
	}
	switch c.Sync.OnConflict {
	case "abort", "local-wins":
	default:
		return fmt.Errorf("sync.on_conflict: must be 'abort' or 'local-wins' (got %q)", c.Sync.OnConflict)
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level: must be one of debug|info|warn|error (got %q)", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "text", "json":
	default:
		return fmt.Errorf("logging.format: must be 'text' or 'json' (got %q)", c.Logging.Format)
	}
	if c.Remote.Host != "" {
		parent := filepath.Dir(c.Remote.KnownHostsFile)
		if info, err := os.Stat(parent); err != nil || !info.IsDir() {
			if err := os.MkdirAll(parent, 0o700); err != nil {
				return fmt.Errorf("known_hosts parent dir %s: %w", parent, err)
			}
		}
	}
	return nil
}

func pathContains(parent, child string) bool {
	p := filepath.Clean(parent) + string(filepath.Separator)
	return strings.HasPrefix(filepath.Clean(child)+string(filepath.Separator), p)
}

// RemoteEnabled returns whether the remote section is configured to talk to a host.
func (c Config) RemoteEnabled() bool {
	return strings.TrimSpace(c.Remote.Host) != ""
}

// HostUser returns the user portion of remote.host, defaulting to the local user.
func (c Config) HostUser() string {
	h := c.Remote.Host
	if i := strings.Index(h, "@"); i >= 0 {
		return h[:i]
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return ""
}

// HostName returns the host portion of remote.host (without user@).
func (c Config) HostName() string {
	h := c.Remote.Host
	if i := strings.Index(h, "@"); i >= 0 {
		return h[i+1:]
	}
	return h
}

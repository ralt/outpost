package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	c := Defaults()
	if c.Remote.Port != 22 {
		t.Errorf("default port = %d", c.Remote.Port)
	}
	if c.Sync.OnConflict != "abort" {
		t.Errorf("default on_conflict = %q", c.Sync.OnConflict)
	}
	if !strings.HasSuffix(c.Paths.Mount, ".claude") {
		t.Errorf("default mount = %q", c.Paths.Mount)
	}
}

func TestLoadMissing(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.ini"))
	if err != nil {
		t.Fatalf("Load missing should be ok: %v", err)
	}
	if c.Remote.Host != "" {
		t.Errorf("host should be empty by default")
	}
}

func TestLoadAndValidate(t *testing.T) {
	d := t.TempDir()
	cfg := filepath.Join(d, "config.ini")
	body := `[paths]
backing = ` + d + `/data
mount   = ` + d + `/mount

[remote]
host = alice@example.com
port = 2222

[sync]
on_conflict = local-wins
background_interval = 30m
`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfg)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Remote.Host != "alice@example.com" || c.Remote.Port != 2222 {
		t.Errorf("remote not parsed: %+v", c.Remote)
	}
	if c.Sync.OnConflict != "local-wins" {
		t.Errorf("on_conflict not parsed: %q", c.Sync.OnConflict)
	}
}

func TestRejectsNestedPaths(t *testing.T) {
	d := t.TempDir()
	cfg := filepath.Join(d, "config.ini")
	// mount inside backing → should be rejected
	body := `[paths]
backing = ` + d + `
mount = ` + d + `/inside
`
	_ = os.WriteFile(cfg, []byte(body), 0o600)
	if _, err := Load(cfg); err == nil {
		t.Fatal("expected nested-path error")
	}
}

package assets

import (
	"embed"
	"io/fs"
)

//go:embed commands/*.md
var commandsFS embed.FS

//go:embed systemd/*
var systemdFS embed.FS

// Commands returns a map of "<name>.md" → file bytes for every virtual slash
// command shipped in the binary.
func Commands() map[string][]byte {
	out := map[string][]byte{}
	entries, err := fs.ReadDir(commandsFS, "commands")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := fs.ReadFile(commandsFS, "commands/"+e.Name())
		if err != nil {
			continue
		}
		out[e.Name()] = b
	}
	return out
}

// SystemdUnit returns the embedded outpost.service template.
func SystemdUnit() ([]byte, error) {
	return fs.ReadFile(systemdFS, "systemd/outpost.service")
}

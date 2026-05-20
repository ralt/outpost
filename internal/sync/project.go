package sync

import (
	"strings"
	"time"
)

// Owner enum.
const (
	OwnerLocal  = "local"
	OwnerRemote = "remote"
)

// RemoteState enum.
const (
	StateClonePending  = "clone-pending"
	StateCloneFailed   = "clone-failed"
	StateSynced        = "synced"
	StateDirtyRemote   = "dirty-remote"
	StateNotApplicable = "not-applicable"
)

// Project is the daemon's in-memory view of a project. Persisted form lives
// in projectmeta.Meta. We keep the runtime view richer (e.g. streaming
// worker handles).
type Project struct {
	Munged         string
	Path           string
	IsGit          bool
	Owner          string
	RemoteState    string
	Streaming      bool
	ActiveBranch   string
	LastMirrorPush time.Time
	LastError      string
	Sessions       map[string]SessionMeta
}

// SessionMeta captures one headless agent on the remote.
type SessionMeta struct {
	PID       int       `json:"pid"`
	Log       string    `json:"log"`
	StartedAt time.Time `json:"started_at"`
}

// MungedFromCwd computes the Claude-style project name from an absolute cwd.
// /home/alice/foo → -home-alice-foo. Claude Code also replaces '.' in path
// components, so /home/alice/github.com → -home-alice-github-com.
func MungedFromCwd(cwd string) string {
	cwd = strings.TrimPrefix(cwd, "/")
	cwd = strings.ReplaceAll(cwd, "/", "-")
	cwd = strings.ReplaceAll(cwd, ".", "-")
	return "-" + cwd
}

// CwdFromMunged is the inverse. Ambiguity around literal '-' and '.' in path
// components is unresolvable from the munged name alone; callers must verify
// the result exists on disk.
func CwdFromMunged(name string) string {
	return "/" + strings.ReplaceAll(strings.TrimPrefix(name, "-"), "-", "/")
}

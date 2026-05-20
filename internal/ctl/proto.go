package ctl

import "encoding/json"

// Request is a JSON-line RPC request: one request per connection.
type Request struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
}

// Response is the matching reply, sent before the daemon closes the conn.
type Response struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
	Code  string          `json:"code,omitempty"`
	Req   string          `json:"req,omitempty"`
}

// Error codes, surfaced by the daemon and rendered by the CLI.
const (
	CodeNoGit           = "NO_GIT"
	CodeNoCommits       = "NO_COMMITS"
	CodeNoRemote        = "NO_REMOTE"
	CodeDisconnected    = "DISCONNECTED"
	CodeHomeMismatch    = "HOME_MISMATCH"
	CodeInProgressOp    = "IN_PROGRESS_OP"
	CodeAlreadySent     = "ALREADY_SENT_AWAY"
	CodeNotSentAway     = "NOT_SENT_AWAY"
	CodeLocalDirty      = "LOCAL_DIRTY"
	CodeRemoteDiskFull  = "REMOTE_DISK_FULL"
	CodeRemoteReject    = "REMOTE_REJECT"
	CodeRemoteMissing   = "REMOTE_MISSING_BIN"
	CodeApplyFailed     = "APPLY_FAILED"
	CodeAuthNoKeys      = "AUTH_NO_KEYS"
	CodeKnownHostsBad   = "KNOWN_HOSTS_MISMATCH"
	CodeBusy            = "BUSY"
	CodeBadArgs         = "BAD_ARGS"
	CodeUnknownMethod   = "UNKNOWN_METHOD"
	CodeInternal        = "INTERNAL"
	CodeNeedsConfirm    = "NEEDS_CONFIRM"
	CodeCloneFailed     = "CLONE_FAILED"
	CodeConfigError     = "CONFIG_ERROR"
)

// ── method-specific args + data payloads ────────────────────────────

type SendAwayArgs struct {
	Cwd     string `json:"cwd"`
	Session string `json:"session,omitempty"`
}

type AgentInfo struct {
	Session string `json:"session"`
	PID     int    `json:"pid"`
	Log     string `json:"log"`
	State   string `json:"state,omitempty"` // started|updated
}

type SendAwayResult struct {
	Project        string      `json:"project"`
	Agents         []AgentInfo `json:"agents"`
	BootstrapState string      `json:"bootstrap_state,omitempty"`
	MirrorPushRefs int         `json:"mirror_push_refs,omitempty"`
	UncommittedHunks int       `json:"uncommitted_hunks,omitempty"`
	Untracked      int         `json:"untracked,omitempty"`
	Branch         string      `json:"branch,omitempty"`
	Head           string      `json:"head,omitempty"`
}

type BringBackArgs struct {
	Cwd       string `json:"cwd"`
	Confirmed bool   `json:"confirmed,omitempty"`
}

type BringBackResult struct {
	Project        string `json:"project"`
	CommitsPulled  int    `json:"commits_pulled"`
	Hunks          int    `json:"hunks"`
	Untracked      int    `json:"untracked"`
	Sessions       int    `json:"sessions"`
	NeedsConfirm   bool   `json:"needs_confirm,omitempty"`
	WillOverwrite  int    `json:"will_overwrite,omitempty"`
}

type StatusResult struct {
	Mount struct {
		Path    string `json:"path"`
		Mounted bool   `json:"mounted"`
	} `json:"mount"`
	Backing      string `json:"backing"`
	Remote       struct {
		Connected bool   `json:"connected"`
		Host      string `json:"host"`
		Home      string `json:"home,omitempty"`
		LastError string `json:"last_error,omitempty"`
	} `json:"remote"`
	Projects int    `json:"projects"`
	Version  string `json:"version"`
}

type ProjectInfo struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	IsGit         bool   `json:"is_git"`
	Sessions      int    `json:"sessions"`
	LatestSession string `json:"latest_session,omitempty"`
	Owner         string `json:"owner"`
	RemoteState   string `json:"remote_state"`
	Streaming     bool   `json:"streaming"`
}

type ProjectsResult struct {
	Projects []ProjectInfo `json:"projects"`
}

# 03 — Config [locked]

## File location

`$XDG_CONFIG_HOME/outpost/config.ini`
(fallback `~/.config/outpost/config.ini`).

A missing file is fine — the daemon runs with defaults and just has no
remote configured. `outpost send-away` errors with an explicit message
in that case.

## Schema (INI)

```ini
[paths]
# Where the real data lives. The FUSE mount mirrors this directory.
# Default: $XDG_DATA_HOME/outpost/data
backing = /home/alice/.local/share/outpost/data

# Where the FUSE mount is exposed. Almost always ~/.claude.
mount = /home/alice/.claude

# Unix socket the daemon listens on for CLI clients.
# Default: $XDG_RUNTIME_DIR/outpost.sock
control_socket = /run/user/1000/outpost.sock

[remote]
# ssh-style "user@host" or just "host". Empty disables remote sync.
host = alice@dev-box.example.com

# ssh port. Default 22.
port = 22

# Private key. If empty, falls back to ssh-agent, then default key files
# (~/.ssh/id_ed25519, ~/.ssh/id_rsa) in that order.
identity_file = /home/alice/.ssh/id_ed25519

# TOFU known_hosts file. First connection pins the host key here;
# subsequent connections verify. Default ~/.ssh/known_hosts.
known_hosts_file = /home/alice/.ssh/known_hosts

# Path of the claude binary on the remote. Default "claude".
claude_bin = claude

# Keepalive interval for the persistent SSH connection. 0 disables.
# Default 30s.
keepalive_interval = 30s

# Prompt passed to `claude -p` when resuming on the remote. The model
# already has full session context; this just nudges it to keep going.
# Default: "continue".
continue_prompt = continue

# Comma-separated tool allowlist for headless claude. See Claude Code
# docs for the full tool catalogue. Default:
# "Bash,Read,Edit,Write,Glob,Grep,WebFetch".
allowed_tools = Bash,Read,Edit,Write,Glob,Grep,WebFetch

[sync]
# What to do when the remote worktree has its own uncommitted state that
# would conflict with reconcile: "abort" (refuse) or "local-wins"
# (force-overwrite). Default: abort.
on_conflict = abort

# Copy untracked-but-not-gitignored files at send-away time. Default: true.
send_untracked = true

# Background scheduler tick. Mirror pushes every project once per interval.
# 0 disables the tick (clones still happen on discovery, no periodic
# refresh). Default: 1h.
background_interval = 1h

# How long the watcher waits after the first event for a new project
# before firing the initial clone. Default: 5s.
discovery_debounce = 5s

[logging]
# debug | info | warn | error. Default: info.
level = info

# text | json. Default: text.
format = text
```

The daemon logs only to stderr. Persistence is the supervisor's job —
journald handles it under `systemd --user`. See
[10-logging.md](10-logging.md) for what gets logged.

## Defaults summary

| Key                            | Default                              |
| ------------------------------ | ------------------------------------ |
| `paths.backing`                | `$XDG_DATA_HOME/outpost/data`    |
| `paths.mount`                  | `~/.claude`                          |
| `paths.control_socket`         | `$XDG_RUNTIME_DIR/outpost.sock`  |
| `remote.host`                  | *(empty — remote disabled)*          |
| `remote.port`                  | `22`                                 |
| `remote.identity_file`         | *(empty — agent + default keys)*     |
| `remote.known_hosts_file`      | `~/.ssh/known_hosts`                 |
| `remote.claude_bin`            | `claude`                             |
| `remote.keepalive_interval`    | `30s`                                |
| `remote.continue_prompt`       | `continue`                           |
| `remote.allowed_tools`         | `Bash,Read,Edit,Write,Glob,Grep,WebFetch` |
| `sync.on_conflict`             | `abort`                              |
| `sync.send_untracked`          | `true`                               |
| `sync.background_interval`     | `1h`                                 |
| `sync.discovery_debounce`      | `5s`                                 |
| `logging.level`                | `info`                               |
| `logging.format`               | `text`                               |

## Reload semantics

- Daemon reads config once at startup.
- Reload is opt-in: `outpost reload` triggers a reread + reconnect.
- A bad config (parse error, unknown key) is fatal at startup but a no-op
  on reload — the daemon keeps running with the last good config and
  logs loudly.

## Validation

On load:

- Resolve `~` and env vars in path fields.
- `paths.backing` and `paths.mount` must differ; neither may contain the
  other.
- `remote.known_hosts_file`: if `remote.host` is non-empty, the parent
  directory must exist and be writable. The file itself is created on
  first connect if absent (TOFU; see [05](05-ssh-transport.md)).
- `sync.on_conflict` ∈ {`abort`, `local-wins`}.
- Durations (`keepalive_interval`, `background_interval`,
  `discovery_debounce`) parse as Go durations.
- `logging.level` ∈ {`debug`, `info`, `warn`, `error`}.
- `logging.format` ∈ {`text`, `json`}.

## Library

`gopkg.in/ini.v1`. Mature, handles the comment + section shape we use;
we don't lean on its more exotic features.

## What the config does NOT contain

- No project allowlist/denylist (v1: every project under `projects/` is
  fair game for `send-away` if it has a git repo).
- No per-project remote (one host for the whole daemon).
- No secrets. SSH auth uses agent or key files; no passphrases in config.
- No file-logging knobs. The daemon writes to stderr; if you want
  persistence, run under systemd (journald) or redirect yourself.

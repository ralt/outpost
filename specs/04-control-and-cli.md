# 04 — Control socket and CLI [locked]

## Why a control socket

CLI subcommands (`send-away`, `status`, …) need to talk to the running
daemon to use its single persistent SSH connection and to avoid races
between concurrent invocations. A unix socket gives us local-only auth
(filesystem perms), zero handshake cost, and easy framing.

## Socket

- Path: `paths.control_socket` from config; default
  `$XDG_RUNTIME_DIR/outpost.sock`.
- Permissions `0600`, owner = the user.
- The daemon `unlink`s a stale socket on start, then `listen()`s. If
  another daemon is already running, the new one refuses with a clear
  message.

## Wire format

JSON-lines RPC. One request per connection, one response, close.

Request:

```json
{"method":"send-away","args":{"cwd":"/home/alice/Git/.../outpost"}}
```

Response (success):

```json
{"ok":true,"data":{"project":"-home-alice-...","agents":[{"session":"4f8a2e1c","pid":31204,"log":"/home/alice/.local/share/outpost/logs/-home-alice-.../4f8a2e1c.log"}]}}
```

Response (error):

```json
{"ok":false,"error":"no git repo at /home/alice/...","code":"NO_GIT"}
```

Errors carry a stable `code` string so the CLI can render user-friendly
output without screen-scraping.

## Methods

### `send-away`

Args:
- `cwd` (string, required) — absolute path of the directory the caller
  invoked from. Identifies the Claude project.
- `session` (string, optional) — explicit session id; if absent the
  daemon picks the most recently modified `.jsonl` in the project dir.

Returns:
- `project` — project directory name (same on both sides).
- `agents` — array of `{session, pid, log}` for each headless agent
  the daemon launched (or no-op'd because it was already running).
- `bootstrap_state` (optional) — set to `BOOTSTRAPPED_INLINE` if the
  pipeline had to do an inline bare-init / mirror / worktree-add for
  this project (rare; new project).

The remote working-tree path equals `cwd` (matching `$HOME` is
required — see [06](06-project-sync.md)), so it's not on the wire.

Side effects: see [07-send-away.md](07-send-away.md).

### `bring-back`

Mirror of `send-away` in reverse. Always explicit — the daemon never
fires this on its own. Pre-flight check: `owner=remote` (else
`NOT_SENT_AWAY`).

**Lossy semantics:** any local-side appends to existing session
`.jsonl` files made between send-away and bring-back are overwritten
by the remote's copies. The CLI surfaces this with a y/N
confirmation; see [06 §"Project ownership"](06-project-sync.md).

Args:
- `cwd` (string, required).
- `confirmed` (bool, optional, default `false`) — set by the CLI after
  the user has answered y to the data-loss prompt (or passed `--yes`).
  Without this, the RPC returns a `data` payload describing what
  *would* be discarded and does nothing else, so the CLI can render
  the prompt.

Behaviour:

1. **Stop the remote agents.** For each PID recorded in
   `.meta/<munged>.json` under `sessions.*.pid`: SIGTERM, wait up to
   5s, then SIGKILL if still alive. The headless agents stop writing.
2. **go-git fetch** from the remote bare into the local repo, then
   fast-forward the matching local branch. No shell `git pull`.
3. Ship the remote's **uncommitted delta** back: ssh sessions run
   `git -C <cwd> diff --binary HEAD` and
   `git -C <cwd> ls-files --others --exclude-standard -z | tar -c …`;
   the daemon pipes their stdout into local `git apply --index` and
   `tar -x` (shelled out — local computation, not protocol, same
   rationale as send-away).
4. sftp every `.jsonl` from remote `~/.claude/projects/<munged>/` back
   into the local project dir, overwriting same-id files.
5. Flip `owner` → `local` in `.meta/`; clear `sessions` PIDs.

Returns:
- `project`, plus counts: `commits_pulled`, `hunks`, `untracked`,
  `sessions`.

Preconditions: local working tree must be clean. If the user edited
locally between send-away and bring-back (violating the contract — see
[06 §"Project ownership"](06-project-sync.md)), the daemon refuses
with `LOCAL_DIRTY`; the remediation is to commit/stash by hand, or to
edit `~/.local/share/outpost/.meta/<munged>.json` to flip
`owner=local` if the user wants to give up on the remote work
entirely.

### `status`

No args. Returns:
- `mount` — mountpoint path and `mounted: true/false`.
- `backing` — backing path.
- `remote.connected` — `true`/`false`.
- `remote.host`.
- `remote.home` — remote `$HOME`, learned at connect time. Equal to
  local `$HOME` if the daemon is healthy; a mismatch shows up here as
  the cause of `HOME_MISMATCH` in `remote.last_error`.
- `remote.last_error` — most recent SSH-layer error, if any.
- `projects` — count of detected projects.
- `version` — daemon binary version.

### `projects`

No args. Returns an array:

```json
[
  {"name":"-home-alice-...","path":"/home/alice/...","is_git":true,"sessions":3,"latest_session":"abc.jsonl","remote_state":"synced"},
  ...
]
```

### `reload`

No args. Re-reads config; if `remote.*` changed, tears down and
re-establishes the SSH connection.

## CLI binary

One binary, multiple subcommands:

```
outpost daemon              # run the daemon (long-running)
outpost send-away           # client: trigger remote handoff
outpost bring-back          # client: pull state back
outpost status              # client: print status
outpost projects            # client: list projects
outpost reload              # client: reload config
outpost logs                # client: tail logs (see 10)
outpost config-path         # print which config file was loaded
outpost version             # print version
outpost help [cmd]          # usage
```

All client subcommands:

- Discover the socket from the same config-loading logic as the daemon.
- Default output is human-readable on stdout. With `--json`, emit the
  raw RPC response object on stdout instead. Errors always go to stderr.
- Exit codes: `0` success, `1` daemon-reported error (RPC returned
  `ok=false`), `2` usage error, `3` daemon not running (with a hint
  pointing at the systemd unit).

## Sync vs background

`send-away` blocks until every headless agent has been launched and
its PID captured. The CLI does not return early — this lets the model
running inside Claude's bash tool see real success/failure.

`bring-back` blocks until refs are fetched, the delta is applied, and
sessions are copied.

`reload` blocks until the new SSH connection is up (or fails).

`status` and `projects` are read-only and never block on the remote.

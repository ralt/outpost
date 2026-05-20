# 06 — Project sync [locked]

## What "project" means here

A Claude project is a subdirectory of `<mount>/projects/` named after the
cwd Claude was launched in, with `/` replaced by `-`. Example:

```
cwd       : /home/alice/Git/github.com/alice/outpost
proj dir  : <mount>/projects/-home-alice-Git-github-com-alice-outpost/
```

Inside the project dir, Claude writes `.jsonl` session transcripts plus
metadata. We don't modify them; we just ship them.

## Mapping project name ↔ working tree

```
project_name := "-" + strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
working_tree := "/" + strings.ReplaceAll(strings.TrimPrefix(project_name, "-"), "-", "/")
```

Ambiguity: a real path component containing `-` is indistinguishable from
a `/` after the round-trip. We accept this — it's how Claude itself names
projects, so any ambiguity already exists in their store. When the
reconstructed path doesn't exist or isn't a git repo, `send-away` fails
with a clear error pointing at the resolved path.

## Requirement: matching `$HOME`

Session `.jsonl` files contain absolute path strings baked in at recording
time (tool inputs/outputs, shell cwds, file references), and the project
name itself is the cwd with `/` → `-`. For `claude --resume` on the
remote to find its session and have those paths still resolve, the
remote cwd must be **byte-identical** to the local cwd.

That implies:

- `remote $HOME == local $HOME` (the daemon checks this at connect time
  by running `echo $HOME` in an `ssh.Session` and comparing).
- Every absolute path the local user works under (including `$HOME`
  itself and any project cwds outside `$HOME`) exists and is writable
  on the remote as the ssh user.

In practice: **same username, same OS family** (both Linux with
`/home/<user>`, or both macOS with `/Users/<user>`). Setups that achieve
the same `$HOME` by other means (custom passwd entry, NFS-mounted homes,
bind mounts) also work — the daemon checks the value, not the username.

If the homes don't match, the daemon refuses to operate on the remote
and `outpost status` surfaces
`remote.last_error = HOME_MISMATCH: local=… remote=…`. No fallback or
auto-rewrite.

## Remote layout

Paths are reproduced exactly. For a local cwd of
`/home/alice/Git/github.com/alice/outpost`:

```
/home/alice/Git/github.com/alice/outpost/        ← linked worktree
        .git                                           pointer file →
                                                       …/repos/<munged>.git/worktrees/main

/home/alice/.local/share/outpost/
├── repos/
│   └── -home-alice-Git-github-com-alice-outpost.git/      ← bare mirror
│       └── (all branches, all tags, full repo)
├── logs/
│   └── -home-alice-Git-github-com-alice-outpost/         ← headless agent logs
│       ├── 4f8a2e1c.log
│       └── 9c7d1f3b.log
└── .meta/
    └── -home-alice-Git-github-com-alice-outpost.json
        { "owner": "local|remote",
          "last_mirror_push": "...",
          "active_branch": "...",
          "origin_cwd":     "/home/alice/Git/github.com/alice/outpost",
          "sessions": {
            "4f8a2e1c": { "pid": 31204, "log": ".../4f8a2e1c.log", "started_at": "..." },
            "9c7d1f3b": { "pid": 31207, "log": ".../9c7d1f3b.log", "started_at": "..." }
          } }

/home/alice/.claude/projects/
        -home-alice-Git-github-com-alice-outpost/          ← session files
        -home-alice-Git-github-com-alice-outpost/<id1>.jsonl
        -home-alice-Git-github-com-alice-outpost/<id2>.jsonl
        …
```

The munged project name is the same on both sides (because the cwd is
the same), so the Claude session resumes cleanly.

## Remote storage: bare mirror + linked worktree

The user wants the *whole* local repo on the remote, not just the
currently checked-out tip. That precludes the "just push HEAD" model:

- `git push HEAD:HEAD` only updates one branch.
- `git push --all` skips tags; `git push --mirror` covers everything
  but doesn't play well with a non-bare receiver (`denyCurrentBranch`
  rejects updates to the checked-out branch even with `updateInstead`
  when multiple refs change in unrelated ways).

The solution is to separate "where refs live" from "where the working
tree lives":

```
local repo                  ssh                  remote
─────────────────                                ────────────────────
.git/refs/* ─── git push --mirror ──► repos/<munged>.git/refs/*
                                              │
                                              │ git worktree add
                                              ▼
                                      <remote_path>/.git (pointer)
                                      <remote_path>/<files>
```

Mirror push semantics:

- All refs under `refs/heads/`, `refs/tags/`, and any custom namespaces
  are pushed.
- Refs that exist on the bare mirror but not locally get **deleted**
  on the mirror — sync semantics, matching what most users mean by
  "sync".
- The bare repo has no working tree, so `denyCurrentBranch` doesn't
  apply.

Worktree reconcile semantics (only at send-away time):

- `git switch <local_branch>` then `git reset --hard <local_branch>`.
  Any prior remote uncommitted state is discarded, governed by
  `sync.on_conflict` (`abort` default, `local-wins` to force).

The split also gives us cheap branch switching — `git switch` inside
the worktree is just a ref move because every commit is already in the
bare's object DB.

## Discovery & background sync

Cloning and keeping the remote in sync is the daemon's *steady-state*
job, not something `send-away` does on demand:

1. **Watch.** fsnotify on `<backing>/projects/` fires when Claude
   creates a new project directory. (fsnotify on the backing dir, not
   the FUSE mount — backing is a normal kernel filesystem so events
   are reliable.)
2. **Debounce.** Wait `sync.discovery_debounce` (default 5s) before
   doing anything; avoids firing 50 syncs while Claude is writing the
   project skeleton.
3. **Reconstruct.** Map project name → cwd. If the cwd doesn't exist
   or isn't a git repo, mark the project `non-git` and skip background
   sync. It stays eligible for `outpost projects` and `bring-back`.
4. **Init-or-mirror-push** over the shared `ssh.Client` (see
   [05](05-ssh-transport.md)):
   - First time:
     a. `git init --bare ~/.local/share/outpost/repos/<munged>.git`
     b. **go-git** `Remote.Push` with refspec `+refs/*:refs/*`.
     c. `git -C <bare> worktree add <remote_path> <local_branch>`.
     d. Record the project in `.meta/`.
   - Subsequent: **go-git** mirror push to the same bare.
5. **Refresh.** Every `sync.background_interval` (default 1h), walk
   every known git project **whose `owner=local`** and run step 4
   again. Projects in `owner=remote` are skipped — the remote is the
   live copy, and pushing stale local refs would lose its work.

Failures are logged, surfaced through `outpost status`, and retried
with exponential backoff (cap: the next scheduled refresh). They don't
block `send-away`; send-away does its own inline retry.

`outpost projects` lists every directory in `<mount>/projects/`
and for each one reports:

- `name`, reconstructed `path`
- `is_git` — whether `path/.git` exists
- `sessions` — count of `*.jsonl`
- `latest_session` — newest `.jsonl` by mtime
- `owner` — `local` or `remote`
- `remote_state` — `clone-pending` | `clone-failed` | `synced` (with
  last-refresh timestamp) | `dirty-remote` | `not-applicable`
- `streaming` — `true`/`false` (see §"Continuous session streaming")

## Continuous session streaming

The FUSE layer doubles as a tap: every write under
`<mount>/projects/<munged>/*.jsonl` is also forwarded over the shared
`ssh.Client` and appended to the matching file on the remote. This
runs continuously for every project in `owner=local`. By the time the
user fires `send-away`, the remote already has every session byte the
local side has — send-away ships **zero session data** over the wire.

**Mechanism:**

- FUSE hooks `open()`, `write()`, `create()`, and `unlink()` on the
  matching paths. The hook captures byte offset + length per write.
- A per-project streaming worker coalesces nearby writes (100ms
  debounce) and forwards them as `sftp.WriteAt(...)` calls into
  `~/.claude/projects/<munged>/<id>.jsonl` on the remote. New files
  are created on first write; deletes are mirrored.
- A small (1 MB) ring buffer of pending bytes survives short ssh
  outages — on reconnect the worker replays anything unacknowledged.
  If the buffer overflows, the project is flagged `streaming=false`
  and the next `send-away` repairs by full re-ship (see
  §"Session shipment").

**When streaming runs:**

- `owner == local`
- `remote_state ∈ {synced, dirty-remote}` (bare and worktree exist)
- SSH connection up

If any is false, the stream pauses; writes still hit the backing dir
normally, and `send-away` will catch up. The stream is an
**optimisation, not a correctness gate**.

## Project ownership

At any given time, every known project has exactly one **owner**:

- `local` — the user is working on this project on the laptop. The
  remote (if a bare mirror exists) is a frozen mirror, periodically
  refreshed from local refs; session writes are streamed to the remote
  continuously.
- `remote` — the user has sent this project away. The remote is now
  the live copy. Local writes are **not blocked**, but session-file
  streaming pauses and any local changes during this window —
  session-file appends, working-tree edits — will be **discarded** on
  `bring-back`.

Every project's owner is tracked independently. A user may have many
projects in flight: some `local`, some `remote`, in any combination.
Send-away and bring-back operate on a single project at a time.

The owner is stored in `.meta/<munged>.json` on the remote (and a
small local cache). Transitions:

```
                send-away
        local ─────────────► remote
              ◄─────────────
                bring-back
```

**Implications:**

- `send-away` requires `owner=local` (or freshly-discovered, treated
  as local). Calling it on a project already in `owner=remote` fails
  with `ALREADY_SENT_AWAY`.
- `bring-back` requires `owner=remote`. Calling it on `owner=local`
  fails with `NOT_SENT_AWAY`.
- The background scheduler skips projects in `owner=remote`.
- Session streaming runs only while `owner=local`.

**Bring-back is destructive toward local-after-send-away state.**
The local claude that fired send-away keeps writing for a while
(finishing its turn, the user typing more, etc.); the daemon can't
tell those appends apart from "the user is back and wants to resume",
so we don't try. `bring-back` is therefore **explicit and lossy**: the
remote's `.jsonl` files overwrite local copies with matching ids, and
any local-after-send-away appends — including the trailing turns of
the session that fired send-away — are discarded. The CLI prompts
with a y/N before proceeding (or refuses without `--yes` when stdin
is not a tty); full bring-back flow lives in
[07](07-send-away.md).

**Recovery escape hatch.** If the remote is permanently unreachable
or the user wants to forcibly reset to `local` without a working
bring-back, they edit
`~/.local/share/outpost/.meta/<munged>.json` to flip `owner` back
to `local`. Documented as the manual path; no dedicated subcommand in
v1.

## Multiple sessions per project, multiple projects per user

Two independent multiplicities, both supported:

- **Multiple sessions in one project.** Local users routinely run
  several `claude` instances against the same working tree. Each
  produces a separate `.jsonl`. On the remote they become parallel
  headless processes — one `nohup claude -p ... --resume <id>` per
  session id, each with its own PID and log file. See
  [07](07-send-away.md) for the launch details.
- **Multiple projects, each with their own lifecycle.** A user
  typically has many Claude projects in flight at once. Each is
  tracked independently: own owner, own bare mirror, own worktree,
  own set of headless agent processes, own streaming worker. Projects
  A and B may be `local` (still streaming) while C is `remote` and D
  was never sent away at all. All operations are per-project.

## What `send-away` ships

Because the background scheduler keeps the remote bare mirror in sync
*and* the streaming worker keeps session files in sync, send-away has
very little to do at the moment of the slash command:

1. **Catch-up mirror push** (defensive, usually a no-op): go-git mirror
   push to the remote bare. Picks up any refs the scheduler hasn't
   reached yet.
2. **Drain the session stream** (a few hundred ms at most): flush
   pending bytes in the streaming worker's buffer, then pause
   streaming for this project.
3. **Worktree reconcile**: `git switch <local_branch>` then
   `git reset --hard <local_branch>` on the remote.
4. **Uncommitted state**: staged + unstaged + untracked-not-ignored.
   See §"Shipping the uncommitted delta".
5. **Owner flip** to `remote` in `.meta/`.
6. **Launch headless agents**: `nohup claude -p '<prompt>' --resume <id>`
   per session, backgrounded; PIDs recorded in `.meta/`.

If the background scheduler has never successfully bootstrapped this
project (brand-new project the daemon hasn't reached), the pipeline
performs bare-init + mirror push + `worktree add` synchronously
before step 1 and returns `STATE: BOOTSTRAPPED_INLINE`. Session files
are also shipped inline in that case (no streaming worker had been
established yet).

## Shipping the uncommitted delta

The local working tree at send-away time can contain three kinds of
uncommitted change:

- **Untracked** (not in `.gitignore`): files git is willing to add.
- **Unstaged**: modifications in tracked files not yet staged.
- **Staged**: in the index, not yet committed.

v1 ships all three in one pass:

1. **Local computation, shelled out**: `git diff --binary HEAD`
   captures staged + unstaged modifications to tracked files as a
   unified patch (binary chunks included).
   `git ls-files --others --exclude-standard -z` lists untracked
   files; piped into a local `tar`. Both are pure local work —
   shelling out gets us the user's exact git config and gitignore
   semantics. go-git is not in this path.
2. **Wire transfer over the shared `ssh.Client`**:
   - Patch → `ssh.Session("git -C <remote_path> apply --index --whitespace=nowarn")`
     with the patch on stdin.
   - Tarball → `ssh.Session("tar -x -C <remote_path>")` with the tar
     stream on stdin.
3. **Remote checks**: the remote worktree's pre-existing uncommitted
   state, if any, is governed by `sync.on_conflict`. `git apply --index`
   updates index + working tree atomically per file.
4. The remote is byte-identical to the local working tree as far as
   git sees it; `git status` on the remote shows the same dirty set.

Gitignored files are deliberately skipped (`node_modules`, `.venv`,
`dist/`, `.env`, …). `outpost` is not a secret-syncing tool.

The diff-apply approach (vs `git stash push`-the-stash) keeps the
local working tree untouched through send-away — no risk of leaving
the user with a half-applied stash if the network drops.

Partial-apply recovery is best-effort: rely on `git apply --index`
being reversible and retry on the next send-away.

## Session shipment (fallback only)

In the steady state, session files aren't "shipped" — they're streamed
continuously. Full re-shipment only happens in two cases:

- **First bootstrap** (new project, streaming hadn't been set up
  yet): full sftp upload of every `*.jsonl` in
  `<mount>/projects/<munged>/` during inline bootstrap.
- **Streaming-buffer-overflow recovery** (project flagged
  `streaming=false`): the next `send-away` re-uploads every `*.jsonl`
  in full to bring the remote back into parity.

When a full re-shipment runs, the RPC accepts `--session <id>`
(repeatable) and `--since <duration>` to narrow it; default is "all".

The daemon does **not** modify session content. Path strings inside
refer to the local cwd and stay as-is on the remote — matching `$HOME`
makes this safe.

## Rerun semantics

`send-away` is idempotent and non-destructive locally:

- Re-running picks up new commits, new uncommitted state, and any
  fresh session files.
- If a headless agent for a given session id is still running on the
  remote (its recorded PID responds to `kill -0`), the daemon does
  not re-launch it; the existing process keeps going. Re-running
  reports "updated" rather than "started".
- We read the index and working tree (via `git diff`, `git ls-files`);
  we don't commit, stash, or touch the working tree. If the network
  drops mid-flight the local repo is exactly as it was.

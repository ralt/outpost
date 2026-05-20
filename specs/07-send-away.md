# 07 — Send-away (and bring-back) [locked]

## User story

> **README seed.** When the project README is written, this user
> story is its opening. Phrasing here should read as well to a
> first-time visitor as it does in-context.

I'm pairing with Claude on my laptop. I need to step away — close the
lid, go to a meeting, get on a flight. I want the agent to keep
working **autonomously** on my configured dev box, picking up exactly
where we left off. When I'm back, I run `bring-back` and resume work
locally with the remote's progress applied. The agent on the remote
is fully headless — there's nothing to attach to, by design. "Work
while I'm away" is the whole point.

## Trigger paths

Two equivalent triggers, same RPC underneath:

- In Claude: type `/send-away`. The virtual `commands/send-away.md`
  tells Claude to run `outpost send-away`.
- In any shell: `outpost send-away`.

The slash command is the primary UX. Shell is the escape hatch.

**Mid-task hand-off.** If the local model is mid-turn (a long-running
tool call, a stream of edits), the user presses **`Esc`** to interrupt,
then types `/send-away`. Claude finalises the interrupted turn into the
session `.jsonl` with its standard interruption marker, then proceeds
to run send-away. `/btw <command>` does **not** do this — it's Claude
Code's side-questions overlay, not an interrupt-and-redirect.

The continuous session streaming (06 §"Continuous session streaming")
keeps the remote up-to-date right up to the slash command, so the
remote agent picks up from a session that includes the interrupted
turn and continues from there.

## The `send-away.md` slash-command markdown

Baked into the daemon binary and served as a virtual file
(see [02-fuse-overlay.md](02-fuse-overlay.md)):

```markdown
---
description: Push this project to your outpost remote and continue there headlessly.
---

Run this and show me the output verbatim:

```bash
outpost send-away
```

If it returns successfully, tell me which sessions were launched
headlessly and their PIDs. Then **stop**. Don't take any more turns
in this local session. The remote agent is taking over; anything
written to this `.jsonl` after this point will be discarded when I
run `bring-back`.
```

The "stop" instruction is what keeps trailing-local-writes to a
minimum: the model acknowledges the handoff in one terse turn and
goes quiet. The user then walks away and runs `outpost bring-back`
later to apply the remote's progress locally.

## End-to-end flow

Background prerequisite (steady-state machinery; see
[06 §"Discovery & background sync"](06-project-sync.md) and
[06 §"Continuous session streaming"](06-project-sync.md)):

```
[B1] fsnotify sees a new <backing>/projects/<munged>/ → debounce.
     If the reconstructed path is a git repo:
       - git init --bare on remote
       - go-git mirror push to remote bare
       - git worktree add at the reproduced absolute path
[B2] Every sync.background_interval, go-git mirror push to each
     known project's bare repo (only while owner=local).
[B3] FUSE write-stream forwards every session-file append to the
     remote (only while owner=local).
```

Foreground (triggered by the user / Claude):

```
[1] CLI: outpost send-away
        │
        │ unix-socket RPC
        ▼
[2] Daemon resolves the project from cwd
        - munged      = "-" + cwd_with_slashes_to_dashes
        - remote_path = cwd  (byte-identical; matching $HOME)
        │
        │ errors out on: NO_GIT / NO_COMMITS / NO_REMOTE /
        │   DISCONNECTED / HOME_MISMATCH / IN_PROGRESS_OP /
        │   ALREADY_SENT_AWAY
        ▼
[3] Daemon checks remote_state in .meta:
        ├─ clone-pending  → inline bootstrap (rare; new project)
        ├─ clone-failed   → surface error, refuse send-away
        └─ synced / dirty-remote → proceed
        │
        ▼
[4] Daemon ships the delta:
        - go-git mirror push to remote bare       ← catch-up, usually no-op
        - drain session stream + pause it
        - ssh: git -C <cwd> switch  <local_branch>
        - ssh: git -C <cwd> reset --hard <local_branch>
        - git diff --binary HEAD | ssh → git apply --index
        - tar(untracked-not-ignored) | ssh → tar -x
        │
        ▼
[5] Daemon flips owner → remote in .meta/<munged>.json
        │
        ▼
[6] Daemon launches headless agents (one per session, fully detached):
        - log_dir = ~/.local/share/outpost/logs/<munged>/
        - ssh: mkdir -p <log_dir>
        - For each session <id> not already tracked-and-alive:
            ssh: cd <cwd> && nohup <claude_bin> -p '<continue_prompt>' \
                   --resume <id> --allowedTools '<tools>' \
                   > <log_dir>/<id>.log 2>&1 < /dev/null & echo $!
            capture PID from ssh stdout; record under
            .meta/<munged>.json sessions.<id> = { pid, log, started_at }
        - For sessions whose recorded PID is still alive: noop
        │
        ▼
[7] CLI prints:
        ✓ Project: -home-alice-Git-github-com-alice-outpost
        ✓ Last mirror push: 17 min ago (caught up; 4 refs pushed now)
        ✓ Worktree reconciled to feature/x @ a3f9c1
        ✓ Uncommitted: 12 hunks across 4 files / 3 untracked files
        ✓ 2 agents launched headlessly:
            - 4f8a2e1c → PID 31204
            - 9c7d1f3b → PID 31207
        ✓ Run `outpost bring-back` when you're back.
```

If `[3]` falls into `clone-pending`, the pipeline performs the
bare-init + mirror push + `worktree add` synchronously between `[3]`
and `[4]`; session files are also shipped inline (full sftp upload)
since the streaming worker hadn't been established yet. The response
carries `bootstrap_state: BOOTSTRAPPED_INLINE` so the user knows it
took longer than usual.

## Preconditions

Checked in `[2]`/`[3]` before any remote work:

- Project's current owner is `local` (else `ALREADY_SENT_AWAY`).
- Working tree maps to the current project name AND is a git repo
  (else `NO_GIT`).
- Repo has at least one commit on the current branch (else
  `NO_COMMITS`).
- Remote is configured and connected; remote `$HOME` matches local
  (else `NO_REMOTE` / `DISCONNECTED` / `HOME_MISMATCH`).
- Working tree is not mid-rebase/merge/bisect (else `IN_PROGRESS_OP`).
- Background scheduler is not in `clone-failed` state for this
  project.

Error responses carry a structured code and a one-line remediation
hint:

```
ALREADY_SENT_AWAY : "project already sent away (owner=remote); run bring-back"
NO_GIT            : "cwd /home/alice/foo is not a git repo (run git init)"
NO_COMMITS        : "the current branch has no commits — commit at least once"
NO_REMOTE         : "set [remote] host in ~/.config/outpost/config.ini"
DISCONNECTED      : "ssh to dev-box.example.com is down: <last_error>"
HOME_MISMATCH     : "remote $HOME != local $HOME (see 05/06)"
IN_PROGRESS_OP    : "git is mid-rebase; finish or abort it first"
```

On success, the daemon flips `owner` to `remote` *before* `[6]`, so
a crash between flip and launch doesn't leave the project in an
ambiguous state — `outpost status` shows `owner=remote` and the user
knows to run `bring-back` or manually reset.

## Autonomous remote execution

The remote agent is launched in **headless mode** (`claude -p
<prompt> --resume <id>`), backgrounded with `nohup` so it survives
the ssh client disconnecting. No tmux, no screen, no PTY. The model:

- reads the resumed session,
- acts on `<continue_prompt>` (default `"continue"`),
- loops through tool calls autonomously,
- exits when it decides the task is done (or hits a hard error).

**Process lifecycle.**

- Launch captures `$!` via `ssh stdout` and stores it in
  `.meta/<munged>.json` under `sessions.<id> = { pid, log, started_at }`.
- stdout/stderr → `~/.local/share/outpost/logs/<munged>/<id>.log`
  on the remote. The session `.jsonl` (canonical transcript) is
  written by Claude Code as it works and pulled back on bring-back.
- `outpost status` checks `kill -0 <pid>` per session; a periodic
  background poll (every 30s) keeps the reported state fresh.
- `bring-back` sends `SIGTERM`, waits up to 5s, then `SIGKILL`.

**Interrupted tool calls.** If send-away fires mid-tool-call, Claude
Code's resume behaviour is to **re-attempt the tool from scratch**
rather than resume mid-execution. The model sees the previous
incomplete attempt in context and decides what to do.

**Tool allowlist.** Headless mode requires explicit `--allowedTools`.
Default: `Bash,Read,Edit,Write,Glob,Grep,WebFetch`. Tunable via
`remote.allowed_tools`.

**Continue prompt.** Default: `continue`. Short and deliberately
unopinionated — the model has full session context and is expected
to just keep going. Customisable via `remote.continue_prompt`.

**If the model needs input.** Headless mode has no way to ask. The
model either makes its best guess and proceeds, or stops with a
question in the session `.jsonl`. The user sees it on `bring-back`.

**Peeking at progress.** Out of scope as a workflow — the agent is
running so the user *doesn't* have to watch. Escape hatch: `ssh remote
tail -f ~/.local/share/outpost/logs/<munged>/<id>.log`. Not a
supported UI.

## Concurrency

Only one `send-away` runs at a time, daemon-wide. A second concurrent
call **fails fast** with `BUSY` rather than blocking — each call can
take 10s+, and queueing makes the second caller think their call is
hung.

## Bring-back

The reverse trip — **always explicit**, triggered by the user typing
`outpost bring-back` (or the `/bring-back` slash command). The daemon
does **not** watch the FS and auto-fire: the local claude that
initiated the send-away keeps writing for some time after, which
would make any FS-watch heuristic race against state we just sent.

**Bring-back is destructive toward local-after-send-away state.** The
remote's session `.jsonl` files overwrite the local copies with
matching ids. Any local appends made after send-away are discarded.
The CLI prompts y/N before proceeding (or refuses without `--yes`
when stdin is not a tty); see
[06 §"Project ownership"](06-project-sync.md).

**Preconditions.**

- `owner == remote` (else `NOT_SENT_AWAY`).
- Local working tree is clean (else `LOCAL_DIRTY`; user resolves
  manually, or edits `.meta/<munged>.json` to forfeit the remote
  work).

**Steps.**

1. **Stop the remote agents.** For each PID recorded in
   `.meta/<munged>.json` under `sessions.*.pid`: `SIGTERM`, wait up
   to 5s, `SIGKILL` if still alive. The headless agents stop writing.
2. **go-git fetch** from the remote bare into the local repo; fast-
   forward the matching local branch. No shell `git pull`.
3. **Ship the remote's uncommitted delta back**: ssh sessions run
   `git -C <cwd> diff --binary HEAD` and
   `git -C <cwd> ls-files --others --exclude-standard -z | tar -c …`;
   the daemon pipes their stdout into local `git apply --index` and
   `tar -x` (shelled out — local computation, not protocol).
4. sftp every `.jsonl` from `~/.claude/projects/<munged>/` on the
   remote back to the local project dir, overwriting same-id files.
5. Flip `owner` → `local` in `.meta/`; clear `sessions` PIDs.
6. Print, for each session id, the local resume command.

The remote bare mirror and worktree stay in place — a future
send-away will reconcile them.

## Failure-mode UX

If `send-away` fails midway:

- Daemon attempts to leave the remote in a *no worse* state. If
  `git push` fails, we don't ship untracked files or launch agents.
  If `tar -x` fails, we don't launch agents (state would be partial).
- Daemon returns the structured error to the CLI, which prints a
  one-line reason and a `Suggested fix:` line.
- Claude, seeing the non-zero exit + stderr, surfaces this to the
  user.

If `bring-back` fails midway, the local working tree may be in a
partial state (some files applied, some not). The daemon does not
attempt to roll back; the user resolves with `git status` and
`git checkout` / `git restore` as needed.

## Telemetry

No phone-home. Logging is local only — sinks, levels, components, and
per-RPC trace ids are spec'd in [10-logging.md](10-logging.md).

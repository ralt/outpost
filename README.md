# outpost

Hand off your Claude Code session to a remote machine so it can keep
working headlessly while you're away, then bring the result back when
you're ready.

> **⚠️ Abandoned.** Outpost's whole premise — run `claude -p --resume`
> on your dev box against the same plan you use locally — stops working
> once Anthropic rolls out the change that requires `claude -p` (headless
> mode) to authenticate with an Anthropic API key, separate from Pro /
> Max / Team plans. The "hand the laptop's session off to the cloud and
> keep going on the same subscription" trick is no longer available, so
> outpost has no remaining reason to exist. The code in this repo is a
> working snapshot of how far it got; it isn't going to be developed
> further. Specs and source are left up as a reference for anyone
> interested in the FUSE-overlay or single-SSH-client patterns.

I'm pairing with Claude on my laptop. I need to step away — close the
lid, go to a meeting, get on a flight. I want the agent to keep working
**autonomously** on my configured dev box, picking up exactly where we
left off. When I'm back, I run `bring-back` and resume work locally with
the remote's progress applied. The agent on the remote is fully
headless — there's nothing to attach to, by design. "Work while I'm
away" is the whole point.

That's the user story. Outpost is the daemon that makes it happen.

## What you get

Two slash commands inside Claude:

- **`/send-away`** — flips the project to "remote owns it", launches a
  headless `claude -p --resume` on your dev box, walks away.
- **`/bring-back`** — stops the remote agent, pulls back its progress
  (commits, uncommitted edits, updated session transcript), resumes
  locally.

Under the hood, outpost mounts a **FUSE filesystem** at `~/.claude/` —
every read and write passes straight through to a real directory on
disk at kernel speed. Never in your way, incredibly efficient.

## Why abandoned, in one paragraph

Outpost was built on a quiet assumption: a Pro / Max / Team
subscription to Claude Code authenticates `claude` *whether you run it
interactively or with `-p`*, and on *any* machine that subscription is
signed into. So if your laptop can run `claude` against your plan,
your dev box can too — same plan, no extra cost, no separate keys.
That made the whole "hand off to a remote, come back later" loop a
matter of plumbing (FUSE overlay, SSH transport, git mirror, session
streaming) rather than billing. Anthropic is closing that loophole:
headless `claude -p` will require an Anthropic API key, which is paid
per-token and not bundled with the subscription. Once that change is
live, sending a session "away" means paying twice — once for your
plan, once again per token for the headless agent — and the value
proposition collapses. Rather than pivot to "outpost, but bring your
own API budget," the project is being parked.

## What's still useful here

If you want to read the code for ideas, the bits that don't depend on
the auth model still apply:

- **FUSE passthrough with virtual overlay entries** — see
  [`internal/fusefs/fs.go`](internal/fusefs/fs.go) and
  [`specs/02-fuse-overlay.md`](specs/02-fuse-overlay.md).
- **Single persistent `ssh.Client` with TOFU, `$HOME` check, keepalive,
  and reconnect** — see [`internal/sshx/client.go`](internal/sshx/client.go)
  and [`specs/05-ssh-transport.md`](specs/05-ssh-transport.md).
- **fsnotify project watcher + go-git mirror push + sftp session
  streaming** — see [`internal/sync/`](internal/sync) and
  [`specs/06-project-sync.md`](specs/06-project-sync.md).

The build is still green and the dev-remote round trip still works
against the stub `claude` in [`scripts/dev-remote.sh`](scripts/dev-remote.sh)
if you want to poke at the mechanics.

## Design docs

The [`specs/`](specs/) directory has the locked design documents —
FUSE overlay, SSH transport, sync engine, send-away pipeline.

## License

TBD.

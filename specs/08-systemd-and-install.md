# 08 — systemd + install [locked]

## systemd user unit

File: `~/.config/systemd/user/outpost.service`

```ini
[Unit]
Description=outpost FUSE overlay + remote sync for ~/.claude
# Run after the user manager is fully up. ~/.claude is opened on first
# `claude` invocation, so we only need to be ready by then.
After=default.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=%h/.local/bin/outpost daemon
ExecStop=%h/.local/bin/outpost stop
Restart=on-failure
RestartSec=2

# The daemon needs FUSE; deny everything else we can.
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%h/.claude %h/.local/share/outpost %h/.cache/outpost %t
NoNewPrivileges=yes
PrivateTmp=yes

# FUSE-specific: we need the fuse device and the kernel's
# unprivileged-mount support (default on modern Linux distros).

[Install]
WantedBy=default.target
```

Notes:

- `Type=notify` plus `sd_notify("READY=1")` once the FUSE mount is up —
  this is what makes "the mount is ready before Claude starts" a
  guarantee rather than a race.
- `Restart=on-failure` handles transient crashes. We do NOT set
  `Restart=always`: if config is broken, repeated restarts thrash; we'd
  rather the unit sit in `failed` so `systemctl --user status` tells the
  user something is up.
- `outpost stop` is a thin wrapper that sends a clean-shutdown RPC,
  waits for unmount, then exits. Less brittle than `ExecStop=fusermount -u`
  which races the daemon's own shutdown path.

## Install script

`scripts/install.sh`. Run by the user as themselves (no sudo). Steps:

1. Verify prerequisites:
   - `fusermount3` (or `fusermount`) in PATH.
   - Kernel FUSE support: `[ -e /dev/fuse ]`.
   - `git`, `ssh`, `tar` in PATH locally. (No `tmux` requirement —
     the remote agents run as detached `nohup` processes.)
2. Build the binary:
   - `go build -o $HOME/.local/bin/outpost ./` (assumes the user has
     a Go toolchain). For users without Go, we ship a release binary too;
     install script picks whichever is available.
3. Migrate existing `~/.claude/` if needed:
   - If `~/.claude/` exists and isn't a mountpoint and isn't empty:
     a. Create backing dir.
     b. `mv ~/.claude/* ~/.claude/.* $BACKING/` (excluding `.` and `..`).
     c. Confirm `~/.claude/` is now empty.
   - If `~/.claude/` doesn't exist: just `mkdir -p` and continue.
4. Write a default `~/.config/outpost/config.ini` if absent. Includes
   a commented-out `[remote]` section so the user knows what to fill in.
5. Install the systemd unit:
   - `mkdir -p ~/.config/systemd/user`
   - Write `outpost.service` (from the template above).
   - `systemctl --user daemon-reload`
   - `systemctl --user enable --now outpost.service`
6. Smoke-check:
   - `ls -la ~/.claude/` returns the overlay (should show
     `commands/send-away.md`).
   - `outpost status` returns OK.

The script is idempotent — re-running it upgrades the binary, re-installs
the unit, and skips migration if the backing dir already contains data.

## Uninstall

`scripts/uninstall.sh` is the inverse of install — when it's done, the
user's `~/.claude/` is exactly the way it would have been if outpost
had never been installed:

1. `systemctl --user disable --now outpost.service`. The daemon's
   `ExecStop` runs the clean unmount; on exit, `~/.claude/` is an empty
   directory and the data lives in the backing dir.
2. `rm ~/.config/systemd/user/outpost.service` and
   `systemctl --user daemon-reload`.
3. **Un-migrate**: `mv $BACKING/* $BACKING/.[!.]* ~/.claude/ 2>/dev/null`,
   then `rmdir $BACKING` (its parent dirs too if empty). After this
   step:
   - `~/.claude/` looks like a normal Claude install with the user's
     real data.
   - The virtual entries (`commands/send-away.md`, etc.) are gone, as
     expected — they were never on disk.
   - Any user-authored files that *shadowed* a virtual entry are
     preserved (those lived in the backing dir, so they migrate back
     naturally).
4. Remove `$HOME/.local/bin/outpost`.
5. Config (`~/.config/outpost/config.ini`) is left in place by
   default. `--purge` removes config as well.

Edge cases:

- If `~/.claude/` is non-empty when un-migrate runs (it shouldn't be,
  because the daemon just unmounted), the script aborts with a message
  pointing at both directories and asks the user to resolve by hand.
  We don't merge — too risky.
- If the backing dir doesn't exist (someone deleted it manually while
  the daemon was off), uninstall skips step 3 and warns.

A `--no-unmigrate` flag skips step 3 entirely, leaving data in the
backing dir. Use case: the user is reinstalling immediately and doesn't
want the data shuffled twice.

## Logs

Daemon stdout/stderr → journald via the systemd unit. That's the only
log destination. If the user wants a durable file copy, that's a
journald-config concern (`SystemMaxUse=`, `Storage=persistent`, a
journal forwarder). See [10-logging.md](10-logging.md) for what gets
logged.

## Permissions / capabilities

- Unprivileged user-namespace FUSE mount. Works on Ubuntu 22.04+, Debian
  bookworm+, Fedora 36+, Arch — anywhere `user_allow_other` is the
  default. If a distro has FUSE locked down, install script detects this
  and prints a fix (typically `sudo modprobe fuse` + `/etc/fuse.conf`).
- No SUID, no setuid binary, no root operation needed.

## Failure recovery

If the daemon dies in a way that leaves a stale FUSE mount,
`Restart=on-failure` alone isn't enough — the stale mount can block
the new mount. Manual recovery:

```
fusermount -uz ~/.claude
systemctl --user restart outpost.service
```

This is rare in practice (clean shutdown via `ExecStop` is the normal
path) and we don't ship a dedicated subcommand for it in v1.

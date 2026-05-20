#!/usr/bin/env bash
# install.sh — install outpost as a `systemd --user` service.
# Run as your normal user; no sudo.
#
# Idempotent: re-runs upgrade the binary, re-install the unit, skip migration
# if the backing dir already has content.

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PREFIX="${PREFIX:-$HOME/.local}"
BIN_DIR="$PREFIX/bin"
SYSTEMD_DIR="$HOME/.config/systemd/user"
CONFIG_DIR="$HOME/.config/outpost"
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/outpost"
BACKING_DIR="$DATA_DIR/data"
MOUNT_DIR="$HOME/.claude"

say() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!! \033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m!! \033[0m %s\n' "$*" >&2; exit 1; }

# ── 1. Prereqs ─────────────────────────────────────────────────────
say "Checking prerequisites"
command -v fusermount3 >/dev/null 2>&1 || command -v fusermount >/dev/null 2>&1 || \
  die "fusermount(3) not found — install fuse3 (Debian/Ubuntu: apt install fuse3)"
[ -e /dev/fuse ] || die "/dev/fuse missing — kernel FUSE support is required"
for tool in git ssh tar; do
  command -v "$tool" >/dev/null 2>&1 || die "missing $tool in PATH"
done
command -v systemctl >/dev/null 2>&1 || die "systemctl not found — outpost expects systemd --user"

# ── 2. Build / install binary ──────────────────────────────────────
say "Building outpost"
mkdir -p "$BIN_DIR"
( cd "$REPO_DIR" && go build -o "$BIN_DIR/outpost" ./cmd/outpost )
say "Installed $BIN_DIR/outpost"

# ── 3. Migrate ~/.claude/ into backing dir if needed ───────────────
mkdir -p "$BACKING_DIR"
if [ -d "$MOUNT_DIR" ] && ! mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
  if [ -n "$(ls -A "$MOUNT_DIR" 2>/dev/null || true)" ] && [ -z "$(ls -A "$BACKING_DIR" 2>/dev/null || true)" ]; then
    say "Migrating existing $MOUNT_DIR into $BACKING_DIR"
    shopt -s dotglob
    mv "$MOUNT_DIR"/* "$BACKING_DIR"/
    shopt -u dotglob
  else
    say "Skipping migration — backing already populated or mount empty"
  fi
fi
mkdir -p "$MOUNT_DIR"

# ── 4. Default config ──────────────────────────────────────────────
mkdir -p "$CONFIG_DIR"
if [ ! -f "$CONFIG_DIR/config.ini" ]; then
  cat > "$CONFIG_DIR/config.ini" <<EOF
# outpost config — see specs/03-config.md for the full schema.

[paths]
# backing = $BACKING_DIR
# mount   = $MOUNT_DIR

# [remote]
# host = your-user@your-dev-box.example.com
# port = 22
# identity_file = $HOME/.ssh/id_ed25519

[sync]
on_conflict = abort
background_interval = 1h

[logging]
level = info
EOF
  say "Wrote default $CONFIG_DIR/config.ini"
else
  say "Config already exists at $CONFIG_DIR/config.ini — leaving alone"
fi

# ── 5. systemd unit ────────────────────────────────────────────────
mkdir -p "$SYSTEMD_DIR"
cat > "$SYSTEMD_DIR/outpost.service" <<'EOF'
[Unit]
Description=outpost FUSE overlay + remote sync for ~/.claude
After=default.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=%h/.local/bin/outpost daemon
ExecStop=%h/.local/bin/outpost stop
Restart=on-failure
RestartSec=2

ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%h/.claude %h/.local/share/outpost %h/.cache/outpost %t
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=default.target
EOF
say "Installed $SYSTEMD_DIR/outpost.service"

systemctl --user daemon-reload
systemctl --user enable --now outpost.service
say "Service enabled and started"

# ── 6. Smoke ───────────────────────────────────────────────────────
sleep 1
if ls "$MOUNT_DIR/commands/send-away.md" >/dev/null 2>&1; then
  say "Overlay is live (commands/send-away.md visible)"
else
  warn "Overlay not visible yet — check 'journalctl --user -u outpost.service -f'"
fi

"$BIN_DIR/outpost" status || warn "outpost status reported errors — see logs"

say "Done. Edit $CONFIG_DIR/config.ini to set [remote] host, then 'outpost reload'."

#!/usr/bin/env bash
# uninstall.sh — undo install.sh.
#
# When successful, ~/.claude/ looks exactly as it would if outpost
# had never been installed. Pass --purge to also drop ~/.config/outpost/.
# Pass --no-unmigrate to leave data inside the backing dir (useful when
# reinstalling immediately).

set -euo pipefail

PREFIX="${PREFIX:-$HOME/.local}"
BIN_DIR="$PREFIX/bin"
SYSTEMD_DIR="$HOME/.config/systemd/user"
CONFIG_DIR="$HOME/.config/outpost"
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/outpost"
BACKING_DIR="$DATA_DIR/data"
MOUNT_DIR="$HOME/.claude"

PURGE=0
UNMIGRATE=1
for a in "$@"; do
  case "$a" in
    --purge) PURGE=1 ;;
    --no-unmigrate) UNMIGRATE=0 ;;
    *) echo "unknown flag: $a" >&2; exit 2 ;;
  esac
done

say() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!! \033[0m %s\n' "$*" >&2; }

# 1. Stop the unit (which runs the clean unmount via ExecStop).
if systemctl --user list-unit-files | grep -q '^outpost.service'; then
  say "Stopping outpost.service"
  systemctl --user disable --now outpost.service || true
fi

# 2. Remove the unit file.
if [ -f "$SYSTEMD_DIR/outpost.service" ]; then
  rm -f "$SYSTEMD_DIR/outpost.service"
  systemctl --user daemon-reload || true
fi

# 3. Un-migrate: move backing back into mount.
if [ "$UNMIGRATE" = 1 ]; then
  # Make sure the mount is gone.
  if mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
    warn "$MOUNT_DIR is still a mountpoint; trying lazy unmount"
    fusermount -uz "$MOUNT_DIR" || fusermount3 -uz "$MOUNT_DIR" || true
  fi
  if [ -d "$BACKING_DIR" ]; then
    mkdir -p "$MOUNT_DIR"
    if [ -n "$(ls -A "$MOUNT_DIR" 2>/dev/null || true)" ]; then
      warn "$MOUNT_DIR is non-empty after unmount; aborting un-migrate"
      warn "Resolve by hand: contents are at $BACKING_DIR and $MOUNT_DIR"
    else
      say "Moving $BACKING_DIR back into $MOUNT_DIR"
      shopt -s dotglob nullglob
      mv "$BACKING_DIR"/* "$MOUNT_DIR"/ 2>/dev/null || true
      shopt -u dotglob nullglob
      rmdir "$BACKING_DIR" 2>/dev/null || true
      rmdir "$DATA_DIR" 2>/dev/null || true
    fi
  else
    warn "Backing dir $BACKING_DIR is missing — nothing to un-migrate"
  fi
else
  say "Skipping un-migrate (--no-unmigrate)"
fi

# 4. Remove binary.
rm -f "$BIN_DIR/outpost"

# 5. Config.
if [ "$PURGE" = 1 ]; then
  rm -rf "$CONFIG_DIR"
  say "Removed config dir"
else
  say "Config left in place at $CONFIG_DIR (pass --purge to remove)"
fi

say "Done"

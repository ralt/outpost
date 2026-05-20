#!/usr/bin/env bash
# dev-remote.sh — stand up a local Docker container that pretends to be the
# outpost remote, so you can exercise send-away / bring-back without a real
# dev box.
#
# What it does:
#   - creates a Docker bridge network with a static subnet
#   - builds a Debian image with sshd, git, tar, and a stub `claude` binary
#   - creates a user inside the container matching your local user + $HOME
#     (so the daemon's $HOME match check passes)
#   - drops your public key into the container's authorized_keys
#   - rewrites the [remote] section of ~/.config/outpost/config.ini
#
# Run:
#   ./scripts/dev-remote.sh up      # spin everything up
#   ./scripts/dev-remote.sh down    # tear it back down
#   ./scripts/dev-remote.sh ssh     # ssh into the container
#   ./scripts/dev-remote.sh status  # show container state

set -euo pipefail

NETWORK="outpost-dev"
SUBNET="172.28.0.0/16"
CONTAINER_IP="172.28.0.10"
CONTAINER_NAME="outpost-dev-remote"
IMAGE="outpost-dev-remote:latest"
CONFIG_FILE="${OUTPOST_CONFIG:-$HOME/.config/outpost/config.ini}"

LOCAL_USER="$(id -un)"
LOCAL_UID="$(id -u)"
LOCAL_GID="$(id -g)"
LOCAL_HOME="$HOME"

say()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!! \033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m!! \033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<EOF
Usage: $(basename "$0") <command>

Commands:
  up                Build image, start container, update outpost config
  down              Stop + remove the container and network
  ssh [args...]     ssh into the container as your local user
  status            Print container/network state
EOF
}

pub_key_file() {
  for f in "$HOME/.ssh/id_rsa.pub" "$HOME/.ssh/id_ed25519.pub"; do
    [ -f "$f" ] && { echo "$f"; return 0; }
  done
  return 1
}

priv_key_for() {
  case "$1" in
    *id_rsa.pub)     echo "$HOME/.ssh/id_rsa" ;;
    *id_ed25519.pub) echo "$HOME/.ssh/id_ed25519" ;;
  esac
}

require_docker() {
  command -v docker >/dev/null 2>&1 || die "docker not found in PATH"
  docker info >/dev/null 2>&1 || die "docker daemon not reachable (need sudo? add user to docker group?)"
}

# ── up ────────────────────────────────────────────────────────────

cmd_up() {
  require_docker
  PUB="$(pub_key_file)" || die "no public key found at ~/.ssh/id_rsa.pub or ~/.ssh/id_ed25519.pub"
  PRIV="$(priv_key_for "$PUB")"
  [ -f "$PRIV" ] || die "no matching private key at $PRIV"

  say "Using key pair: $PRIV / $PUB"

  if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
    say "Creating Docker network $NETWORK ($SUBNET)"
    docker network create --subnet="$SUBNET" "$NETWORK" >/dev/null
  fi

  say "Building image $IMAGE"
  docker build \
    --quiet \
    -t "$IMAGE" \
    --build-arg "USER_NAME=$LOCAL_USER" \
    --build-arg "USER_UID=$LOCAL_UID" \
    --build-arg "USER_GID=$LOCAL_GID" \
    --build-arg "USER_HOME=$LOCAL_HOME" \
    --build-arg "PUB_KEY=$(cat "$PUB")" \
    - <<'DOCKERFILE' >/dev/null
FROM debian:bookworm-slim

ARG USER_NAME
ARG USER_UID
ARG USER_GID
ARG USER_HOME
ARG PUB_KEY

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        openssh-server git tar ca-certificates procps && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /var/run/sshd && \
    sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config && \
    sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config && \
    sed -i 's/^#\?PubkeyAuthentication.*/PubkeyAuthentication yes/' /etc/ssh/sshd_config

# Create the user with the exact UID/GID/$HOME the daemon expects locally.
# `useradd -d $HOME -m` builds $HOME even if it lives outside /home.
RUN (getent group "$USER_GID" >/dev/null || groupadd -g "$USER_GID" "$USER_NAME") && \
    useradd -m -u "$USER_UID" -g "$USER_GID" -d "$USER_HOME" -s /bin/bash "$USER_NAME" && \
    install -d -o "$USER_UID" -g "$USER_GID" -m 700 "$USER_HOME/.ssh" && \
    printf '%s\n' "$PUB_KEY" > "$USER_HOME/.ssh/authorized_keys" && \
    chown "$USER_UID:$USER_GID" "$USER_HOME/.ssh/authorized_keys" && \
    chmod 600 "$USER_HOME/.ssh/authorized_keys"

# Stub `claude` so send-away has *something* to launch. Replace
# /usr/local/bin/claude inside the container if you want real Claude Code.
RUN printf '%s\n' \
    '#!/bin/sh' \
    'case "$1" in --version|-v) echo "outpost-dev-stub-claude 0.0.0"; exit 0;; esac' \
    'echo "[stub claude] $(date -Iseconds) args: $*"' \
    'echo "[stub claude] pretending to work; send SIGTERM to stop."' \
    'trap "echo \"[stub claude] exiting\"; exit 0" TERM INT' \
    'while true; do sleep 60; done' \
    > /usr/local/bin/claude && \
    chmod +x /usr/local/bin/claude

EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
DOCKERFILE

  if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    say "Removing previous container $CONTAINER_NAME"
    docker rm -f "$CONTAINER_NAME" >/dev/null
  fi

  say "Starting container $CONTAINER_NAME @ $CONTAINER_IP"
  docker run -d \
    --name "$CONTAINER_NAME" \
    --network "$NETWORK" \
    --ip "$CONTAINER_IP" \
    --restart unless-stopped \
    "$IMAGE" >/dev/null

  printf '==> Waiting for sshd '
  for i in $(seq 1 30); do
    if ssh -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=1 \
          -i "$PRIV" "$LOCAL_USER@$CONTAINER_IP" true 2>/dev/null; then
      printf 'ok\n'
      break
    fi
    printf '.'
    sleep 1
    if [ "$i" = 30 ]; then
      printf '\n'
      die "sshd did not come up within 30s (check 'docker logs $CONTAINER_NAME')"
    fi
  done

  # Refresh known_hosts so the daemon's TOFU doesn't have to do it on first use.
  ssh-keygen -R "$CONTAINER_IP" >/dev/null 2>&1 || true
  ssh-keyscan -t ed25519,rsa -H "$CONTAINER_IP" 2>/dev/null >> "$HOME/.ssh/known_hosts"

  remote_home="$(ssh -o BatchMode=yes -i "$PRIV" "$LOCAL_USER@$CONTAINER_IP" 'printf %s "$HOME"')"
  if [ "$remote_home" != "$LOCAL_HOME" ]; then
    warn "Remote \$HOME=$remote_home, local=$LOCAL_HOME — outpost will refuse to operate."
  else
    say "Remote \$HOME matches: $remote_home"
  fi

  update_config "$LOCAL_USER" "$CONTAINER_IP" "$PRIV"
  say "Updated [remote] section in $CONFIG_FILE"

  cat <<EOF

Container is up.

  host          : $LOCAL_USER@$CONTAINER_IP
  network       : $NETWORK ($SUBNET)
  ssh quickcheck: ssh -i $PRIV $LOCAL_USER@$CONTAINER_IP

If outpost is running, restart it so it picks up the new [remote] config:

    systemctl --user restart outpost.service
    outpost status

Note: the container ships a *stub* \`claude\` that just blocks on sleep so
send-away has a real PID to capture. Replace /usr/local/bin/claude inside
the container if you want real Claude Code on the "remote".

EOF
}

# update_config rewrites the [remote] section of CONFIG_FILE. Other sections
# are preserved as-is. If the file doesn't exist, it's created with just
# [remote] populated and a comment pointing at the rest of the schema.
update_config() {
  local user="$1" ip="$2" priv="$3"
  mkdir -p "$(dirname "$CONFIG_FILE")"
  if [ ! -f "$CONFIG_FILE" ]; then
    cat > "$CONFIG_FILE" <<EOF
# outpost config — see specs/03-config.md for the full schema.

[remote]
host = $user@$ip
port = 22
identity_file = $priv
EOF
    return
  fi
  awk -v sec="remote" '
    BEGIN { in_target = 0 }
    /^\[/ {
      trimmed = $0
      sub(/[[:space:]]*$/, "", trimmed)
      in_target = (trimmed == "[" sec "]")
      if (in_target) next
    }
    !in_target { print }
  ' "$CONFIG_FILE" > "$CONFIG_FILE.tmp"
  # Strip trailing blank lines, then append a fresh [remote].
  sed -i -e ':a' -e '/^$/{$d;N;ba' -e '}' "$CONFIG_FILE.tmp"
  cat >> "$CONFIG_FILE.tmp" <<EOF

[remote]
host = $user@$ip
port = 22
identity_file = $priv
EOF
  mv "$CONFIG_FILE.tmp" "$CONFIG_FILE"
}

# ── down ──────────────────────────────────────────────────────────

cmd_down() {
  require_docker
  if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    docker rm -f "$CONTAINER_NAME" >/dev/null
    say "Container removed"
  fi
  if docker network inspect "$NETWORK" >/dev/null 2>&1; then
    docker network rm "$NETWORK" >/dev/null
    say "Network removed"
  fi
  ssh-keygen -R "$CONTAINER_IP" >/dev/null 2>&1 || true
  say "Done"
}

# ── ssh ───────────────────────────────────────────────────────────

cmd_ssh() {
  PUB="$(pub_key_file)" || die "no public key found"
  PRIV="$(priv_key_for "$PUB")"
  exec ssh -i "$PRIV" "$LOCAL_USER@$CONTAINER_IP" "$@"
}

# ── status ────────────────────────────────────────────────────────

cmd_status() {
  if docker ps --filter "name=^${CONTAINER_NAME}$" --format '{{.Names}}' 2>/dev/null | grep -q .; then
    docker ps --filter "name=^${CONTAINER_NAME}$" --format 'container : {{.Names}}\nstatus    : {{.Status}}\nimage     : {{.Image}}'
    echo "ip        : $CONTAINER_IP"
  else
    echo "container : not running"
  fi
  if docker network inspect "$NETWORK" >/dev/null 2>&1; then
    echo "network   : $NETWORK ($SUBNET)"
  else
    echo "network   : absent"
  fi
}

case "${1:-}" in
  up)     shift; cmd_up "$@" ;;
  down)   shift; cmd_down "$@" ;;
  ssh)    shift; cmd_ssh "$@" ;;
  status) shift; cmd_status "$@" ;;
  ""|-h|--help|help) usage; exit 0 ;;
  *) usage; exit 2 ;;
esac

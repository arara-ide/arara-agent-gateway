#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DOCKERFILE="$ROOT/runtime/Dockerfile"
ENV_FILE="${ENV_FILE:-$ROOT/.env}"
AGY_MANIFESTS="https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests"
VERSION_PATTERN='^[0-9]+\.[0-9]+\.[0-9]+([.+-][0-9A-Za-z.-]+)?$'

log() { printf '== %s\n' "$*" >&2; }

env_value() {
  [ -f "$ENV_FILE" ] || return 0
  sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1
}

valid_version() {
  [[ "$1" =~ $VERSION_PATTERN ]] && printf '%s' "$1"
  return 0
}

npm_latest() {
  valid_version "$(docker run --rm "$NODE_IMAGE" npm view "$1" version 2>/dev/null | tail -n 1 || true)"
}

agy_latest() {
  local platform
  case "$(docker info --format '{{.Architecture}}' 2>/dev/null || true)" in
    x86_64 | amd64) platform=linux_amd64 ;;
    aarch64 | arm64) platform=linux_arm64 ;;
    *) return 0 ;;
  esac
  valid_version "$(curl -fsSL --max-time 20 "$AGY_MANIFESTS/$platform.json" 2>/dev/null |
    sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1 || true)"
}

build() {
  docker build --pull -f "$DOCKERFILE" -t "$IMAGE" "$@" "$ROOT"
}

IMAGE="${RUNTIME_IMAGE:-$(env_value RUNTIME_IMAGE)}"
IMAGE="${IMAGE:-arara-agent-runtime:local}"
NODE_IMAGE="$(sed -n 's/^ARG NODE_IMAGE=//p' "$DOCKERFILE")"
PREVIOUS="$(docker image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || true)"

docker pull -q "$NODE_IMAGE" >/dev/null 2>&1 || log "could not refresh $NODE_IMAGE, using the local copy"

CLAUDE_CODE_VERSION="$(npm_latest @anthropic-ai/claude-code)"
CODEX_VERSION="$(npm_latest @openai/codex)"
AGY_VERSION="$(agy_latest)"
log "latest: claude-code=${CLAUDE_CODE_VERSION:-unknown} codex=${CODEX_VERSION:-unknown} agy=${AGY_VERSION:-unknown}"

args=()
[ -n "$CLAUDE_CODE_VERSION" ] && args+=(--build-arg "CLAUDE_CODE_VERSION=$CLAUDE_CODE_VERSION")
[ -n "$CODEX_VERSION" ] && args+=(--build-arg "CODEX_VERSION=$CODEX_VERSION")
[ -n "$AGY_VERSION" ] && args+=(--build-arg "AGY_VERSION=$AGY_VERSION")

log "building $IMAGE with the latest CLIs"
if ! build ${args[@]+"${args[@]}"}; then
  if [ -n "$PREVIOUS" ]; then
    log "build with the latest CLIs failed, keeping the current $IMAGE"
    exit 0
  fi
  log "build with the latest CLIs failed, building with the Dockerfile defaults"
  build
fi

CURRENT="$(docker image inspect --format '{{.Id}}' "$IMAGE")"
docker run --rm --entrypoint sh "$IMAGE" -c 'claude --version && codex --version && agy --version' >&2
if [ -n "$PREVIOUS" ] && [ "$PREVIOUS" != "$CURRENT" ]; then
  docker image rm "$PREVIOUS" >/dev/null 2>&1 && log "removed previous runtime image" || log "previous runtime image still in use, kept"
fi
log "runtime image ready: $IMAGE"

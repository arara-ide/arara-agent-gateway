#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
IMAGE="${RUNTIME_IMAGE:-arara-agent-runtime:local}"
WORK="$(mktemp -d)"
PORT="${E2E_PORT:-$((20000 + RANDOM % 20000))}"
NETWORK="aag-e2e-$RANDOM"
ADMIN="$(openssl rand -hex 24)"
ENC="$(openssl rand -hex 32)"
BASE="http://127.0.0.1:$PORT/api"
GW_PID=""
PASS=0

log() { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
ok() { PASS=$((PASS + 1)); printf '  ok  %s\n' "$*"; }

start_gateway() {
  env -i PATH="$PATH" HOME="$WORK" \
    API_KEY="$ADMIN" CREDENTIALS_ENCRYPTION_KEY="$ENC" \
    PORT="$PORT" DATA_DIR="$WORK/data" LOG_LEVEL=info \
    IDLE_TIMEOUT_SECONDS=30 IDLE_CHECK_INTERVAL_SECONDS=2 \
    CONTAINER_MEMORY=1g CONTAINER_CPUS=1 CONTAINER_NETWORK="$NETWORK" RUNTIME_IMAGE="$IMAGE" \
    DOCKER_SOCKET="${DOCKER_SOCKET:-$(docker context inspect --format '{{.Endpoints.docker.Host}}' | sed 's#^unix://##')}" \
    "$WORK/gateway" >>"$WORK/gateway.log" 2>&1 &
  GW_PID=$!
  for _ in $(seq 1 50); do
    curl -fsS "$BASE/health" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  cat "$WORK/gateway.log" >&2
  fail "gateway did not start"
}

stop_gateway() {
  if [ -n "$GW_PID" ] && kill -0 "$GW_PID" 2>/dev/null; then
    kill "$GW_PID"
    wait "$GW_PID" 2>/dev/null || true
  fi
  GW_PID=""
}

cleanup() {
  local code=$?
  stop_gateway
  for c in $(docker ps -aq --filter "network=$NETWORK"); do docker rm -f "$c" >/dev/null 2>&1 || true; done
  for id in ${CREATED:-}; do
    docker rm -f "aag-$id" >/dev/null 2>&1 || true
    docker volume rm -f "aag-$id" >/dev/null 2>&1 || true
  done
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  if [ $code -ne 0 ] && [ -f "$WORK/gateway.log" ]; then
    echo "--- gateway log (tail)" >&2
    tail -n 40 "$WORK/gateway.log" >&2
  fi
  rm -rf "$WORK"
  if [ $code -eq 0 ]; then echo; echo "PASS ($PASS checks)"; else echo; echo "FAIL"; fi
}
trap cleanup EXIT

req() {
  local method="$1" path="$2" key="$3" body="${4:-}"
  local args=(-sS -o "$WORK/body" -w '%{http_code}' -X "$method" "$BASE$path")
  [ -n "$key" ] && args+=(-H "apikey: $key")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  STATUS="$(curl "${args[@]}")"
  BODY="$(cat "$WORK/body")"
}

expect() {
  [ "$STATUS" = "$1" ] || fail "$2: expected $1, got $STATUS: $BODY"
  ok "$2"
}

docker image inspect "$IMAGE" >/dev/null 2>&1 || fail "runtime image $IMAGE not found (scripts/build-runtime.sh)"
log "build gateway"
(cd "$ROOT" && go build -o "$WORK/gateway" ./cmd/gateway)
start_gateway
CREATED=""

log "authentication"
req GET /instances ""; expect 401 "missing apikey is rejected"
req GET /instances "wrong"; expect 401 "invalid apikey is rejected"
req GET /instances "$ADMIN"; expect 200 "apikey is accepted"

log "instance lifecycle"
req POST /instances "$ADMIN" '{"name":"e2e-a"}'; expect 201 "create first instance"
A="$(jq -r .instance.id <<<"$BODY")"; CREATED="$A"
[ "$(jq -r .instance.state <<<"$BODY")" = stopped ] || fail "new instance must be stopped"
docker container inspect "aag-$A" >/dev/null 2>&1 && fail "container created before start"
ok "creation does not start a container"
req POST /instances "$ADMIN" '{"name":"e2e-b"}'; expect 201 "create second instance freely"
B="$(jq -r .instance.id <<<"$BODY")"; CREATED="$A $B"
[ "$A" != "$B" ] || fail "second instance reused first id"
ok "multiple instances can be created"

req POST "/instances/$A/start" "$ADMIN"; expect 200 "start instance"
[ "$(jq -r .instance.state <<<"$BODY")" = running ] || fail "instance not running"
INSPECT="$(docker container inspect "aag-$A")"
[ "$(jq -r '.[0].Config.User' <<<"$INSPECT")" = "1000:1000" ] || fail "container user"
[ "$(jq -r '.[0].HostConfig.Privileged' <<<"$INSPECT")" = false ] || fail "privileged"
[ "$(jq -r '.[0].HostConfig.ReadonlyRootfs' <<<"$INSPECT")" = true ] || fail "readonly rootfs"
[ "$(jq -r '.[0].HostConfig.CapDrop | join(",")' <<<"$INSPECT")" = ALL ] || fail "cap drop"
[ "$(jq -r '.[0].HostConfig.Memory' <<<"$INSPECT")" = $((1 << 30)) ] || fail "memory limit"
jq -e '.[0].HostConfig.SecurityOpt | index("no-new-privileges:true")' <<<"$INSPECT" >/dev/null || fail "no-new-privileges"
jq -e '[.[0].Mounts[].Source] | map(select(test("docker.sock"))) | length == 0' <<<"$INSPECT" >/dev/null || fail "docker socket mounted"
ok "container isolation flags"
docker exec "aag-$A" sh -c 'test ! -e /var/run/docker.sock && ! touch /usr/x 2>/dev/null && touch ~/marker && whoami' | grep -qx agent || fail "in-container checks"
ok "non-root user, read-only rootfs, writable home volume"
docker exec "aag-$A" sh -c 'mkdir -p ~/.codex/sessions && touch -d "40 days ago" ~/.codex/sessions/old.jsonl && touch ~/.codex/sessions/new.jsonl'

req POST "/instances/$A/start" "$ADMIN"; expect 200 "start is idempotent"

log "repository changes"
CHANGES="/instances/$A/repos/changes?full_name=fixture%2Fapp&agent=codex"
req GET "$CHANGES" "$ADMIN"; expect 200 "no worktree is reported as clean"
jq -e '.has_changes == false and .can_push == false' <<<"$BODY" >/dev/null || fail "missing worktree: $BODY"
docker exec -i "aag-$A" sh -eu <<'SH'
repo="$HOME/work/repos/fixture/app"
worktree="$HOME/work/worktrees/fixture/app/codex"
git init -q -b main "$repo"
git init -q --bare "$HOME/fixture-origin"
git -C "$repo" remote add origin "$HOME/fixture-origin"
git -C "$repo" config user.name Fixture
git -C "$repo" config user.email fixture@example.invalid
printf 'base\n' >"$repo/app.txt"
printf 'app.txt filter=fixture\n' >"$repo/.gitattributes"
git -C "$repo" add .
git -C "$repo" commit -qm base
git -C "$repo" worktree add -qb arara/codex "$worktree"
printf 'staged\n' >"$worktree/app.txt"
git -C "$worktree" add app.txt
printf 'working\n' >>"$worktree/app.txt"
printf 'untracked content\n' >"$worktree/new.txt"
git -C "$repo" config filter.fixture.clean 'touch /tmp/aag-e2e-filter-ran; cat'
SH
req GET "$CHANGES" "$ADMIN"; expect 200 "status enables diff without generating a patch"
jq -e '.has_changes == true and .can_push == false and (has("diff") | not)' <<<"$BODY" >/dev/null || fail "working changes status: $BODY"
req GET "$CHANGES&include_diff=true" "$ADMIN"; expect 200 "details show staged and unstaged lines with untracked paths"
jq -e '(.diff | contains("+staged") and contains("+working")) and .untracked == ["new.txt"]' <<<"$BODY" >/dev/null || fail "working changes details: $BODY"
docker exec "aag-$A" test ! -e /tmp/aag-e2e-filter-ran || fail "Git clean filter ran during a read"
req GET "/instances/$A/repos/changes?full_name=fixture%2Fapp&agent=claude" "$ADMIN"; expect 200 "another agent cannot see the Codex worktree"
jq -e '.has_changes == false and .can_push == false' <<<"$BODY" >/dev/null || fail "other worktree: $BODY"
docker exec -i "aag-$A" sh -eu <<'SH'
cd "$HOME/work/worktrees/fixture/app/codex"
git config --unset filter.fixture.clean
git add .
git commit -qm changes
SH
req GET "$CHANGES&include_diff=true" "$ADMIN"; expect 200 "unpublished commits enable send and remain visible in diff"
jq -e '.has_changes == true and .can_push == true and (.diff | contains("+working"))' <<<"$BODY" >/dev/null || fail "unpublished commit: $BODY"
docker exec "aag-$A" sh -eu -c 'cd "$HOME/work/worktrees/fixture/app/codex"; git push -q origin HEAD:refs/heads/arara/codex'
req GET "$CHANGES" "$ADMIN"; expect 200 "published worktree has both actions disabled"
jq -e '.has_changes == false and .can_push == false' <<<"$BODY" >/dev/null || fail "published worktree: $BODY"

log "agents"
req GET "/instances/$A/agents" "$ADMIN"; expect 200 "agent status"
[ "$(jq -r '[.agents[].status] | unique | join(",")' <<<"$BODY")" = setup_required ] || fail "fresh agents should need setup: $BODY"
ok "fresh instance reports setup_required for all three agents"
for agent in claude codex antigravity; do
  req GET "/instances/$A/agents/$agent/usage" "$ADMIN"; expect 200 "$agent usage probe"
  jq -e 'has("usage")' <<<"$BODY" >/dev/null || fail "$agent usage payload: $BODY"
done
ok "usage probes answer without a prompt on a fresh instance"
req POST "/instances/$A/agents/claude/run" "$ADMIN" '{"prompt":"hi"}'; expect 409 "claude run without login is rejected"
[ "$(jq -r .error.code <<<"$BODY")" = agent_not_authenticated ] || fail "error code"
if [ "${E2E_SKIP_PROVIDERS:-0}" != 1 ]; then
req POST "/instances/$A/agents/claude/login" "$ADMIN"; expect 202 "claude login starts"
jq -e '.login.url | test("^https://")' <<<"$BODY" >/dev/null || fail "claude login url missing: $BODY"
jq -e '.login.needs_input == true' <<<"$BODY" >/dev/null || fail "claude login should ask for a code"
ok "claude login exposes the official URL"
req DELETE "/instances/$A/agents/claude/login" "$ADMIN"; expect 200 "claude login cancel"

req POST "/instances/$A/agents/antigravity/login" "$ADMIN"; expect 202 "antigravity login starts"
jq -e '.login.url | test("^https://accounts\\.google\\.com/")' <<<"$BODY" >/dev/null || fail "antigravity login url missing: $BODY"
jq -e '.login.needs_input == true' <<<"$BODY" >/dev/null || fail "antigravity login should ask for a code"
ok "antigravity login exposes the Google OAuth URL"
req DELETE "/instances/$A/agents/antigravity/login" "$ADMIN"; expect 200 "antigravity login cancel"

req POST "/instances/$A/agents/codex/login" "$ADMIN"; expect 202 "codex device login starts"
if jq -e '.login.url != null and .login.code != null' <<<"$BODY" >/dev/null; then ok "codex device code captured"; else echo "  note codex login: $(jq -c .login <<<"$BODY")"; fi
req DELETE "/instances/$A/agents/codex/login" "$ADMIN"; expect 200 "codex login cancel"
sleep 3
docker exec "aag-$A" sh -c 'ps -eo comm' | grep -Eq '^(claude|codex|agy)$' && fail "login processes left running"
ok "cancelled logins leave no processes"

req PUT "/instances/$A/agents/antigravity/api-key" "$ADMIN" '{"api_key":"AIzaInvalidE2EKey000000000"}'; expect 200 "store antigravity api key"
grep -q AIzaInvalid "$WORK/data/gateway.db" && fail "api key stored in plaintext"
ok "api key encrypted at rest"
req GET "/instances/$B/connections" "$ADMIN"; expect 200 "second instance connections isolated"
[ "$(jq -r '.connections[] | select(.provider=="antigravity") | .status' <<<"$BODY")" = setup_required ] || fail "connection leaked between instances: $BODY"
ok "connections are isolated per instance"

req POST "/instances/$A/agents/antigravity/run" "$ADMIN" '{"prompt":"Reply PONG"}'; expect 202 "antigravity run accepted"
RUN="$(jq -r .run.id <<<"$BODY")"
STREAM="$(curl -sS -N --max-time 120 -H "apikey: $ADMIN" "$BASE/instances/$A/runs/$RUN/stream")"
grep -q '^event: done' <<<"$STREAM" || fail "stream did not finish: $STREAM"
DONE="$(grep -A1 '^event: done' <<<"$STREAM" | sed -n 's/^data: //p')"
[ "$(jq -r .status <<<"$DONE")" = failed ] || fail "invalid key run should fail: $DONE"
ok "antigravity runs in the container and streams a final event ($(jq -r .error_code <<<"$DONE"))"
docker exec "aag-$A" env | grep -q GEMINI_API_KEY && fail "api key persisted in container env"
ok "api key is not part of the container environment"

log "parallel runs and cancellation"
req PUT "/instances/$A/agents/claude/api-key" "$ADMIN" '{"api_key":"sk-ant-invalid-e2e-key-000000"}'; expect 200 "store claude api key"
req PUT "/instances/$A/agents/codex/api-key" "$ADMIN" '{"api_key":"sk-invalid-e2e-key-0000000000"}'; expect 200 "store codex api key"
for agent in claude codex antigravity; do
  req POST "/instances/$A/agents/$agent/run" "$ADMIN" '{"prompt":"Reply PONG"}'; expect 202 "$agent run accepted in parallel"
  eval "RUN_$agent=\"$(jq -r .run.id <<<"$BODY")\""
done
req POST "/instances/$A/agents/claude/run" "$ADMIN" '{"prompt":"again"}'
if [ "$STATUS" = 409 ]; then ok "same agent cannot run twice at once"; else echo "  note claude finished before the duplicate check ($STATUS)"; fi
req POST "/instances/$A/runs/$RUN_codex/cancel" "$ADMIN"
expect 202 "codex run cancel requested"
for agent in claude codex antigravity; do
  eval "rid=\$RUN_$agent"
  for _ in $(seq 1 90); do
    req GET "/instances/$A/runs/$rid" "$ADMIN"
    [ "$(jq -r .run.status <<<"$BODY")" != running ] && break
    sleep 1
  done
  st="$(jq -r .run.status <<<"$BODY")"
  code="$(jq -r .run.error_code <<<"$BODY")"
  case "$agent" in
    codex) want="cancelled/cancelled_by_request" ;;
    *) want="failed/agent_not_authenticated" ;;
  esac
  [ "$st/$code" = "$want" ] || fail "$agent run finished as $st/$code, expected $want"
  ok "$agent run finished as $want"
done
sleep 3
docker exec "aag-$A" sh -c 'ps -eo comm' | grep -Eq '^(claude|codex|agy)$' && fail "agent processes left running"
ok "no agent processes left after parallel runs"

else
  echo "  skip provider network checks (E2E_SKIP_PROVIDERS=1)"
fi

log "idle shutdown"
for _ in $(seq 1 30); do
  req GET "/instances/$A" "$ADMIN"
  [ "$(jq -r .instance.state <<<"$BODY")" = stopped ] && break
  sleep 2
done
[ "$(jq -r .instance.state <<<"$BODY")" = stopped ] || fail "instance not stopped by idle manager"
docker container inspect "aag-$A" >/dev/null 2>&1 && fail "container still exists after idle stop"
docker volume inspect "aag-$A" >/dev/null || fail "volume removed by idle stop"
ok "idle instance stopped and its volume kept"

log "persistence across gateway restart"
stop_gateway
start_gateway
req GET "/instances/$A/history" "$ADMIN"; expect 200 "history after restart"
want_runs=4
[ "${E2E_SKIP_PROVIDERS:-0}" = 1 ] && want_runs=0
[ "$(jq -r '.runs | length' <<<"$BODY")" = "$want_runs" ] || fail "history lost"
req GET "/instances/$A/connections" "$ADMIN"; expect 200 "connections after restart"
if [ "${E2E_SKIP_PROVIDERS:-0}" != 1 ]; then
  [ "$(jq -r '.connections[] | select(.provider=="antigravity") | .method' <<<"$BODY")" = api_key ] || fail "connection lost"
fi
req POST "/instances/$A/start" "$ADMIN"; expect 200 "restart instance"
docker exec "aag-$A" test -f /home/agent/marker || fail "volume content lost"
ok "volume content survives stop and gateway restart"
docker exec "aag-$A" sh -c 'test ! -e ~/.codex/sessions/old.jsonl && test -e ~/.codex/sessions/new.jsonl' || fail "session retention"
ok "session files older than the retention are pruned on start"

log "delete"
req DELETE "/instances/$A" "$ADMIN"; expect 400 "delete requires confirmation"
req DELETE "/instances/$A?confirm=$A" "$ADMIN"; expect 204 "delete instance"
docker container inspect "aag-$A" >/dev/null 2>&1 && fail "container left after delete"
docker volume inspect "aag-$A" >/dev/null 2>&1 && fail "volume left after delete"
ok "delete removes container and volume"
req DELETE "/instances/$B?confirm=$B" "$ADMIN"; expect 204 "delete second instance"

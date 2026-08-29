#!/usr/bin/env bash
# test/agentdeath_integration.sh — proves the relay survives its ACP agent
# process dying, and says so honestly while it is down.
#
# Tests:
#   A. AGENT KILLED: kill the agent child; the worker logs the unexpected exit
#      and exits, the supervisor respawns worker+agent, the next prompt works.
#   B. HONEST TURN: with agent.restart.enabled=false the worker stays up with a
#      dead agent; a prompt then gets a VISIBLE text frame saying the agent is
#      not running (the 2026-08-28 incident produced a silent empty bubble).
#   C. BACKOFF: agent-cmd is an ssh to an unroutable host, so no worker can
#      ever come up. The supervisor must stay alive and retry on a growing,
#      capped delay — never Fatalf, never hot-loop.
#
# Self-contained: builds the worktree binary + fake agent, runs on :8099,
# drives everything with curl. Transcript: /tmp/poeacp-agentdeath-evidence.txt
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1
EV=/tmp/poeacp-agentdeath-evidence.txt
ADDR=127.0.0.1:8099
KEY=deathtest
export POEACP_ACCESS_KEY="$KEY"

BIN=/tmp/poeacp-agentdeath-bin
AGENT=/tmp/poeacp-agentdeath-agent
STATE=$(mktemp -d)
LOG=/tmp/poeacp-agentdeath-run.log

: > "$EV"
log() { echo "$@" | tee -a "$EV"; }
fail() { log "FAIL: $*"; exit 1; }
PASS_A=no; PASS_B=no; PASS_C=no

cleanup() {
  [ -n "${SRV_PID:-}" ] && kill "$SRV_PID" 2>/dev/null
  pkill -f "$BIN" 2>/dev/null
  pkill -f "$AGENT" 2>/dev/null
  rm -rf "$STATE"
}
trap cleanup EXIT

post() { # conv msg_id content -> raw SSE
  curl -sN --max-time 60 -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    --data-raw "{\"type\":\"query\",\"conversation_id\":\"$1\",\"user_id\":\"u\",\"message_id\":\"$2\",\"query\":[{\"role\":\"user\",\"content\":\"$3\"}]}" \
    "http://$ADDR/poe"
}
health() { curl -s --max-time 5 "http://$ADDR/healthz"; }
pidof_health() { health | sed -n 's/.*pid=\([0-9]*\).*/\1/p'; }
count() { local n; n=$(grep -c "$1" "$2" 2>/dev/null); echo "${n:-0}"; }
# waitlog PATTERN SECONDS -> 0 when PATTERN appears in $LOG within the budget
waitlog() {
  local pat="$1" budget="$2" i
  for i in $(seq 1 $((budget * 5))); do
    grep -q "$pat" "$LOG" && return 0
    sleep 0.2
  done
  return 1
}

log "=== agent-death integration $(date -u +%FT%TZ) ==="
log "--- build ---"
go build -o "$BIN" ./cmd/poe-acp || fail "build poe-acp"
go build -o "$AGENT" ./test/fakeagent || fail "build fakeagent"

start_relay() { # config-path
  FAKE_DELAY=10ms FAKE_CHUNKS=2 "$BIN" -http-addr "$ADDR" -agent-cmd "$AGENT" \
    -config "$1" -state-dir "$STATE" -heartbeat-interval 1s -turn-timeout 60s \
    >"$LOG" 2>&1 &
  SRV_PID=$!
  local i
  for i in $(seq 1 100); do health >/dev/null 2>&1 && return 0; sleep 0.2; done
  return 1
}

stop_relay() {
  [ -n "${SRV_PID:-}" ] && kill "$SRV_PID" 2>/dev/null
  wait "$SRV_PID" 2>/dev/null
  pkill -f "$AGENT" 2>/dev/null
  SRV_PID=""
  sleep 0.5
}

###############################################################################
log ""
log "=== TEST A: agent killed -> worker exits -> supervisor respawns ==="
echo '{}' > /tmp/poeacp-agentdeath-cfg.json
start_relay /tmp/poeacp-agentdeath-cfg.json || fail "server never became healthy (see $LOG)"
W1=$(pidof_health)
log "[A] worker pid=$W1 serving (supervisor pid=$SRV_PID)"

R=$(post convA mA1 "hello")
[ "$(count '^event: text' <(echo "$R"))" -ge 1 ] || fail "[A] first prompt produced no text"
log "[A] first prompt OK ($(count '^event: text' <(echo "$R")) text events)"

AGENT_PID=$(pgrep -f "^$AGENT" | head -1)
log "[A] killing agent pid=$AGENT_PID"
kill -9 "$AGENT_PID"

waitlog "agent exited unexpectedly" 10 || fail "[A] worker never noticed the agent death (see $LOG)"
waitlog "died unexpectedly; respawning" 10 || fail "[A] supervisor never saw the worker exit"
W2=""
for _ in $(seq 1 100); do
  W2=$(pidof_health); [ -n "$W2" ] && [ "$W2" != "$W1" ] && break
  sleep 0.2
done
[ -n "$W2" ] && [ "$W2" != "$W1" ] || fail "[A] no fresh worker after respawn (old=$W1 new=$W2)"
log "[A] respawned: worker pid=$W1 -> pid=$W2"

R=$(post convA2 mA2 "hello again")
if [ "$(count '^event: text' <(echo "$R"))" -ge 1 ] && [ "$(count '^event: done' <(echo "$R"))" -ge 1 ]; then
  PASS_A=yes
  log "[A] PASS: prompt after respawn works"
else
  log "[A] post-respawn SSE: $R"
fi
log "[A] --- relay log excerpt ---"
grep -E "agent started|agent exited unexpectedly|died unexpectedly; respawning|worker pid=.* serving" "$LOG" | tee -a "$EV"
stop_relay

###############################################################################
log ""
log "=== TEST B: agent dead, restart disabled -> honest user-facing turn ==="
echo '{"agent":{"restart":{"enabled":false}}}' > /tmp/poeacp-agentdeath-cfg-norestart.json
start_relay /tmp/poeacp-agentdeath-cfg-norestart.json || fail "[B] server never became healthy"
WB=$(pidof_health)
post convB mB0 "warm up" >/dev/null
AGENT_PID=$(pgrep -f "^$AGENT" | head -1)
log "[B] worker pid=$WB; killing agent pid=$AGENT_PID (restart disabled)"
kill -9 "$AGENT_PID"
waitlog "agent exited unexpectedly" 10 || fail "[B] worker never noticed the agent death"
[ "$(pidof_health)" = "$WB" ] || fail "[B] worker recycled despite restart.enabled=false"

R=$(post convB mB1 "are you there?")
if echo "$R" | grep -q 'backing agent is not running'; then
  PASS_B=yes
  log "[B] PASS: user got a visible outage message while the worker still ran"
fi
log "[B] --- SSE seen by the user ---"
echo "$R" | grep -E '^event:|^data:' | head -12 | tee -a "$EV"
stop_relay

###############################################################################
log ""
log "=== TEST C: unroutable agent host -> capped backoff, supervisor survives ==="
# 192.0.2.1 is TEST-NET-1: guaranteed unroutable, so ssh never connects.
FAKE_DELAY=10ms "$BIN" -http-addr "$ADDR" \
  -agent-cmd "ssh -T -o BatchMode=yes -o ConnectTimeout=2 192.0.2.1 acp-agent" \
  -config /tmp/poeacp-agentdeath-cfg.json -state-dir "$STATE" >"$LOG" 2>&1 &
SRV_PID=$!
sleep 20
RETRIES=$(count "retrying in" "$LOG")
ALIVE=no; kill -0 "$SRV_PID" 2>/dev/null && ALIVE=yes
FATAL=$(count "respawn failed: .*exiting" "$LOG")
log "[C] supervisor alive=$ALIVE retry-lines=$RETRIES after 20s"
log "[C] --- backoff log ---"
grep -E "start agent|failed \(attempt" "$LOG" | tail -8 | tee -a "$EV"
if [ "$ALIVE" = yes ] && [ "$RETRIES" -ge 2 ] && [ "$FATAL" -eq 0 ]; then
  PASS_C=yes
  log "[C] PASS: supervisor kept retrying without dying"
fi
stop_relay

###############################################################################
log ""
log "=== SUMMARY: A=$PASS_A B=$PASS_B C=$PASS_C ==="
[ "$PASS_A" = yes ] && [ "$PASS_B" = yes ] && [ "$PASS_C" = yes ] || exit 1
log "ALL PASS"

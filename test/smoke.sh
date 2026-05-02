#!/usr/bin/env bash
# test/smoke.sh — black-box end-to-end check against a running poe-acp-relay.
#
# Assumes: relay binary running on $POEACP_ADDR (default localhost:8080) with
# $POEACP_ACCESS_KEY set to the same value in this shell. Typically:
#
#   export POEACP_ACCESS_KEY=testsecret
#   go run ./cmd/poe-acp-relay --agent-cmd "fir --mode acp" &
#   ./test/smoke.sh
#
# Exits non-zero on failure. Prints the SSE stream to stderr for eyeballing.

set -euo pipefail

ADDR="${POEACP_ADDR:-localhost:8080}"
KEY="${POEACP_ACCESS_KEY:?POEACP_ACCESS_KEY required}"

conv="smoke-$(date +%s)"
msg="${1:-Say the word \"pong\" and stop.}"

echo "--- /healthz ---" >&2
curl -fsS "http://${ADDR}/healthz" >&2
echo >&2

echo "--- /poe settings ---" >&2
curl -fsS -H "Authorization: Bearer ${KEY}" \
     -H 'Content-Type: application/json' \
     -d '{"type":"settings"}' \
     "http://${ADDR}/poe" >&2
echo >&2

echo "--- /poe query (conv=${conv}) ---" >&2
body=$(cat <<JSON
{
  "type": "query",
  "conversation_id": "${conv}",
  "user_id": "smoke-user",
  "message_id": "msg-1",
  "query": [{"role": "user", "content": "${msg}"}]
}
JSON
)

out=$(curl -fsSN -H "Authorization: Bearer ${KEY}" \
           -H 'Content-Type: application/json' \
           --data-raw "$body" \
           "http://${ADDR}/poe")
echo "$out" >&2

echo "$out" | grep -q '^event: meta'  || { echo "FAIL: no meta" >&2; exit 1; }
echo "$out" | grep -q '^event: text'  || { echo "FAIL: no text" >&2; exit 1; }
echo "$out" | grep -q '^event: done'  || { echo "FAIL: no done" >&2; exit 1; }
echo "--- /debug/sessions ---" >&2
curl -fsS -H "Authorization: Bearer ${KEY}" "http://${ADDR}/debug/sessions" >&2
echo >&2

echo "--- /poe query with attachment (conv=${conv}-att) ---" >&2
# Spin up a tiny local file server so the smoke test has zero network deps.
ATT_DIR="$(mktemp -d)"
trap 'rm -rf "$ATT_DIR"; [ -n "${ATT_PID:-}" ] && kill "$ATT_PID" 2>/dev/null || true' EXIT
echo "hello from poe-acp-relay smoke test" > "$ATT_DIR/note.txt"
ATT_PORT="${POEACP_ATT_PORT:-18181}"
( cd "$ATT_DIR" && python3 -m http.server "$ATT_PORT" --bind 127.0.0.1 ) >/dev/null 2>&1 &
ATT_PID=$!
# Wait briefly for the server.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  curl -fsS "http://127.0.0.1:${ATT_PORT}/note.txt" >/dev/null 2>&1 && break
  sleep 0.2
done

att_body=$(cat <<JSON
{
  "type": "query",
  "conversation_id": "${conv}-att",
  "user_id": "smoke-user",
  "message_id": "msg-att-1",
  "query": [{"role": "user", "content": "Acknowledge the attached file by name only.",
             "attachments": [{"url": "http://127.0.0.1:${ATT_PORT}/note.txt",
                              "content_type": "text/plain", "name": "note.txt"}]}]
}
JSON
)

att_out=$(curl -fsSN -H "Authorization: Bearer ${KEY}" \
              -H 'Content-Type: application/json' \
              --data-raw "$att_body" \
              "http://${ADDR}/poe")
echo "$att_out" >&2
echo "$att_out" | grep -q '^event: meta' || { echo "FAIL(att): no meta" >&2; exit 1; }
echo "$att_out" | grep -q '^event: done' || { echo "FAIL(att): no done" >&2; exit 1; }

echo "OK"

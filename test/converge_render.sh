#!/usr/bin/env bash
# shellcheck disable=SC2015  # `check && ok || bad` is the assertion idiom here; ok/bad never fail
# converge_render.sh — offline tests for scripts/converge.sh.
#
# 1. Golden-fixture tests: render config/execstart/unit/plist for every bot
#    spec and compare against test/golden/.
# 2. Boolean/absent-flag edge: enable_mcp_attach false vs absent vs true.
# 3. Fake-target converge: dry-run against a --target-root dir, apply,
#    then verify idempotence (second run reports no artefact diffs).
#
# Run from anywhere: ./test/converge_render.sh
# Regenerate goldens after an intentional renderer change:
#   REGEN=1 ./test/converge_render.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CONVERGE="$ROOT/scripts/converge.sh"
GOLDEN="$ROOT/test/golden"
BOTS=(two-fir kopi-fir sea-fir fir-air)

pass=0 fail=0
ok()  { pass=$((pass + 1)); echo "  ok   $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL $1"; }

check_golden() { # <name> <golden-file> <actual-content-file>
  if [ "${REGEN:-}" = 1 ]; then
    cp "$3" "$2"; echo "  regen $1"; return
  fi
  if diff -u "$2" "$3" >/dev/null 2>&1; then ok "$1"; else
    bad "$1"; diff -u "$2" "$3" || true
  fi
}

echo "== golden renders"
mkdir -p "$GOLDEN"
for bot in "${BOTS[@]}"; do
  for art in config execstart; do
    t=$(mktemp); "$CONVERGE" render "$bot" "$art" >"$t"
    check_golden "$bot.$art" "$GOLDEN/$bot.$art" "$t"; rm -f "$t"
  done
done
for bot in two-fir kopi-fir sea-fir; do
  t=$(mktemp); "$CONVERGE" render "$bot" unit >"$t"
  check_golden "$bot.unit" "$GOLDEN/$bot.unit" "$t"; rm -f "$t"
done
t=$(mktemp); "$CONVERGE" render fir-air plist >"$t"
check_golden "fir-air.plist" "$GOLDEN/fir-air.plist" "$t"; rm -f "$t"

echo "== boolean flag edge cases (false and absent must render identically)"
tmpd=$(mktemp -d)
trap 'rm -rf "$tmpd"' EXIT
mkdir -p "$tmpd/repo/bots" "$tmpd/repo/scripts"
cp "$CONVERGE" "$tmpd/repo/scripts/"
cp "$ROOT/dist.lock" "$tmpd/repo/"
mkspec() { # <name> <server-json-extra>
  cat >"$tmpd/repo/bots/$1.json" <<EOF
{
  "name": "$1", "host": "nowhere", "platform": "linux/amd64",
  "supervisor": "systemd-user", "unit": "poe-acp-$1",
  "binary": "~/.local/bin/poe-acp",
  "agent": {"cmd": "fir --mode acp", "kind": "fir"},
  "fir": {"exts": []},
  "server": {"http_addr": "127.0.0.1:9999", "poe_path": "/x"$2},
  "config": {"bot_name": "$1"},
  "credentials": {"env_file": "~/.config/poe-acp/bot-$1/env"}
}
EOF
}
mkspec eabsent ""
mkspec efalse  ', "enable_mcp_attach": false'
mkspec etrue   ', "enable_mcp_attach": true'
a=$("$tmpd/repo/scripts/converge.sh" render eabsent execstart)
f=$("$tmpd/repo/scripts/converge.sh" render efalse execstart)
tr_=$("$tmpd/repo/scripts/converge.sh" render etrue execstart)
[ "$a" = "$f" ] && ok "false == absent (no flag)" || bad "false must render like absent"
case "$tr_" in *--enable-mcp-attach*) ok "true emits flag" ;; *) bad "true must emit flag" ;; esac
case "$a" in *--enable-mcp-attach*) bad "absent must not emit flag" ;; *) ok "absent emits no flag" ;; esac
case "$a" in *--session-ttl*) bad "absent session_ttl must not emit flag" ;; *) ok "absent session_ttl emits no flag" ;; esac

echo "== misc guards"
if "$CONVERGE" --tot --apply >/dev/null 2>&1; then
  bad "--tot must refuse extra arguments"
else
  ok "--tot refuses extra arguments"
fi
# introduction guard: a double quote in the intro must die at render time
mkspec eguard ', "introduction": "bad \" quote"'
if "$tmpd/repo/scripts/converge.sh" render eguard execstart >/dev/null 2>&1; then
  bad "introduction with a double quote must be rejected"
else
  ok "introduction guard rejects double quote"
fi

echo "== fake-target converge (dry-run, apply, idempotence)"
fake="$tmpd/fakehost"
mkdir -p "$fake/.local/bin"
printf '#!/bin/sh\necho 0.51.0\n' >"$fake/.local/bin/poe-acp"
chmod +x "$fake/.local/bin/poe-acp"

out=$("$CONVERGE" two-fir --target-root "$fake")
case "$out" in
  *"DRY RUN"*) ok "dry run is the default" ;;
  *) bad "expected DRY RUN banner"; echo "$out" ;;
esac
case "$out" in
  *"poe-acp 0.51.0 ✓"*) ok "stub binary version matches lock" ;;
  *) bad "expected poe-acp version ✓"; echo "$out" ;;
esac
[ ! -f "$fake/.config/poe-acp/bot-two-fir/config.json" ] \
  && ok "dry run wrote nothing" || bad "dry run must not write config"

out=$("$CONVERGE" two-fir --target-root "$fake" --apply)
[ -f "$fake/.config/poe-acp/bot-two-fir/config.json" ] \
  && ok "apply wrote config" || bad "apply must write config"
[ -f "$fake/.config/systemd/user/poe-acp-two-fir.service" ] \
  && ok "apply wrote unit" || bad "apply must write unit"
diff <("$CONVERGE" render two-fir config) "$fake/.config/poe-acp/bot-two-fir/config.json" >/dev/null \
  && ok "written config matches render" || bad "config on fake host differs from render"

out=$("$CONVERGE" two-fir --target-root "$fake" --apply)
case "$out" in
  *"config"*"✓"*) ok "second apply: config already converged" ;;
  *) bad "second apply should show config ✓"; echo "$out" ;;
esac
case "$out" in
  *"unit"*"✓"*) ok "second apply: unit already converged" ;;
  *) bad "second apply should show unit ✓"; echo "$out" ;;
esac
nbaks=$(find "$fake" -name "*.bak-*" | wc -l)
# second apply rewrote nothing, so at most the artefacts' first-write count of backups (0: files didn't pre-exist)
[ "$nbaks" -eq 0 ] && ok "no spurious backups" || bad "expected 0 backups, got $nbaks"

# backup on overwrite: mutate config, re-apply, expect .bak
echo '{"x":1}' >"$fake/.config/poe-acp/bot-two-fir/config.json"
"$CONVERGE" two-fir --target-root "$fake" --apply >/dev/null
nbaks=$(find "$fake" -name "*.bak-*" | wc -l)
[ "$nbaks" -eq 1 ] && ok "backup created on overwrite" || bad "expected 1 backup, got $nbaks"

# fir-air: no config_path => config lands at the DEFAULT path
fake2="$tmpd/fakehost2"
mkdir -p "$fake2"
"$CONVERGE" fir-air --target-root "$fake2" --apply >/dev/null
[ -f "$fake2/.config/poe-acp/config.json" ] \
  && ok "fir-air config written to default path" || bad "fir-air config must land at ~/.config/poe-acp/config.json"
[ -f "$fake2/Library/LaunchAgents/dev.kfet.poe-acp.plist" ] \
  && ok "fir-air plist written" || bad "fir-air plist missing"

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]

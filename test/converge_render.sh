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
BOTS=(two-fir kopi-fir sea-fir fir-air acptmux)

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
for bot in two-fir kopi-fir sea-fir acptmux; do
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
# Track the lock rather than a hardcoded version: this asserts "already
# converged is detected", not "the fleet is pinned to X".
LOCKED_POE_ACP=$(jq -r .poe_acp "$ROOT/dist.lock")
printf '#!/bin/sh\necho %s\n' "$LOCKED_POE_ACP" >"$fake/.local/bin/poe-acp"
chmod +x "$fake/.local/bin/poe-acp"

out=$("$CONVERGE" two-fir --target-root "$fake")
case "$out" in
  *"DRY RUN"*) ok "dry run is the default" ;;
  *) bad "expected DRY RUN banner"; echo "$out" ;;
esac
case "$out" in
  *"poe-acp $LOCKED_POE_ACP ✓"*) ok "stub binary version matches lock" ;;
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

echo "== recycle mechanism selection"
plan() { "$CONVERGE" plan-recycle "$@"; }
#            supervisor    unit_changed running version has_worker has_reload
expect() { # <expected-mech> <label> <args...>
  local want=$1 label=$2; shift 2
  local got; got=$(plan "$@")
  case "$got" in
    "$want"|"$want|"*) ok "$label ($got)" ;;
    *) bad "$label: wanted $want, got $got" ;;
  esac
}
expect graceful "systemd: binary-only, running 0.53.0"  systemd-user 0 1 0.53.0 1 1
expect graceful "systemd: exactly 0.36.0 qualifies"     systemd-user 0 1 0.36.0 1 1
expect hard     "systemd: unit changed"                 systemd-user 1 1 0.53.0 1 1
expect hard     "systemd: running 0.35.0 < 0.36.0"      systemd-user 0 1 0.35.0 1 1
expect hard     "systemd: not running"                  systemd-user 0 0 ''     0 1
expect hard     "systemd: no ExecReload"                systemd-user 0 1 0.53.0 1 0
expect graceful "systemd: version unreadable, worker child present" systemd-user 0 1 '' 1 1
expect hard     "systemd: version unreadable, no worker child"      systemd-user 0 1 '' 0 1
expect graceful "launchd: worker child present"         launchd 0 1 ''     1 1
expect hard     "launchd: plist changed"                launchd 1 1 ''     1 1
expect hard     "launchd: no worker child (pre-0.36)"   launchd 0 1 ''     0 1
expect hard     "launchd: not running"                  launchd 0 0 ''     0 1

echo "== fake-target recycle (stubbed systemd: graceful swap vs hard restart)"
# Stub systemd + ps so the whole recycle/verify path runs offline. The stub
# supervisor pid never moves; each `reload` advances the worker pid, which is
# exactly the signature converge verifies after a graceful swap.
fake3="$tmpd/fakehost3"
mkdir -p "$fake3/.local/bin" "$fake3/.stub"
printf '%s\n' 999001 >"$fake3/.stub/worker"
printf '%s\n' 999000 >"$fake3/.stub/sup"
printf '%s\n' poe-acp >"$fake3/.stub/comm"
printf '#!/bin/sh\necho %s\n' "$LOCKED_POE_ACP" >"$fake3/.local/bin/poe-acp"
cat >"$fake3/.local/bin/systemctl" <<'STUB'
#!/bin/sh
s="$HOME/.stub"
echo "$*" >>"$s/log"
case "$*" in
  *"show -p MainPID"*)    cat "$s/sup" ;;
  *"show -p ExecReload"*) echo "/bin/kill -HUP \$MAINPID" ;;
  *is-active*)            echo active ;;
  *daemon-reload*)        : ;;
  *reload*)               expr "$(cat "$s/worker")" + 1 >"$s/worker" ;;
  # A restart replaces BOTH processes: new supervisor, new worker.
  *restart*)              expr "$(cat "$s/sup")" + 1 >"$s/sup"
                          expr "$(cat "$s/worker")" + 1 >"$s/worker" ;;
  *) ;;
esac
STUB
# `pgrep -P <supervisor>` — the tracked pid's children.
cat >"$fake3/.local/bin/pgrep" <<'STUB'
#!/bin/sh
cat "$HOME/.stub/worker"
STUB
# `ps -o comm= -p <pid>` — the image a pid is running. converge filters the
# child list by this: only children running the poe-acp image are workers.
cat >"$fake3/.local/bin/ps" <<'STUB'
#!/bin/sh
cat "$HOME/.stub/comm"
STUB
chmod +x "$fake3/.local/bin/poe-acp" "$fake3/.local/bin/systemctl" \
         "$fake3/.local/bin/ps" "$fake3/.local/bin/pgrep"

out=$("$CONVERGE" two-fir --target-root "$fake3")
case "$out" in
  *"would hard restart"*) ok "dry run previews the recycle mechanism (unit missing => hard)" ;;
  *) bad "dry run must preview the mechanism"; echo "$out" ;;
esac

# First apply writes config+unit => unit changed => hard restart is mandatory.
out=$("$CONVERGE" two-fir --target-root "$fake3" --apply)
case "$out" in
  *"hard restart (daemon-reload + restart) ✓"*) ok "unit change forces a hard restart" ;;
  *) bad "expected hard restart on unit change"; echo "$out" ;;
esac
case "$out" in *"unit changed"*) ok "hard restart reason reported" ;; *) bad "expected reason 'unit changed'" ;; esac
grep -q -- "--user daemon-reload" "$fake3/.stub/log" && ok "daemon-reload issued for a unit change" \
  || bad "unit change must daemon-reload"
grep -q -- "--user restart" "$fake3/.stub/log" && ok "restart issued" || bad "expected restart"
case "$out" in
  *"supervisor pid 999000 → 999001"*) ok "hard restart moved the supervisor pid" ;;
  *) bad "expected the supervisor pid to move across a hard restart"; echo "$out" ;;
esac

# Binary-only change (config edited, unit untouched) => graceful worker swap.
echo '{"x":0}' >"$fake3/.config/poe-acp/bot-two-fir/config.json"
out=$("$CONVERGE" two-fir --target-root "$fake3")
case "$out" in
  *"would graceful worker swap (SIGHUP)"*) ok "dry run previews a graceful swap for a config-only change" ;;
  *) bad "dry run must preview the graceful swap"; echo "$out" ;;
esac
: >"$fake3/.stub/log"
wbefore=$(cat "$fake3/.stub/worker")
echo '{"x":1}' >"$fake3/.config/poe-acp/bot-two-fir/config.json"
out=$("$CONVERGE" two-fir --target-root "$fake3" --apply)
case "$out" in
  *"graceful worker swap (SIGHUP) ✓"*) ok "config-only change does a graceful swap" ;;
  *) bad "expected graceful swap"; echo "$out" ;;
esac
grep -q -- "--user reload" "$fake3/.stub/log" && ok "reload issued" || bad "expected systemctl reload"
grep -q -- "restart" "$fake3/.stub/log" && bad "graceful path must not restart" || ok "no restart on the graceful path"
grep -q -- "daemon-reload" "$fake3/.stub/log" && bad "graceful path must not daemon-reload" \
  || ok "no daemon-reload when the unit did not change"
wafter=$(cat "$fake3/.stub/worker")
[ "$wbefore" != "$wafter" ] && ok "worker pid moved ($wbefore → $wafter)" || bad "worker pid must move"
case "$out" in
  *"supervisor $(cat "$fake3/.stub/sup") held"*) ok "supervisor pid verified unmoved" ;;
  *) bad "expected supervisor-held assertion"; echo "$out" ;;
esac

# A pre-0.36.0 host: the tracked process is a single relay whose children are
# ACP AGENTS, not workers. Those must not be read as a swap capability.
printf '%s\n' fir >"$fake3/.stub/comm"
echo '{"x":9}' >"$fake3/.config/poe-acp/bot-two-fir/config.json"
out=$("$CONVERGE" two-fir --target-root "$fake3")
case "$out" in
  *"would hard restart"*) ok "agent children are not workers (pre-0.36 => hard)" ;;
  *) bad "non-poe-acp children must not select the graceful path"; echo "$out" ;;
esac

# ... and a new NON-worker child must not satisfy the post-swap verification:
# a SIGHUP that only spawned an agent means the swap never took.
printf '%s\n' poe-acp >"$fake3/.stub/comm"
cat >"$fake3/.local/bin/systemctl" <<'STUB'
#!/bin/sh
s="$HOME/.stub"
case "$*" in
  *"show -p MainPID"*)    cat "$s/sup" ;;
  *"show -p ExecReload"*) echo "/bin/kill -HUP \$MAINPID" ;;
  *is-active*)            echo active ;;
  # The reload spawns a child, but it is an agent, not a new worker.
  *reload*)               expr "$(cat "$s/worker")" + 1 >"$s/worker"
                          echo fir >"$s/comm" ;;
esac
STUB
chmod +x "$fake3/.local/bin/systemctl"
echo '{"x":10}' >"$fake3/.config/poe-acp/bot-two-fir/config.json"
if "$CONVERGE" two-fir --target-root "$fake3" --apply >/dev/null 2>&1; then
  bad "a new non-worker child must not pass as a completed swap"
else
  ok "new child that is not the poe-acp image fails the swap verification"
fi
printf '%s\n' poe-acp >"$fake3/.stub/comm"

# A swap that does not take (worker pid frozen) must FAIL loudly.
cat >"$fake3/.local/bin/systemctl" <<'STUB'
#!/bin/sh
case "$*" in
  *"show -p MainPID"*)    cat "$HOME/.stub/sup" ;;
  *"show -p ExecReload"*) echo "/bin/kill -HUP \$MAINPID" ;;
  *is-active*)            echo active ;;
esac
STUB
chmod +x "$fake3/.local/bin/systemctl"
echo '{"x":2}' >"$fake3/.config/poe-acp/bot-two-fir/config.json"
if "$CONVERGE" two-fir --target-root "$fake3" --apply >/dev/null 2>&1; then
  bad "a swap that never forks a new worker must fail"
else
  ok "no new worker after SIGHUP fails loudly"
fi

# Same for the hard path: a restart that leaves the supervisor pid frozen
# means the service never actually came back on the new binary.
rm -f "$fake3/.config/systemd/user/poe-acp-two-fir.service"
if "$CONVERGE" two-fir --target-root "$fake3" --apply >/dev/null 2>&1; then
  bad "a restart that does not move the supervisor pid must fail"
else
  ok "frozen supervisor pid after a hard restart fails loudly"
fi

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]

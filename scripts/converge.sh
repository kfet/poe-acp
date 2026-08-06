#!/usr/bin/env bash
# converge.sh — the only sanctioned way to change a poe-acp bot host.
#
# Reads bots/<bot>.json + dist.lock and makes the target host match.
#
# Usage:
#   converge.sh <bot>                          dry-run (default): show what would change
#   converge.sh <bot> --apply                  actually converge the host
#   converge.sh <bot> --target-root DIR        act on a local fake host rooted at DIR
#                                              (no ssh; for testing)
#   converge.sh --tot                          resolve latest-of-everything ONCE and
#                                              rewrite dist.lock; NEVER converges
#   converge.sh render <bot> <artefact>        render one artefact to stdout
#                                              artefact: config | execstart | unit | plist
#
# Conventions:
#   - a spec.server key that is absent (or false/null) emits no flag at all;
#     present-and-true booleans emit a bare flag
#   - spec paths use ~/...; rendered as %h/... in systemd units and
#     "$HOME"/... in launchd plists
#   - rendering is canonical: flag order and dash style are normalised
#     (--flag, fixed order). First --apply on a host may rewrite a unit into
#     canonical form with identical semantics.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
BOTS_DIR="$REPO_ROOT/bots"
LOCK="$REPO_ROOT/dist.lock"
STAMP=$(date +%Y%m%d-%H%M%S)
TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

POE_ACP_REPO="https://github.com/kfet/poe-acp"
FIR_DIST_REPO="https://github.com/kfet/fir-dist"

die()  { echo "converge: error: $*" >&2; exit 1; }
note() { echo "  $*"; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"; }

need jq

# ---------------------------------------------------------------------------
# Spec access ($SPEC is set in main)
# ---------------------------------------------------------------------------
SPEC=""

jqs() { jq -r "$1" "$SPEC"; }

# server key helpers: "absent or false or null" => not emitted
skey() { jq -r --arg k "$1" '.server[$k] // empty' "$SPEC"; }
sflag() { # boolean flag present-and-true?
  [ "$(jq -r --arg k "$1" '.server[$k] == true' "$SPEC")" = "true" ]
}

# ---------------------------------------------------------------------------
# Path style conversion (spec uses ~/...)
# ---------------------------------------------------------------------------
p_systemd() { printf '%s' "${1/#\~/%h}"; }          # ~/x -> %h/x
p_home()    { printf '%s' "${1/#\~/\$HOME}"; }      # ~/x -> $HOME/x  (for remote shell)
p_plist()   { printf '%s' "${1/#\~\//\$HOME/}"; }   # ~/x -> $HOME/x  (inside sh -c string)

xml_escape() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }

# ---------------------------------------------------------------------------
# Renderers
# ---------------------------------------------------------------------------
render_config() { jq '.config' "$SPEC"; }

# render_execstart <style>   style: systemd (~ -> %h) | sh (~ -> $HOME)
render_execstart() {
  local style=$1 conv bin out v
  case "$style" in
    systemd) conv=p_systemd ;;
    sh)      conv=p_home ;;
    *)       die "render_execstart: bad style $style" ;;
  esac
  bin=$("$conv" "$(jqs '.binary')")
  # Guard content that would corrupt the rendered line: double quotes break
  # the shell quoting; % is a systemd specifier.
  v=$(skey introduction)
  case "$v" in *'"'*|*%*) die "introduction must not contain \" or %" ;; esac
  case "$(jqs '.agent.cmd')" in *'"'*|*%*) die "agent.cmd must not contain \" or %" ;; esac
  out="$bin"
  out+=" --http-addr $(skey http_addr)"
  out+=" --poe-path $(skey poe_path)"
  v=$(skey config_path); [ -n "$v" ] && out+=" --config $("$conv" "$v")"
  out+=" --agent-cmd \"$(jqs '.agent.cmd')\""
  v=$(skey heartbeat_interval); [ -n "$v" ] && out+=" --heartbeat-interval $v"
  v=$(skey session_ttl); [ -n "$v" ] && out+=" --session-ttl $v"
  sflag enable_mcp_attach && out+=" --enable-mcp-attach"
  v=$(skey introduction); [ -n "$v" ] && out+=" --introduction \"$v\""
  printf '%s\n' "$out"
}

render_unit() {
  local v
  cat <<EOF
[Unit]
Description=poe-acp ($(jqs '.name'))
After=network-online.target

[Service]
EOF
  v=$(jqs '.systemd.env_path // empty')
  [ -n "$v" ] && echo "Environment=PATH=$v"
  echo "EnvironmentFile=$(p_systemd "$(jqs '.credentials.env_file')")"
  # Type=notify + ExecReload are required for the supervisor readiness
  # handshake and graceful SIGHUP worker swap on every locked poe-acp
  # version (>= 0.36) — emitted unconditionally.
  echo "Type=notify"
  echo "ExecStart=$(render_execstart systemd)"
  # shellcheck disable=SC2016  # $MAINPID is expanded by systemd, not the shell
  echo 'ExecReload=/bin/kill -HUP $MAINPID'
  v=$(jqs '.systemd.restart // empty')
  [ -n "$v" ] && echo "Restart=$v"
  v=$(jqs '.systemd.restart_sec // empty')
  [ -n "$v" ] && echo "RestartSec=$v"
  cat <<'EOF'

[Install]
WantedBy=default.target
EOF
}

render_plist() {
  local label env_file cmd path_env log_out log_err
  label=$(jqs '.unit')
  env_file=$(p_plist "$(jqs '.credentials.env_file')")
  cmd="set -a; . \"$env_file\"; set +a; exec $(render_execstart sh)"
  path_env=$(jqs '.launchd.path_env')
  # launchd does not expand $HOME in Standard*Path — spec must hold
  # absolute log paths; rendered literally.
  log_out=$(jqs '.launchd.log_out')
  log_err=$(jqs '.launchd.log_err')
  cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>$(printf '%s' "$cmd" | xml_escape)</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>$(printf '%s' "$path_env" | xml_escape)</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$log_out</string>
  <key>StandardErrorPath</key><string>$log_err</string>
</dict>
</plist>
EOF
}

# ---------------------------------------------------------------------------
# Remote execution (ssh by host alias, or a local fake root for testing)
# ---------------------------------------------------------------------------
HOST=""
TARGET_ROOT=""

rsh() { # run a shell command on the target; stdin is forwarded
  if [ -n "$TARGET_ROOT" ]; then
    HOME="$TARGET_ROOT" bash -c "$1"
  else
    # shellcheck disable=SC2029  # remote-side expansion is intended
    ssh "$HOST" "$1"
  fi
}

# rcat <spec-path>  — print remote file; empty if missing; die on transport error
rcat() {
  local rc=0
  rsh "p=\"$(p_home "$1")\"; if [ -f \"\$p\" ]; then cat \"\$p\"; else exit 42; fi" || rc=$?
  case "$rc" in 0|42) : ;; *) die "failed reading $1 (rc=$rc — ssh/transport error?)" ;; esac
}

# rwrite <spec-path> <local-content-file> — backup + write
rwrite() {
  rsh "p=\"$(p_home "$1")\"; mkdir -p \"\$(dirname \"\$p\")\"; \
       [ -f \"\$p\" ] && cp -p \"\$p\" \"\$p.bak-$STAMP\"; cat > \"\$p\"" <"$2"
}

# ---------------------------------------------------------------------------
# Diff helper: diff_artifact <label> <spec-path> <desired-content-file>
# Prints a diff if different. Returns 0 if identical, 1 if different.
# ---------------------------------------------------------------------------
diff_artifact() {
  local label=$1 rpath=$2 want=$3 have
  have=$(mktemp "$TMPD/have.XXXXXX")
  rcat "$rpath" >"$have"
  if diff -u --label "$label (current)" --label "$label (desired)" "$have" "$want"; then
    rm -f "$have"
    return 0
  fi
  rm -f "$have"
  return 1
}

# ---------------------------------------------------------------------------
# Converge
# ---------------------------------------------------------------------------
converge() {
  local bot=$1 apply=$2 changes=0 unit_changed=0
  local want_pa want_fir want_ext cur host supervisor binary

  [ -f "$LOCK" ] || die "missing $LOCK"
  want_pa=$(jq -r '.poe_acp' "$LOCK")
  want_fir=$(jq -r '.fir' "$LOCK")
  want_ext=$(jq -r '.exts["github.com/kfet/fir-exts"]' "$LOCK")

  host=$(jqs '.host')
  supervisor=$(jqs '.supervisor')
  binary=$(jqs '.binary')
  HOST="$host"

  echo "== converge $bot (host=$host supervisor=$supervisor)$([ -n "$TARGET_ROOT" ] && echo " [fake root: $TARGET_ROOT]")"
  [ "$apply" = 1 ] || echo "== DRY RUN — no changes will be made (use --apply)"

  # Preflight: fail loudly on an unreachable target rather than mistaking
  # transport failure for missing files.
  rsh true || die "cannot reach target ($host)"

  # -- 1. poe-acp version ---------------------------------------------------
  cur=$(rsh "\"$(p_home "$binary")\" --version 2>/dev/null || true")
  if [ "$cur" = "$want_pa" ]; then
    note "poe-acp $cur ✓"
  else
    changes=$((changes + 1))
    note "poe-acp: ${cur:-missing} → $want_pa"
    if [ "$apply" = 1 ]; then
      if [ -n "$TARGET_ROOT" ]; then
        note "skipping poe-acp fetch (fake root)"
      else
      local os arch url
      os=$(jqs '.platform' | cut -d/ -f1)
      arch=$(jqs '.platform' | cut -d/ -f2)
      url="$POE_ACP_REPO/releases/download/v$want_pa/poe-acp-$os-$arch"
      note "fetching $url"
      rsh "set -e; b=\"$(p_home "$binary")\"; t=\$(mktemp); \
           curl -fsSL -o \"\$t\" \"$url\"; chmod +x \"\$t\"; \
           [ -f \"\$b\" ] && cp -p \"\$b\" \"\$b.bak-$STAMP\"; mv \"\$t\" \"\$b\""
      cur=$(rsh "\"$(p_home "$binary")\" --version")
      [ "$cur" = "$want_pa" ] || die "poe-acp still $cur after fetch (wanted $want_pa)"
      note "poe-acp now $cur ✓"
      fi
    fi
  fi

  # -- 2. fir version -------------------------------------------------------
  if [ "$(jqs '.agent.kind')" = "fir" ]; then
    cur=$(rsh "fir --version 2>/dev/null | head -1 | awk '{print \$2}' || true")
    if [ "$cur" = "$want_fir" ]; then
      note "fir $cur ✓"
    else
      changes=$((changes + 1))
      note "fir: ${cur:-missing} → $want_fir"
      if [ "$apply" = 1 ]; then
        if [ -n "$TARGET_ROOT" ]; then
          note "skipping fir update (fake root)"
        else
        rsh "fir update" || true
        cur=$(rsh "fir --version 2>/dev/null | head -1 | awk '{print \$2}' || true")
        [ "$cur" = "$want_fir" ] || die "fir is $cur after 'fir update', lock wants $want_fir — releases moved past the lock? Re-run --tot or fetch v$want_fir from $FIR_DIST_REPO/releases manually"
        note "fir now $cur ✓"
        fi
      fi
    fi

    # -- 3. fir extensions at locked rev ------------------------------------
    local ext rev pkg_path
    for ext in $(jqs '.fir.exts[]? // empty'); do
      # SOURCE column may be the bare slug or the full install URL — match on substring.
      pkg_path=$(rsh "fir packages list 2>/dev/null | awk -v s=\"$ext\" 'index(\$1, s) {print \$NF}' || true")
      if [ -z "$pkg_path" ]; then
        changes=$((changes + 1))
        note "ext $ext: not installed → install @ ${want_ext:0:12}"
        if [ "$apply" = 1 ] && [ -z "$TARGET_ROOT" ]; then
          rsh "fir install \"https://$ext\""
          pkg_path=$(rsh "fir packages list 2>/dev/null | awk -v s=\"$ext\" 'index(\$1, s) {print \$NF}' || true")
        fi
      fi
      if [ -n "$pkg_path" ]; then
        rev=$(rsh "git -C \"$(p_home "$pkg_path")\" rev-parse HEAD 2>/dev/null || true")
        if [ "$rev" = "$want_ext" ]; then
          note "ext $ext @ ${rev:0:12} ✓"
        elif [ -z "$rev" ]; then
          note "WARN: ext $ext installed at $pkg_path but rev unreadable — verify manually"
        else
          changes=$((changes + 1))
          note "ext $ext: ${rev:0:12} → ${want_ext:0:12}"
          if [ "$apply" = 1 ] && [ -z "$TARGET_ROOT" ]; then
            rsh "fir packages update \"$ext\""
            rev=$(rsh "git -C \"$(p_home "$pkg_path")\" rev-parse HEAD 2>/dev/null || true")
            if [ "$rev" != "$want_ext" ]; then
              note "WARN: ext $ext is at ${rev:0:12} after update, lock wants ${want_ext:0:12} (fir cannot pin an arbitrary rev; re-run --tot if upstream moved)"
            fi
          fi
        fi
      fi
    done
  fi

  # -- 4. config.json -------------------------------------------------------
  local cfg_path want_file
  cfg_path=$(skey config_path)
  # shellcheck disable=SC2088  # spec-style path; expanded later via p_home
  [ -n "$cfg_path" ] || cfg_path="~/.config/poe-acp/config.json"  # no --config flag => default path
  want_file=$(mktemp "$TMPD/want.XXXXXX")
  render_config >"$want_file"
  if diff_artifact "config.json" "$cfg_path" "$want_file"; then
    note "config $cfg_path ✓"
  else
    changes=$((changes + 1))
    if [ "$apply" = 1 ]; then
      rwrite "$cfg_path" "$want_file"
      note "config written (backup: $cfg_path.bak-$STAMP)"
    fi
  fi
  rm -f "$want_file"

  # -- 5. unit / plist ------------------------------------------------------
  local unit_path
  case "$supervisor" in
    systemd-user)
      # shellcheck disable=SC2088  # spec-style path; expanded later via p_home
      unit_path="~/.config/systemd/user/$(jqs '.unit').service"
      want_file=$(mktemp "$TMPD/want.XXXXXX"); render_unit >"$want_file" ;;
    launchd)
      unit_path=$(jqs '.launchd.plist')
      want_file=$(mktemp "$TMPD/want.XXXXXX"); render_plist >"$want_file" ;;
    *) die "unknown supervisor: $supervisor" ;;
  esac
  if diff_artifact "$supervisor unit" "$unit_path" "$want_file"; then
    note "unit $unit_path ✓"
  else
    changes=$((changes + 1)); unit_changed=1
    if [ "$apply" = 1 ]; then
      rwrite "$unit_path" "$want_file"
      note "unit written (backup: $unit_path.bak-$STAMP)"
    fi
  fi
  rm -f "$want_file"

  # -- 6. recycle + verify --------------------------------------------------
  if [ "$changes" = 0 ]; then
    echo "== $bot: already converged, nothing to do"
    return 0
  fi
  if [ "$apply" != 1 ]; then
    echo "== $bot: $changes change(s) pending (dry run; re-run with --apply)"
    return 0
  fi
  if [ -n "$TARGET_ROOT" ]; then
    echo "== $bot: applied to fake root; skipping service recycle/verify"
    return 0
  fi
  # verify_up <check-command> — services need a moment after (re)start
  verify_up() {
    local i
    for i in 1 2 3 4 5; do
      rsh "$1" >/dev/null 2>&1 && return 0
      [ "$i" = 5 ] || sleep 1
    done
    return 1
  }
  local unit label
  case "$supervisor" in
    systemd-user)
      unit="$(jqs '.unit').service"
      rsh "systemctl --user daemon-reload && systemctl --user restart $unit"
      verify_up "systemctl --user is-active $unit" || die "$unit not active after restart"
      note "$unit restarted, active ✓" ;;
    launchd)
      label=$(jqs '.unit')
      if [ "$unit_changed" = 1 ]; then
        # kickstart -k re-runs the CACHED job definition and ignores an
        # edited plist. Plist changes require bootout + bootstrap.
        rsh "launchctl bootout gui/\$(id -u)/$label 2>/dev/null || true"
        rsh "launchctl bootstrap gui/\$(id -u) \"$(p_home "$unit_path")\""
        note "$label booted out + bootstrapped (plist changed)"
      else
        rsh "launchctl kickstart -k gui/\$(id -u)/$label"
        note "$label kickstarted"
      fi
      verify_up "launchctl print gui/\$(id -u)/$label | grep -q 'state = running'" \
        || die "$label not running after recycle" ;;
  esac
  cur=$(rsh "\"$(p_home "$binary")\" --version")
  [ "$cur" = "$want_pa" ] || die "post-recycle version check failed: $cur != $want_pa"
  echo "== $bot: converged ($changes change(s) applied)"
}

# ---------------------------------------------------------------------------
# --tot: resolve latest-of-everything ONCE into dist.lock, then STOP.
# Resolution never happens on a host, and tot never converges.
# ---------------------------------------------------------------------------
latest_tag() { # <repo-url>
  git ls-remote --tags "$1" \
    | awk '{print $2}' | sed 's|^refs/tags/||' | grep -v '\^{}$' \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 | sed 's/^v//'
}

head_rev() { # <repo-url>
  git ls-remote "$1" HEAD | awk 'NR==1 {print $1}'
}

tot() {
  need git
  local old_pa old_fir old_ext new_pa new_fir new_ext moved=0
  if [ -f "$LOCK" ]; then
    old_pa=$(jq -r '.poe_acp' "$LOCK")
    old_fir=$(jq -r '.fir' "$LOCK")
    old_ext=$(jq -r '.exts["github.com/kfet/fir-exts"]' "$LOCK")
  else
    old_pa=none; old_fir=none; old_ext=none
  fi

  echo "== tot: resolving latest releases (this is the ONLY place resolution happens)"
  new_pa=$(latest_tag "$POE_ACP_REPO")
  new_fir=$(latest_tag "$FIR_DIST_REPO")
  new_ext=$(head_rev "https://github.com/kfet/fir-exts")
  [ -n "$new_pa" ]  || die "could not resolve latest poe-acp tag"
  [ -n "$new_fir" ] || die "could not resolve latest fir-dist tag"
  [ -n "$new_ext" ] || die "could not resolve fir-exts HEAD"

  [ "$old_pa" != "$new_pa" ]   && { note "poe-acp: $old_pa → $new_pa"; moved=1; }
  [ "$old_fir" != "$new_fir" ] && { note "fir:     $old_fir → $new_fir"; moved=1; }
  [ "$old_ext" != "$new_ext" ] && { note "fir-exts: ${old_ext:0:12} → ${new_ext:0:12}"; moved=1; }
  [ "$moved" = 0 ] && { echo "== tot: lock already at latest, nothing moved"; return 0; }

  jq -n --arg pa "$new_pa" --arg fir "$new_fir" --arg ext "$new_ext" \
        --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        '{poe_acp: $pa, fir: $fir, exts: {"github.com/kfet/fir-exts": $ext}, resolved_at: $at}' \
    >"$LOCK"
  echo "== tot: dist.lock rewritten. Review the diff, commit it, then converge each bot:"
  echo "   git diff dist.lock"
  for f in "$BOTS_DIR"/*.json; do
    echo "   scripts/converge.sh $(basename "$f" .json) --apply"
  done
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
usage() {
  cat >&2 <<'EOF'
usage:
  converge.sh <bot>                          dry-run (default): show what would change
  converge.sh <bot> --apply                  actually converge the host
  converge.sh <bot> --target-root DIR        act on a local fake host rooted at DIR (no ssh)
  converge.sh --tot                          resolve latest-of-everything ONCE and
                                             rewrite dist.lock; NEVER converges
  converge.sh render <bot> <artefact>        render one artefact to stdout
                                             artefact: config | execstart | unit | plist
EOF
  exit 1
}

[ $# -ge 1 ] || usage

case "$1" in
  --tot)
    [ $# -eq 1 ] || die "--tot takes no other arguments (tot never converges)"
    tot ;;
  render)
    [ $# -eq 3 ] || usage
    SPEC=$(spec="$BOTS_DIR/$2.json"; [ -f "$spec" ] || die "no such bot spec: $spec"; echo "$spec")
    case "$3" in
      config)    render_config ;;
      execstart) render_execstart systemd ;;
      unit)      render_unit ;;
      plist)     render_plist ;;
      *)         usage ;;
    esac ;;
  -*) usage ;;
  *)
    BOT=$1; shift
    SPEC="$BOTS_DIR/$BOT.json"
    [ -f "$SPEC" ] || die "no such bot spec: $SPEC"
    APPLY=0
    while [ $# -gt 0 ]; do
      case "$1" in
        --apply) APPLY=1; shift ;;
        --dry-run) APPLY=0; shift ;;
        --target-root) TARGET_ROOT=$(cd "$2" && pwd) || die "bad --target-root"; shift 2 ;;
        *) usage ;;
      esac
    done
    converge "$BOT" "$APPLY" ;;
esac

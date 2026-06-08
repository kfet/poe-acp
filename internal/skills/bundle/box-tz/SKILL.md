---
builtin: true
name: box-tz
description: Set a host (or the whole tailnet fleet) to a fixed permanent Pacific offset (UTC-7, no DST) — TIL on timezone setup across Linux/macOS boxes via a jumpbox.
---

# Box Timezone Setup (TIL)

Pin a box — or the whole tailnet fleet — to **permanent UTC-7, no DST switching**
(the year-round Pacific offset BC observes). Use the fixed zone `Etc/GMT+7`, not
`America/Vancouver` (which still springs forward / falls back).

## Gotchas

- **`Etc/GMT+7` is UTC-7**, not +7. POSIX inverts the sign in `Etc/GMT*` zones.
  It renders as `-07` / `-0700` with no DST transitions.
- There is **no tz named "PT"**. A fixed-offset zone is the honest way to get one
  offset all year. Accept the `-07` label.
- `timedatectl set-timezone` often needs interactive polkit auth and fails under
  ssh; the symlink method below works with plain `sudo -n`.

## Linux (per box)

```sh
sudo -n ln -sf /usr/share/zoneinfo/Etc/GMT+7 /etc/localtime
echo "Etc/GMT+7" | sudo -n tee /etc/timezone >/dev/null
date            # verify: ... -07 ...
```

No daemon restart is needed for the clock itself, but long-running services that
cached the zone at start (loggers, cron-driven jobs) should be restarted to pick
it up.

## macOS

Use the proper API; it needs **interactive sudo** (no passwordless ssh path):

```sh
sudo systemsetup -settimezone Etc/GMT+7
```

If `Etc/GMT+7` is rejected by `systemsetup -listtimezones`, symlink
`/etc/localtime` to `/usr/share/zoneinfo/Etc/GMT+7` interactively instead.

## Fleet fan-out via jumpbox

Enumerate hosts, then drive each through the jumpbox with `ssh -J`:

```sh
tailscale status                      # list peers; skip offline/phones
for h in <linux-hosts>; do
  echo "=== $h ==="
  ssh -J <jumpbox> -o ConnectTimeout=10 -o BatchMode=yes "$h" \
    'sudo -n ln -sf /usr/share/zoneinfo/Etc/GMT+7 /etc/localtime; \
     echo "Etc/GMT+7" | sudo -n tee /etc/timezone >/dev/null; date'
done
```

Skip iOS/Android peers and any host `tailscale status` shows as `offline`.
Hosts that answer `Permission denied (publickey)` need a key from the jumpbox
first; hosts that time out during banner exchange are down/overloaded — note
them and move on rather than blocking the fan-out.

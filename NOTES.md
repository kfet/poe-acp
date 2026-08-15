# NOTES — open questions from the self-heal build

Two forks I could not settle from the code. Both are deliberately left
unbuilt rather than guessed at.

## 1. `poe-acp fleet` does not exist

The brief says status.json is read by "`poe-acp fleet` ssh-cats it from
wherever it's run". There is no `fleet` subcommand in this repo (and no
Go-side knowledge of `bots/*.json`, which is `scripts/converge.sh`'s
territory). I did not invent one: `status.json` is a plain file, and

```bash
ssh <host> cat ~/.config/poe-acp/bot-<bot>/status.json
```

already works today. Question: should `fleet` be a new Go subcommand that
walks `bots/*.json` (host + `server.config_path`), or a `converge.sh
status` verb, so host knowledge stays in one place? The latter looks
right — but it means touching `scripts/converge.sh`, which the brief
forbids.

## 2. `dist.lock` still pins fir with a bare string

The lock schema now accepts `{"version": "...", "policy": "floor"}` and a
bare string still means `pin`, so the committed `dist.lock` parses
unchanged. I did NOT switch `fir` to `floor` in the file, because
`scripts/converge.sh` reads it with `jq -r '.fir'` and would render the
whole object into a version string — and converge.sh is out of bounds for
this change. Question: land a one-line converge.sh change
(`.fir | if type == "object" then .version else . end`) in a follow-up,
then flip the lock to floor?

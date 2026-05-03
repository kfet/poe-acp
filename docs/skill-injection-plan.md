# Skill Injection — Plan / Handoff

Branch: `skill-injection`  
Worktree: `~/dev/ai/poe-acp-relay.wt/skill-injection`

## Goal

Make poe-acp-relay a *universal* ACP relay that injects a small, Poe-environment-aware **skills catalog** into any ACP agent it fronts. Agents read skill bodies on demand using whatever read tool they have.

Pattern is copied from fir's `<available_skills>` block (see `~/dev/ai/fir/pkg/resources/skills.go:259`).

## Design (settled)

1. **Skill bundle** — markdown files embedded in the relay binary via `go:embed`. On startup, extracted to a per-install tmp dir (`os.TempDir()/poe-acp-relay-<version>-<hash>/skills/`). Idempotent.
2. **Injected payload = catalog only**, not skill bodies. fir-style XML:
   ```
   <available_skills>
     <skill>
       <name>...</name>
       <description>...</description>
       <location>/abs/path/SKILL.md</location>
     </skill>
   </available_skills>
   ```
   With a short preamble: "use your read tool to load a skill when its description matches the task."
3. **Delivery — capability-negotiated**:
   - Capability: `_meta["session.systemPrompt"]` (generic, version 1), advertised in `initialize` by both client and agent.
   - **If agent advertises**: relay sends catalog as `session/new._meta["session.systemPrompt"].blocks = [{type:"text", text:"..."}]`. Agent treats it as durable system context.
   - **Fallback**: prepend the catalog as a `text` content block on the **first** `session/prompt`, with a "preserve verbatim across summarisation" instruction. Re-inject on `session/load`.
4. **Out of scope**: MCP, `resource_link`/`fs/read_text_file` reliance, override paths, v2 cap fields. Keep it minimal.

## Initial skill set (v1)

Ship the **three existing skills** in `.fir/skills/` as-is — they are already the right relay-specific catalog and require no augmentation:

- **`deploy`** — initial deploy to a Tailscale-Funnel host, start as Poe bot, end-to-end verify. (`.fir/skills/deploy/SKILL.md`)
- **`update`** — upgrade to latest release on one host and restart the supervisor. Already covers brew+launchd (macOS), brew+systemd (Linux), and direct-deploy paths with pitfalls and a checklist. (`.fir/skills/update/SKILL.md`)
- **`release`** — version bump, CHANGELOG, tag, push. (`.fir/skills/release/SKILL.md`)

The launchctl/systemd story flagged as "not yet automated" in the README is already documented in the `update` skill — no new content needed for v1.

Implementation note: move (or copy) these files from `.fir/skills/` into a relay-owned location (e.g. `skills/` at repo root) and `go:embed` from there. Decide whether `.fir/skills/` continues to exist for fir-local dev or gets replaced by a symlink to `skills/`.

Future skills (not in this branch): attachments, rendering quirks, conv-id semantics, no-persistence caveats.

## Deliverables

### This handoff branch

1. ✅ `docs/skill-injection-plan.md` (this file).
2. ✅ `acp-spec/rfd-system-prompt.md` — RFD doc for the `session.systemPrompt` capability extension, mirroring `rfd-auth-methods.md` shape. Source of truth before code lands.

### Follow-up branches (not started)

3. ✅ `internal/skills/` — embed + extract logic, catalog formatter (XML, fir-style).
4. ✅ `internal/router/` — capability negotiation; injection at `session/new` (cap path) or first `session/prompt` (fallback); re-inject on resume.
5. ✅ `internal/acpclient/agent.go` — advertise `clientCapabilities._meta["session.systemPrompt"] = {version:1}` in `initialize`; surface agent's matching cap as `Caps.SystemPrompt`.
6. ✅ Skill content: relocated `.fir/skills/{deploy,update,release}/SKILL.md` to `internal/skills/bundle/` and `go:embed`-ed from there. `.fir/skills` is now a symlink so fir-local dev still resolves. Only SKILL.md files whose frontmatter declares `builtin: true` are surfaced to ACP agents — others (e.g. `release`) live in the bundle tree for git/symlink coherence but stay out of the catalog. Mirrors fir's own `pkg/resources/builtin_skills` loader.
7. ✅ Tests: catalog rendering (`internal/skills/skills_test.go`), capability parsing (`internal/acpclient/agent_test.go`), cap-path / fallback / resume injection (`internal/router/system_prompt_test.go`).

## Open questions deferred

- Exact tmp-dir naming/cleanup policy (per-version dirs accumulate; trivial to GC oldest on startup, but not v1).
- How to signal "compaction happened, re-inject" in the fallback path — punted, accepted as known limitation.
- Whether to also push the RFD upstream to `agentclientprotocol/agent-client-protocol`. Decide after fir lands matching support.

## Context for the next session

- The relay already advertises `clientCapabilities.fs.{ReadTextFile,WriteTextFile}=true` (`internal/acpclient/agent.go:168`). We discussed but rejected relying on agents calling `fs/read_text_file` — too variable across agents. Inline catalog text is the contract.
- `_meta` extension pattern is established in this repo: see `_meta.auth.interactive` work (commit 81f6aeb) and `acp-spec/rfd-auth-methods.md` for the shape to mirror.
- fir's skill bundling: `pkg/resources/skills.go:259` (`FormatSkillsForPrompt`) is the canonical reference for the XML format and preamble text.
- User wants this *universal* — not fir-specific. The cap name and RFD must be agent-agnostic.

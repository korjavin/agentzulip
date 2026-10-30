---
name: az-curator
description: Retrospective on a team of long-lived role agents that coordinate through Zulip with zm (the agent-Zulip flow) — read the Zulip channels, the daemon's journal, Claude transcripts, Codex rollouts and the launcher, find what the flow wasted or broke (reviewer harness not starting, agents waiting on nobody, wake storms, stalls between agents, owner friction, harness refusals), and turn it into evidence-backed changes to role cards, the zulip-agents skill, zm and the launcher. Fable-class. Trigger on "/az-curator", "audit the agent team", "retro on the zulip agents", "why is the reviewer a problem", "are the wake-ups wasting tokens".
---

# /az-curator — learn from how the agent team actually ran

Same job as the **curator** skill (`~/.claude/skills/curator/SKILL.md`: judgment backed by numbers,
process changes only, your context is the scarce thing, guardrails) for a team that talks through
Zulip. Read that skill's **Method** steps 1, 2, 5–7 and **Guardrails**; this one replaces its sources,
tooling and lenses, which do not see this flow. Reports go to
`~/.claude/projects/<slug>/audit/<YYYY-MM-DD>-az-curator.md` (outside the repo).

**Flow being audited:** role agents in their own zmx sessions, talking only through `zm`; a daemon
bot wakes a session by typing `[zm] N new mention(s) for <role>…` into it; two channels (the owner's
and `<channel>-work`); orchestrators spawn developers (develop route 4z) that post the PR in the
bead's topic, start a **fresh `codex exec` as the reviewer role** and `zm wait` for its `VERDICT:`.

## Sources (what the generic curator misses)

| What | Where / how |
|---|---|
| **Outsourced digest** — read FIRST when present | `audit/<date>-az-digest/digest.md` (ranked problems, per role, ping-pong, owner friction) and `findings.md` (every finding with evidence pointers). Made by `scripts/compact.py` + `scripts/outsource.py` (Claude API batch, key in stash, ~$30 for a week): use it instead of reading transcripts yourself; open raw sources only to verify a finding you will act on |
| **Zulip, both channels** — the primary record | `scripts/zulip_dump.py --rc <zuliprc> --channel <ch> --channel <ch>-work --since … --out dump.json` (GET only, key never printed), then `scripts/zulip_lens.py dump.json --owner <name> --reviewer <role> --daemon <name>` |
| Daemon journal — wakes, restarts, backfill | `scripts/daemon_journal.py [--unit <unit>] --since … [--split <team restart time>]`, or `--file` on a saved `journalctl` dump |
| Role sessions + developer subagents | `scripts/transcripts.py ~/.claude/projects/<slug> [<slug>--claude-worktrees-*…] --since … --owner <name> --reviewer <role>`. A `[zm] … mention(s)` prompt is the daemon, not the owner; the script separates them |
| Codex rollouts (reviewer, sparring partner) | `scripts/codex_rollouts.py --since … [--role <r>] [--cwd-substr <text>]` — identified by `zm -as <role>` text; cwd alone finds developers' `codex review` runs, not the reviewer |
| Launcher / restart scripts | `~/.config/zm/*.sh` (which harness, which flags — e.g. a Codex role without approval bypass; env hygiene — an inherited `CLAUDE*` env leaves a session with **no transcript**) |
| Live screens; agy roles | `zmx history <session>` (read-only); agy: `~/.gemini/antigravity-cli/history.jsonl` (wake prompts) and `conversations/<id>.db` (protobuf in sqlite — only `strings | grep 'zm -as'` counts) |
| Generic sources | curator's digest.py for per-session overview (its cost column is `$?` here: use the token sums), `bd`, `git log`, `gh pr list`, earlier audits |

Never `zm send/ack/resolve/join/register`, never type into a session. Reading is fine:
`zm -as <daemon-role> read -c <ch> -t <topic> -n 200` or the dump.

## Method

1. **Scope**: project slug(s), channel pair, owner/reviewer/daemon display names, window (default:
   since the newest `*-az-curator.md`, else the daemon's first start in the journal).
2. **Close the loop** on the last audit (curator step 2).
3. **Extract first, read second.** Run the five scripts into your scratch dir and read their output;
   they are small and structured — most findings come out of them with a number and a pointer.
   Spawn reader subagents (curator step 4) only for prose questions the scripts can't answer
   (why a developer chose X), one per question, with transcript paths and the exact question.
4. **Judge, report, apply** as the curator does (steps 5–7). Project files behind a PR;
   skill/role-card/launcher changes to their source repos; subagent mode applies nothing.

## Lenses

- **Reviewer / route 4z.** Reviewer rollouts ÷ code PRs; launches refused (by shape, below) and the
  stdin-vs-inline command form; developers that posted a review request and `zm wait`ed with **no
  harness started** (waits/timeouts with launch `-`); request→first-post latency; approve-under-60s
  with 0 waits on PRs later re-fixed; rounds ≥3; verdict missing; fallback share (`codex review`);
  tokens per run. Match verdicts against bugs the owner found (`bd list`, owner messages).
- **Wakes.** Wakes per role per hour, storm minutes, daemon restarts and the backfill that follows,
  broadcasts with many mentions (fan-out = that many full-context turns), wakes that ended in
  `(no unread mentions)`, `inbox` polled several times in one turn instead of ending the turn or
  one `zm wait`. Role sessions sit at a large context; every wake re-bills it.
- **Stalls.** Mentions of a role agent with no reaction and no reply in the topic (zulip_lens
  "stalls" — an upper bound, read the examples); an agent "waiting on" another for long; a Codex
  role blocked on its own approval judge (rollout "approval-block phrases" plus the "You are
  judging…" rollouts it spawns); `zm restart` requested but no `restarting` line in the journal.
- **Owner UX, read from Zulip.** Owner messages re-posted with an @mention after silence (no
  mention → no wake); agents asking the owner to do mechanical steps (paste, confirm, approve a
  prompt in a session); agent posts and long posts in owner-started topics after the quiet-topic
  rule; agent→owner mentions that carry no decision; time between an outage (quota limit seen in a
  transcript) and the first line to the owner; owner typing into a console instead of Zulip.
- **Harness refusals by command shape.** Worktree guard ("Refusing to run it … cannot be shown
  not to be git"), auto-mode classifier ("judged this action dangerous", "Credential Exploration"),
  PreToolUse hooks, "Blocked: sleep …" — each recurring shape is one line of skill text (the
  working form) or a zm feature (what the agent was reaching for).
- **Generic lenses still apply**: context & tokens, model tiers, missed bugs (PR → fix-bead
  chains), rework, rule drift against role cards / zulip-agents, tooling errors; revmux only if used.

## Guardrails (in addition to the curator's)

- Read-only on Zulip: nothing posted, reacted, acked, resolved or marked read; no `zmx send`.
- The report names roles, topics and bead ids but **no** server URL or domain, bot or human
  e-mail, API key, account id, upload URL, host or session name beyond the role; strip
  `/user_uploads/…` paths and `op:` tokens from quoted messages.
- A role card is short; prefer one sentence in the shared skill over one per role card, and
  deleting a stale line (a hedge like "no such skill →") over adding.

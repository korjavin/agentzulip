# Prompt: outsourced audit of an agent team (paste into a fresh Claude Code session on the host)

Run it in a plain terminal on the host where the team runs, not inside an agent's session.
Fill in the two blanks first: the stash path of the Anthropic API key and the budget.

```text
Prepare the history of the agent team on this host for an az-curator review. The reading itself is
outsourced to the Claude API (a Message Batch) so it doesn't cost this session's tokens: you only
run scripts and check sizes; don't read transcripts or Zulip yourself.
API key: in stash at <STASH PATH, e.g. secrets/<team>/anthropic>. Budget: $<N>.
Secrets stay in stash; never print the key. Read-only on Zulip: nothing posted, reacted or marked read.

0. Get the scripts: `git -C <agentzulip clone> pull` (github.com/korjavin/agentzulip; usually
   ~/.local/share/zm/agentzulip or ~/Projects/agentzulip). They are in skill/az-curator/scripts:
   compact.py, outsource.py, zulip_dump.py. Make a work dir W in your scratchpad and a venv:
   `python3 -m venv $W/venv && $W/venv/bin/pip install -q anthropic`.
1. Find the team: ~/.config/zm/*.zuliprc ([zm] channel = the owner channel; the work channel is
   that name + "-work"; the daemon's rc has `restart =`), the launcher ~/.config/zm/launch*.sh (the
   project dir and the roles: name, harness), the role cards in ~/.local/share/zm/cards or
   ~/.claude/skills/role-*. Team start = the date of the first message in the work channel (or of
   the first zm commit on this host). Ask me only if something is ambiguous.
2. Extract (no API calls):
   - `python3 -I compact.py $W/units --since <start date> ~/.claude/projects/<project slug>*`
     (the slug is the project dir with / replaced by -; the * picks up its worktrees).
   - `python3 -I zulip_dump.py --rc ~/.config/zm/<daemon>.zuliprc --channel <ch> --channel <ch>-work
     --since "<start> 00:00" --out $W/zulip.json`
   - `journalctl --user -u zm-porter --no-pager > $W/journal.txt` (the daemon's unit; check its name
     with `systemctl --user list-units 'zm*'`).
   - $W/context.md: one line saying the rules evolved and the text is the latest version; a short
     team list (each role: name, job, harness, from the launcher and cards; the owner's display
     name; the daemon bot's name; the product in a few words); then the full text of the
     zulip-agents skill (skill/zulip-agents/SKILL.md).
3. `cd $W && OUTSOURCE_KEY_PATH=<STASH PATH> ./venv/bin/python -I <scripts>/outsource.py estimate --work .`
   Tell me the units and the estimate. If it is over the budget, stop and propose what to cut
   (e.g. a later --since, or fewer transcript dirs).
4. Pilot one unit as a direct call before the batch (about $0.50): import outsource from the scripts
   dir, take one "zp:" unit from work_units("."), call client().messages.create with
   model=MODEL, system=[{"type":"text","text":system_text("."),"cache_control":{"type":"ephemeral"}}],
   output_config={"effort":"medium","format":{"type":"json_schema","schema":SCHEMA}}, and the unit
   as the user message. Show me its summary and the first 3 findings. Findings must cite message ids
   or timestamps; if they're vague, fix context.md and pilot again.
5. `outsource.py submit --work . --budget <N> --yes`, then in the background
   `outsource.py collect --work .` (waits for the batch; can take hours), then
   `outsource.py rollup --work . --out ~/.claude/projects/<project slug>/audit/<YYYY-MM-DD>-az-digest`
   (OUTSOURCE_KEY_PATH set for each).
6. Report: the actual cost (collect + rollup print it), failed units if any, the paths of digest.md
   and findings.md, and only the "##"/"###" headings of digest.md. Then stop: running /az-curator on
   the digest is a separate step I'll ask for.
```

For reference, a week of a 9-agent team (5.8M input tokens, 415 units, Opus 5.5 at batch prices) cost
about $33.

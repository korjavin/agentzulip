#!/usr/bin/env python3
"""Role-session and developer lens over Claude Code transcripts of an agent-Zulip team.

  transcripts.py <projects-dir> [<projects-dir>...] --since "YYYY-MM-DD HH:MM" [--owner <name>] [--reviewer <role>]

<projects-dir> = ~/.claude/projects/<slug> (one per checkout/worktree family). A role session is a
main transcript that runs `zm -as <role>`; its role is that name. Per role session: wake prompts
("[zm] N new mention(s)" typed by the daemon — not owner input), real owner prompts, inbox calls,
empty inboxes, peer/task notices (subagent and sibling-session messages), max inbox calls in one turn (polling), zm waits, sends mentioning the owner,
compactions, tokens (deduplicated by message id). Per developer subagent: did it request the
reviewer, start the reviewer harness (`codex exec` without `review`), get the launch refused
(worktree guard / auto-mode classifier / hook — by shape), wait, time out, fall back to
`codex review`. Tokens are the usage fields; there is no $ in these transcripts.
"""
import argparse, collections, glob, json, os, re, time

WAKE = re.compile(r"^\[zm\] \d+ new mention")
REFUSAL = [("worktree-guard", r"Refusing to run it|worktree-isolated"), ("auto-mode-classifier", r"auto mode classifier"),
           ("hook", r"PreToolUse:\w+ hook error"), ("harness-blocked", r"<tool_use_error>Blocked:")]


def iter_msgs(path):
    for line in open(path, errors="replace"):
        try:
            yield json.loads(line)
        except ValueError:
            continue


def usage(o, seen, tok):
    u = o["message"].get("usage"); mid = o["message"].get("id")
    if u and mid not in seen:
        seen.add(mid)
        tok["in"] += u.get("input_tokens", 0); tok["cache_read"] += u.get("cache_read_input_tokens", 0)
        tok["cache_write"] += u.get("cache_creation_input_tokens", 0); tok["out"] += u.get("output_tokens", 0); tok["turns"] += 1


def user_text(o):
    c = o["message"].get("content")
    if isinstance(c, str):
        return c, []
    return " ".join(x.get("text", "") for x in c if x.get("type") == "text"), [x for x in c if x.get("type") == "tool_result"]


def result_text(r):
    c = r.get("content")
    return c if isinstance(c, str) else " ".join(x.get("text", "") for x in c or [] if isinstance(x, dict))


def scan_main(path, owner):
    s = dict(role=None, wakes=0, owner_prompts=0, peer_notices=0, inbox=0, empty_inbox=0, max_inbox_turn=0, waits=0, sends_owner=0,
             compactions=0, spawns=0, first=None, last=None)
    tok, seen, cur_inbox = collections.Counter(), set(), 0
    for o in iter_msgs(path):
        ts = o.get("timestamp")
        if ts:
            s["first"] = s["first"] or ts; s["last"] = ts
        if o.get("isCompactSummary"):
            s["compactions"] += 1
        if o.get("type") == "user":
            text, results = user_text(o)
            if isinstance(o["message"].get("content"), str) or not results:
                s["max_inbox_turn"] = max(s["max_inbox_turn"], cur_inbox); cur_inbox = 0
                if WAKE.match(text): s["wakes"] += 1
                elif text.startswith("<") or text.startswith("Another Claude session sent"): s["peer_notices"] += 1
                elif text and "Base directory for this skill" not in text and not text.startswith("[Image") \
                        and not text.startswith("This session is being continued"): s["owner_prompts"] += 1
            for r in results:
                if "(no unread mentions)" in result_text(r): s["empty_inbox"] += 1
        elif o.get("type") == "assistant":
            usage(o, seen, tok)
            for c in o["message"].get("content", []):
                if c.get("type") != "tool_use":
                    continue
                if c.get("name") == "Agent": s["spawns"] += 1
                cmd = c.get("input", {}).get("command", "") if c.get("name") == "Bash" else ""
                m = re.search(r"zm -as (\w+) (\w+)", cmd)
                if not m:
                    continue
                s["role"] = s["role"] or m.group(1)
                if m.group(2) == "inbox": s["inbox"] += 1; cur_inbox += 1
                if m.group(2) == "wait": s["waits"] += 1
                if m.group(2) == "send" and owner and re.search(r"-to\s+\S*" + re.escape(owner), cmd): s["sends_owner"] += 1
    s["tokens"] = dict(tok)
    return s


def scan_sub(path, reviewer):
    d = dict(req=None, launch=None, refusals=collections.Counter(), waits=0, timeouts=0, fallback=False, verdicts=[], tools=0, first=None, last=None)
    kinds, tok, seen = {}, collections.Counter(), set()
    for o in iter_msgs(path):
        ts = o.get("timestamp")
        if ts:
            d["first"] = d["first"] or ts; d["last"] = ts
        if o.get("type") == "assistant":
            usage(o, seen, tok)
            for c in o["message"].get("content", []):
                if c.get("type") != "tool_use":
                    continue
                d["tools"] += 1
                cmd = c.get("input", {}).get("command", "") if c.get("name") == "Bash" else ""
                if reviewer and re.search(r"zm -as \w+ send.*-to\s+\S*" + reviewer, cmd, re.I): d["req"] = d["req"] or ts[11:16]
                if "codex exec" in cmd or "codex review" in cmd:
                    tail = cmd.split("codex", 1)[1][:80]
                    if re.search(r"\breview\b", tail): d["fallback"] = True; kinds[c["id"]] = "review"
                    else: d["launch"] = d["launch"] or ts[11:16]; kinds[c["id"]] = "launch"
                if re.search(r"zm -as \w+ wait", cmd): d["waits"] += 1; kinds[c["id"]] = "wait"
        elif o.get("type") == "user":
            _, results = user_text(o)
            for r in results:
                t = result_text(r); k = kinds.get(r.get("tool_use_id"))
                if k == "launch":
                    for name, rx in REFUSAL:
                        if re.search(rx, t): d["refusals"][name] += 1; break
                if k == "wait" and re.search(r"Exit code 3|did not complete within", t): d["timeouts"] += 1
                for v in re.findall(r"VERDICT: [a-z]+", t): d["verdicts"].append(v)
    d["tokens"] = dict(tok); d["refusals"] = dict(d["refusals"]); d["verdicts"] = sorted(set(d["verdicts"]))
    return d


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("dirs", nargs="+")
    ap.add_argument("--since", required=True)
    ap.add_argument("--owner", help="owner display name, to count sends that mention them")
    ap.add_argument("--reviewer", help="reviewer role, e.g. the -to name developers mention", default=None)
    a = ap.parse_args()
    since = time.mktime(time.strptime(a.since, "%Y-%m-%d %H:%M")) - time.timezone
    for d in a.dirs:
        for path in sorted(glob.glob(os.path.join(d, "*.jsonl"))):
            if os.path.getmtime(path) < since:
                continue
            s = scan_main(path, a.owner)
            if not s["role"]:
                continue
            sid = os.path.basename(path)[:8]
            print(f"\n## {s['role']} {sid} {s['first'][5:16]} -> {s['last'][5:16]} turns={s['tokens'].get('turns', 0)} "
                  f"cache_read={s['tokens'].get('cache_read', 0):,} out={s['tokens'].get('out', 0):,} compactions={s['compactions']}")
            print(f"  wakes={s['wakes']} owner_prompts={s['owner_prompts']} peer/task_notices={s['peer_notices']} inbox={s['inbox']} empty_inbox={s['empty_inbox']} "
                  f"max_inbox_in_one_turn={s['max_inbox_turn']} waits={s['waits']} sends_to_owner={s['sends_owner']} spawns={s['spawns']}")
            subs = sorted(glob.glob(os.path.join(path[:-6], "subagents", "agent-*.jsonl")))
            if subs:
                print("  developers (reviewer lens): desc | req | launch | refusals | waits/timeouts | fallback | verdicts | tools | cache_read")
            for sp in subs:
                try:
                    desc = json.load(open(sp[:-6] + ".meta.json")).get("description", "")
                except (OSError, ValueError):
                    desc = ""
                if not re.match(r"(develop|deliver|fix)", desc, re.I):
                    continue
                x = scan_sub(sp, a.reviewer)
                print(f"  - {desc[:36]:36} | {x['req'] or '-':5} | {x['launch'] or '-':5} | {x['refusals'] or '-'} | {x['waits']}/{x['timeouts']} | "
                      f"{'codex-review' if x['fallback'] else '-'} | {','.join(x['verdicts']) or '-'} | {x['tools']} | {x['tokens'].get('cache_read', 0):,}")


if __name__ == "__main__":
    main()

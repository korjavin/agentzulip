#!/usr/bin/env python3
"""Codex rollouts of role agents (reviewer, sparring partner), found by role text, not cwd.

  codex_rollouts.py [--sessions ~/.codex/sessions] --since "YYYY-MM-DD HH:MM" [--role <reviewer-role> --role <sparring-role>] [--cwd-substr <text>]

A rollout belongs to a role when it runs `zm -as <role>`; `codex review` runs started by
developers have no role and are counted separately, as are the approval-judge rollouts
("You are judging one planned coding-agent action"). Per role rollout: start/end, duration,
total tokens (last token_count), zm waits/sends/inbox, approval-block phrases, last VERDICT, cwd tail.
"""
import argparse, glob, json, os, re, time


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--sessions", default=os.path.expanduser("~/.codex/sessions"))
    ap.add_argument("--since", required=True)
    ap.add_argument("--role", action="append", help="only these roles (default: any zm role)")
    ap.add_argument("--cwd-substr", help="only rollouts whose cwd contains this")
    a = ap.parse_args()
    since = time.mktime(time.strptime(a.since, "%Y-%m-%d %H:%M")) - time.timezone
    reviews = judges = 0
    rows = []
    for f in sorted(glob.glob(os.path.join(a.sessions, "*", "*", "*", "*.jsonl"))):
        if os.path.getmtime(f) < since:
            continue
        head = open(f, errors="replace").read(20000)
        cwd = (re.search(r'"cwd":"([^"]*)"', head) or [None, ""])[1]
        if a.cwd_substr and a.cwd_substr not in cwd:
            continue
        if "You are judging one planned coding-agent action" in head:
            judges += 1; continue
        text = open(f, errors="replace").read()
        m = re.search(r"zm -as (\w+)", text)
        if not m:
            reviews += 1; continue
        role = m.group(1)
        if a.role and role not in a.role:
            continue
        first = last = None; tok = None
        for line in open(f, errors="replace"):
            try:
                o = json.loads(line)
            except ValueError:
                continue
            ts = o.get("timestamp")
            if ts:
                first = first or ts; last = ts
            p = o.get("payload", {})
            if o.get("type") == "event_msg" and p.get("type") == "token_count" and p.get("info"):
                tok = p["info"].get("total_token_usage", tok)
        dur = int((time.mktime(time.strptime(last[:19], "%Y-%m-%dT%H:%M:%S")) - time.mktime(time.strptime(first[:19], "%Y-%m-%dT%H:%M:%S"))) // 60) if first and last else "?"
        cmds = lambda verb: len(re.findall(r"zm -as %s %s\b" % (role, verb), text))
        verdict = (re.findall(r"VERDICT: [a-z ]+", text) or ["-"])[-1].strip()
        blocked = sum(len(re.findall(r"automatic approval (review|block)|May I post|requires your approval following", ln))
                      for ln in text.splitlines() if '"role":"assistant"' in ln)  # the agent's own words, not skill text it read
        rows.append((first[5:16] if first else "?", role, dur, (tok or {}).get("input_tokens", 0), (tok or {}).get("output_tokens", 0),
                     cmds("wait"), cmds("send"), cmds("inbox"), blocked, verdict, cwd[-40:], os.path.basename(f)[8:27]))
    print("start | role | min | in_tokens | out_tokens | waits | sends | inbox | approval-block phrases | last verdict | cwd tail | rollout")
    for r in sorted(rows):
        print(" | ".join(str(x) if not isinstance(x, int) else f"{x:,}" for x in r))
    print(f"\nrole rollouts={len(rows)}; headless `codex review`-style rollouts (no zm role)={reviews}; approval-judge rollouts={judges}")


if __name__ == "__main__":
    main()

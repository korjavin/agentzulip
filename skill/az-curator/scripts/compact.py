#!/usr/bin/env python3
"""Compact Claude Code transcripts of an agent-Zulip team into small text units for outsourced analysis.

  compact.py <out-dir> --since YYYY-MM-DD <projects-dir> [<projects-dir>...]

Keeps who said what and what was run: user/assistant text, tool calls (truncated), tool results
(head only), errors and refusals in full-ish. Drops thinking, images, file dumps. One unit file per
transcript (main or subagent), split at ~CHUNK chars. Prints a size table (chars ~ 3.5 per token).
A main transcript counts only if it runs `zm -as <role>`; subagents count when their parent does.
"""
import argparse, glob, json, os, re, time

CHUNK = 240_000  # ~70k tokens per unit
CALL, RESULT, ERR = 400, 300, 1200
ROLE = re.compile(r"zm -as ([a-z]+)")


def text_of(c):
    if isinstance(c, str):
        return c
    out = []
    for b in c or []:
        if isinstance(b, dict) and b.get("type") == "text":
            out.append(b["text"])
        elif isinstance(b, dict) and b.get("type") == "tool_result":
            out.append(text_of(b.get("content")))
    return "\n".join(out)


def compact(path):
    lines, roles = [], {}
    for raw in open(path, errors="replace"):
        try:
            o = json.loads(raw)
        except ValueError:
            continue
        m, ts = o.get("message") or {}, (o.get("timestamp") or "")[:16].replace("T", " ")
        if o.get("type") == "user":
            c = m.get("content")
            if isinstance(c, list) and any(isinstance(b, dict) and b.get("type") == "tool_result" for b in c):
                for b in c:
                    if isinstance(b, dict) and b.get("type") == "tool_result":
                        t = text_of(b.get("content")).strip()
                        lim = ERR if b.get("is_error") or "denied" in t[:300] else RESULT
                        lines.append(f"  -> {'ERROR ' if b.get('is_error') else ''}{t[:lim]}")
            else:
                t = text_of(c).strip()
                if t:
                    lines.append(f"[{ts}] USER: {t[:3000]}")
        elif o.get("type") == "assistant":
            for b in m.get("content") or []:
                if b.get("type") == "text" and b["text"].strip():
                    lines.append(f"[{ts}] AGENT: {b['text'].strip()[:3000]}")
                elif b.get("type") == "tool_use":
                    inp = b.get("input") or {}
                    arg = inp.get("command") or inp.get("description") or inp.get("prompt") or json.dumps(inp)[:CALL]
                    for r in ROLE.findall(str(arg)):
                        roles[r] = roles.get(r, 0) + 1
                    lines.append(f"[{ts}] CALL {b.get('name')}: {str(arg)[:CALL]}")
        elif o.get("type") == "system" and "compact" in str(o.get("subtype", "")):
            lines.append(f"[{ts}] (context compacted)")
    return "\n".join(lines), roles


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("out")
    ap.add_argument("dirs", nargs="+")
    ap.add_argument("--since", required=True)
    a = ap.parse_args()
    since = time.mktime(time.strptime(a.since, "%Y-%m-%d"))
    os.makedirs(a.out, exist_ok=True)
    total, n = 0, 0
    for d in a.dirs:
        for main_t in glob.glob(os.path.join(d, "*.jsonl")):
            if os.path.getmtime(main_t) < since:
                continue
            body, roles = compact(main_t)
            if not roles:
                continue  # not a role agent's session (the owner's own sessions)
            role = max(roles, key=roles.get)
            sid = os.path.basename(main_t)[:-6]
            units = [(f"{role}.{sid[:8]}", body)]
            for sub in sorted(glob.glob(os.path.join(d, sid, "subagents", "*.jsonl"))):
                if os.path.getmtime(sub) >= since:
                    units.append((f"{role}.{sid[:8]}.sub-{os.path.basename(sub)[:-6][-8:]}", compact(sub)[0]))
            for name, text in units:
                for i in range(0, max(len(text), 1), CHUNK):
                    part = text[i:i + CHUNK]
                    if len(part) < 2000:
                        continue
                    fn = f"{name}.{i // CHUNK}.txt"
                    open(os.path.join(a.out, fn), "w").write(part)
                    total += len(part)
                    n += 1
    print(f"{n} units, {total / 1e6:.1f}M chars, ~{total / 3.5 / 1e6:.1f}M tokens")


if __name__ == "__main__":
    main()

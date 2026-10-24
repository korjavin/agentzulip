#!/usr/bin/env python3
"""Wake-up lens over the zm daemon's journal (systemd --user unit) or a saved log file.

  daemon_journal.py [--unit zm-porter] [--since "2026-01-01 00:00"] [--file saved.log] [--split "YYYY-MM-DD HH:MM"]

Prints wakes per role (optionally before/after --split, e.g. a restart of the team), wakes per
hour, storm minutes (>=5 wakes), daemon (re)starts, restart requests, joins/registers and
anything else (errors). Role names come from the log itself.
"""
import argparse, collections, re, subprocess

WOKE = re.compile(r"(\d{4}-\d\d-\d\dT\d\d:\d\d).*woke (\S+) \((\w+)\): (\d+)")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--unit", default="zm-porter")
    ap.add_argument("--since")
    ap.add_argument("--file")
    ap.add_argument("--split", help="UTC 'YYYY-MM-DD HH:MM' -> counts before/after")
    a = ap.parse_args()
    if a.file:
        lines = open(a.file).read().splitlines()
    else:
        cmd = ["journalctl", "--user", "-u", a.unit, "--no-pager", "-o", "short-iso"] + (["--since", a.since] if a.since else [])
        lines = subprocess.run(cmd, capture_output=True, text=True).stdout.splitlines()
    split = a.split.replace(" ", "T") if a.split else None
    per_role, per_hour, per_min, batched, other = collections.Counter(), collections.Counter(), collections.Counter(), 0, collections.Counter()
    starts, restarts = [], []
    for ln in lines:
        m = WOKE.search(ln)
        if m:
            ts, sess, role, n = m.groups()
            k = role if not split else f"{role} {'after' if ts >= split else 'before'}"
            per_role[k] += 1; per_hour[ts[:13]] += 1; per_min[ts] += 1; batched += int(n) > 1
            continue
        body = re.sub(r"^\S+ \S+ \S+\[\d+\]: (\d{4}/\d\d/\d\d \d\d:\d\d:\d\d )?", "", ln)
        ts = ln[:16]
        if "zm daemon:" in body: starts.append(ts)
        elif body.startswith("restarting"): restarts.append(f"{ts} {body}")
        elif re.match(r"(registered|joined|registry:)", body): other["registered/joined"] += 1
        elif re.match(r"(Started|Stopped|Stopping|Starting)", body): pass
        else: other[re.sub(r"\d+", "N", body)[:110]] += 1
    print(f"== {sum(per_role.values())} wakes; batched (>1 mention in one wake): {batched}")
    for k, n in sorted(per_role.items()): print(f"  {k:28} {n}")
    print("\n== wakes per hour"); [print(f"  {h} {n}") for h, n in sorted(per_hour.items())]
    storms = [(t, n) for t, n in per_min.items() if n >= 5]
    print(f"\n== storm minutes (>=5 wakes): {len(storms)}"); [print(f"  {t} {n}") for t, n in sorted(storms)]
    print(f"\n== daemon starts: {len(starts)} at {', '.join(s[11:16] for s in starts)}")
    print(f"== restart requests honoured: {len(restarts)}"); [print("  " + r) for r in restarts]
    print("\n== other lines"); [print(f"  {n:4} {k}") for k, n in other.most_common(15)]


if __name__ == "__main__":
    main()

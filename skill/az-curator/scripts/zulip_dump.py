#!/usr/bin/env python3
"""Read-only dump of Zulip channels to JSON (stdlib only).

  zulip_dump.py --rc ~/.config/zm/<role>.zuliprc --channel <ch> [--channel <ch>-work] \
      --since "2026-01-01 00:00" --out dump.json

The rc is a zuliprc as zm reads it: [api] email, site, and key or key_cmd (a command that prints
the key). Only GET /messages is called: nothing is sent, reacted to or marked read. The key is
never printed. Timestamps are UTC epoch seconds.
"""
import argparse, base64, json, subprocess, sys, time, urllib.parse, urllib.request


def load_rc(path):
    rc, section = {}, ""
    for line in open(path):
        line = line.strip()
        if not line or line[0] in "#;":
            continue
        if line.startswith("[") and line.endswith("]"):
            section = line[1:-1]
            continue
        k, _, v = line.partition("=")
        rc[section + "." + k.strip()] = v.strip()
    key = rc.get("api.key") or subprocess.check_output(rc["api.key_cmd"], shell=True, text=True).strip()
    return rc["api.email"], key, rc["api.site"].rstrip("/")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--rc", required=True)
    ap.add_argument("--channel", action="append", required=True)
    ap.add_argument("--since", required=True, help="UTC 'YYYY-MM-DD HH:MM'")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    since = int(time.mktime(time.strptime(a.since, "%Y-%m-%d %H:%M")) - time.timezone)
    email, key, site = load_rc(a.rc)
    auth = base64.b64encode(f"{email}:{key}".encode()).decode()

    def get(**params):
        url = f"{site}/api/v1/messages?" + urllib.parse.urlencode(params)
        req = urllib.request.Request(url, headers={"Authorization": "Basic " + auth})
        return json.load(urllib.request.urlopen(req))

    out = {}
    for ch in a.channel:
        anchor = "newest"
        while True:
            r = get(anchor=anchor, num_before=1000, num_after=0, apply_markdown="false",
                    narrow=json.dumps([{"operator": "stream", "operand": ch}]))
            msgs = r["messages"]
            for m in msgs:
                if m["timestamp"] >= since:
                    out[m["id"]] = {"id": m["id"], "ts": m["timestamp"], "channel": ch, "topic": m["subject"],
                                    "sender": m["sender_full_name"], "sender_email": m["sender_email"],
                                    "text": m["content"],
                                    "reacted_by": sorted({(r.get("user") or {}).get("full_name") or str(r.get("user_id")) for r in m.get("reactions", [])})}
            if r["found_oldest"] or not msgs or msgs[0]["timestamp"] < since:
                break
            anchor = msgs[0]["id"] - 1
    rows = sorted(out.values(), key=lambda m: m["id"])
    json.dump(rows, open(a.out, "w"))
    if rows:
        f = lambda m: time.strftime("%Y-%m-%d %H:%M", time.gmtime(m["ts"]))
        print(f"{len(rows)} messages {f(rows[0])} -> {f(rows[-1])} UTC -> {a.out}")
    else:
        print("0 messages", file=sys.stderr)


if __name__ == "__main__":
    main()

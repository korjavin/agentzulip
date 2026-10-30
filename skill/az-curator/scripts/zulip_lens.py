#!/usr/bin/env python3
"""Lenses over a zulip_dump.py file: owner signal, reviewer engagement, stalls, fan-out.

  zulip_lens.py dump.json --owner "<owner display name>" [--reviewer <name>] [--daemon <name>]
                [--since "YYYY-MM-DD HH:MM"] [--owner-channel <id|name>] [--reply-minutes 30]

Display names are the Zulip names of the human owner, the short-lived reviewer role and the
daemon bot. Everything else is treated as a role agent. Output is plain text sections.
"""
import argparse, collections, json, re, time

T = lambda ts: time.strftime("%m-%d %H:%M", time.gmtime(ts))
flat = lambda s, n=220: re.sub(r"\s+", " ", s)[:n]
MENTION = re.compile(r"@\*\*([^*|]+)(?:\|\d+)?\*\*")  # @**Name** or @**Name|id**; @_** is silent


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("dump")
    ap.add_argument("--owner", required=True)
    ap.add_argument("--reviewer")
    ap.add_argument("--daemon")
    ap.add_argument("--since")
    ap.add_argument("--owner-channel", help="the owner's channel id (preferred) or name (default: the one not ending in -work)")
    ap.add_argument("--reply-minutes", type=int, default=30, help="a mention with no reply post and no reaction by the mentioned agent after this long is a stall")
    ap.add_argument("--fallback-regex", default=r"fall(ing)? back|fallback|refused|no reply from|usage limit")
    a = ap.parse_args()
    ms = json.load(open(a.dump))
    if a.since:
        since = int(time.mktime(time.strptime(a.since, "%Y-%m-%d %H:%M")) - time.timezone)
        ms = [m for m in ms if m["ts"] >= since]
    system = {"Notification Bot"}
    agents = {m["sender"] for m in ms} - {a.owner, a.daemon} - system
    oc = a.owner_channel
    own_ch = next((m["channel"] for m in ms if oc in (m["channel"], str(m.get("channel_id")))), oc) if oc else next((m["channel"] for m in ms if not m["channel"].endswith("-work")), None)
    by_topic = collections.defaultdict(list)
    for m in ms:
        by_topic[(m["channel"], m["topic"].replace("✔ ", ""))].append(m)
    for L in by_topic.values():
        L.sort(key=lambda m: m["ts"])

    print(f"== {len(ms)} messages; by channel {dict(collections.Counter(m['channel'] for m in ms))}")
    print("by sender:", collections.Counter(m["sender"] for m in ms).most_common())
    print("mentions per hour:", sorted(collections.Counter(time.strftime("%d %Hh", time.gmtime(m["ts"]))
                                                           for m in ms if MENTION.search(m["text"]) and m["sender"] not in system).items()))

    print(f"\n== owner ({a.owner}) messages")
    own = [m for m in ms if m["sender"] == a.owner]
    for m in own:
        print(T(m["ts"]), m["channel"], "|", m["topic"][:40], "|", flat(m["text"], 300))
    print("\n== owner re-posts (same topic, <5 min apart, same first 20 chars after stripping mentions)")
    strip = lambda t: MENTION.sub("", t).strip()[:20]
    for x, y in zip(own, own[1:]):
        if y["ts"] - x["ts"] < 300 and x["topic"] == y["topic"] and strip(x["text"]) == strip(y["text"]):
            print(T(x["ts"]), "->", T(y["ts"]), x["topic"][:40], "|", flat(x["text"], 80))

    print(f"\n== agent -> owner mentions (rule: only when blocked or needing a decision)")
    print(collections.Counter(m["sender"] for m in ms if a.owner in MENTION.findall(m["text"]) and m["sender"] != a.owner))

    print(f"\n== owner-started topics in the owner channel: agent posts (noise lens)")
    for (ch, t), L in by_topic.items():
        if ch != own_ch or L[0]["sender"] != a.owner:
            continue
        ag = [m for m in L if m["sender"] in agents]
        print(f"{t[:45]:45} owner={sum(m['sender'] == a.owner for m in L):2} agent={len(ag):2} >1500ch={sum(len(m['text']) > 1500 for m in ag)} by={dict(collections.Counter(m['sender'] for m in ag))}")

    if a.daemon:
        print(f"\n== daemon ({a.daemon}) broadcasts: mentions per message = wake fan-out")
        for m in ms:
            if m["sender"] == a.daemon:
                print(T(m["ts"]), m["topic"][:30], "mentions=%d" % len(MENTION.findall(m["text"])), "|", flat(m["text"], 120))

    print(f"\n== stalls: @-mentions of a role agent with no reaction and no post by it in that topic within {a.reply_minutes} min")
    stalls = collections.Counter(); examples = collections.defaultdict(list)
    for key, L in by_topic.items():
        for i, m in enumerate(L):
            if m["sender"] == a.daemon or re.search(r"no reply (is )?(needed|requested)", m["text"], re.I):
                continue  # broadcasts and FYIs are not asks
            for who in set(MENTION.findall(m["text"])):
                if who not in agents or who == m["sender"]:
                    continue
                answered = who in m.get("reacted_by", []) or \
                    any(n["sender"] == who and n["ts"] - m["ts"] <= a.reply_minutes * 60 for n in L[i + 1:])
                if not answered:
                    stalls[who] += 1; examples[who].append(f"{T(m['ts'])} {key[1][:30]} <- {m['sender']}")
    for who, n in stalls.most_common():
        print(f"{who}: {n} unanswered; e.g. " + "; ".join(examples[who][:3]))

    if a.reviewer:
        R = a.reviewer
        print(f"\n== reviewer ({R}) per topic: requests, first-reply latency, posts, verdicts, fallback talk")
        for (ch, t), L in by_topic.items():
            req = [m for m in L if R in MENTION.findall(m["text"]) and m["sender"] != R]
            posts = [m for m in L if m["sender"] == R]
            if not req and not posts:
                continue
            lat = f"{(posts[0]['ts'] - req[0]['ts']) // 60}m" if req and posts else "-"
            verdicts = [re.search(r"VERDICT:[^\n]*", m["text"]).group(0)[:50] for m in posts if "VERDICT" in m["text"]]
            fb = [m for m in L if m["sender"] != R and re.search(a.fallback_regex, m["text"], re.I)]
            print(f"{t[:40]:40} req={len(req)} ({T(req[0]['ts']) if req else '-'}) posts={len(posts)} first_reply={lat} verdicts={verdicts} fallback_msgs={len(fb)}")
            for m in fb[:1]:
                print("    FB", T(m["ts"]), m["sender"], flat(m["text"], 160))
            for m in posts[:1]:
                print("    RV", T(m["ts"]), flat(m["text"], 160))


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Outsource the reading of an agent-Zulip team's history to the Claude API, for az-curator.

  outsource.py estimate --work DIR            # units -> token/cost estimate, nothing sent
  outsource.py submit   --work DIR --budget 100 [--yes]   # one Message Batch, one request per unit
  outsource.py collect  --work DIR            # wait for the batch, write findings.jsonl + cost
  outsource.py rollup   --work DIR --out AUDIT_DIR       # one call: findings -> digest.md

DIR holds: units/*.txt (compact.py), zulip.json (zulip_dump.py), journal.txt (journalctl of the
daemon), context.md (the flow's rules: zulip-agents skill, role list). Each unit (a transcript
chunk, a Zulip topic or a pack of small ones, a journal chunk) is read by the model with the same
cached system prompt and returns JSON findings with evidence pointers. The rollup ranks them.
The API key comes from stash (`kv get $OUTSOURCE_KEY_PATH`) and is never printed. Needs the `anthropic` package.
"""
import argparse, collections, glob, json, os, re, subprocess, sys, time

KEY = os.environ.get("OUTSOURCE_KEY_PATH", "")  # stash path of the API key, e.g. secrets/<team>/anthropic
MODEL = "claude-opus-5-5"
PRICE_IN, PRICE_OUT, PRICE_CACHE = 2.0, 10.0, 0.10  # $/MTok, batch = half the list price
OUT_PER_UNIT = 5000  # assumed average output incl. thinking, for the estimate
CHUNK = 240_000
CATS = ["mistake", "inefficiency", "ping_pong", "stall", "owner_friction", "rule_violation",
        "harness_refusal", "context_waste", "review_quality", "tooling_error", "good_practice"]
TARGETS = ["zulip-agents skill", "role card", "develop/orchestrate skill", "zm/daemon",
           "launcher/scripts", "harness config", "owner habit", "project code/process", "none"]

SCHEMA = {
    "type": "object", "additionalProperties": False, "required": ["summary", "findings"],
    "properties": {
        "summary": {"type": "string"},
        "findings": {"type": "array", "items": {
            "type": "object", "additionalProperties": False,
            "required": ["category", "severity", "culprit", "title", "evidence", "cost", "root_cause", "fix", "fix_target"],
            "properties": {
                "category": {"type": "string", "enum": CATS},
                "severity": {"type": "string", "enum": ["high", "medium", "low"]},
                "culprit": {"type": "string"},
                "title": {"type": "string"},
                "evidence": {"type": "string"},
                "cost": {"type": "string"},
                "root_cause": {"type": "string"},
                "fix": {"type": "string"},
                "fix_target": {"type": "string", "enum": TARGETS},
            }}},
    }}

SYSTEM = """You audit how a team of long-lived AI coding agents worked, so a later curator can fix
the process (skills, role cards, the zm CLI and its daemon, launch scripts). You get one unit of
history at a time: a chunk of one agent's Claude Code transcript, a Zulip topic (or a pack of small
topics), or the daemon's journal. Report only what this unit shows.

How the team works (the rules the agents were given are below the line):
- Each role agent (Claude Code, Codex or Gemini) runs in its own terminal session and talks ONLY
  through Zulip with the `zm` CLI. A daemon bot wakes a session by typing
  "[zm] N new mention(s) for <role>. Run: zm -as <role> inbox" into it; that line is NOT the human.
  Every wake is a full-context turn of that agent: it costs real tokens.
- Two channels: the owner's (questions/decisions for the human owner, short summaries) and
  "<channel>-work" (everything else). Topics are tasks: "<short title> <bead-id>".
- Orchestrators spawn developer subagents (develop skill); a developer opens a PR, posts the review
  request in the bead's topic, starts a fresh Codex reviewer itself and waits for its
  VERDICT. Beads (bd) are the issue tracker. The human owner answers in Zulip. Names of roles and the owner are in the team notes below.

What to look for, each with concrete evidence:
- mistake: wrong action or claim, bug shipped, a review that missed something, wrong channel/topic.
  Name the culprit role (or "process", "harness", "skill", "owner").
- inefficiency: rework, long detours, redundant checks, repeated reading of the same thing.
- ping_pong: mentions that carry nothing to act on, acknowledgements that wake someone, two agents
  bouncing a question, broadcasts with many mentions, agents re-asking what's in the topic.
- stall: an agent waiting on nobody, a mention never answered, a wake that ended in "no unread
  mentions", an inbox polled in a loop, a stuck harness.
- owner_friction: the owner asked to do mechanical steps (paste, approve, run), pinged without a
  decision, had to repeat or escalate, noisy owner's topics.
- rule_violation: against the rules below or the agent's role card.
- harness_refusal: guard/classifier/hook refusals, sandbox approval blocks; give the command shape.
- context_waste: huge contexts carried into wakes, compactions, reading whole files/logs needlessly.
- review_quality: reviews that approved fast and were wrong, endless rounds, missing verdicts.
- tooling_error: zm/daemon/scripts/CI misbehaving.
- good_practice: something that worked well and should be kept (sparingly).
Severity: high = wasted much (hours, a big token burn, a shipped bug, the owner blocked); low = minor.
"evidence": message ids, timestamps, roles, a short quote (<200 chars). "cost": your estimate of
what it wasted (turns, tokens, minutes) or "unknown". "fix": the smallest process change that
prevents it, and "fix_target" where it goes. No findings is a valid answer; don't pad. Never copy
secrets, keys or tokens into your answer. "summary": 2-4 sentences of what happened in this unit.
----------------------------------------------------------------
"""


# Credentials that can sit in tool calls/results, logs or chat: scrubbed before anything is sent.
SECRET = re.compile(
    r"(?i)((?<![A-Za-z0-9])(?:api[_-]?key|key|token|secret|password|passwd)[\"']?\s*[=:]\s*[\"']?)[^\s\"',}]+"
    r"|(authorization:\s*(?:bearer|basic)\s+|\s-u\s+)\S+"
    r"|\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16})"
    r"|\b[A-Za-z0-9]{32,}\b")  # bare long tokens (a Zulip key is 32 chars)


def redact(text):
    return SECRET.sub(lambda m: (m.group(1) or m.group(2) or "") + "[redacted]", text)


def work_units(work):
    """(custom_id, text) for every unit, secrets scrubbed."""
    return [(cid, redact(text)) for cid, text in raw_units(work)]


def raw_units(work):
    units = []
    for f in sorted(glob.glob(os.path.join(work, "units", "*.txt"))):
        units.append(("t:" + os.path.basename(f)[:-4], "CLAUDE TRANSCRIPT CHUNK of role session " + os.path.basename(f) + "\n\n" + open(f).read()))
    zp = os.path.join(work, "zulip.json")
    if os.path.exists(zp):
        topics = collections.OrderedDict()
        for m in sorted(json.load(open(zp)), key=lambda m: m["ts"]):
            key = (m["channel"], m["topic"].removeprefix("✔ "))
            r = f" [reactions: {', '.join(m['reacted_by'])}]" if m.get("reacted_by") else ""
            t = time.strftime("%m-%d %H:%M", time.gmtime(m["ts"]))
            topics.setdefault(key, []).append(f"#{m['id']} [{t}] {m['sender']}{r}: {m['text']}")
        pack, packed = [], 0
        for i, ((ch, tp), lines) in enumerate(topics.items()):
            body = f"ZULIP #{ch} > {tp}\n" + "\n".join(lines)
            if len(body) > 60_000:
                for j in range(0, len(body), CHUNK):
                    units.append((f"z:{i}.{j // CHUNK}", body[j:j + CHUNK]))
                continue
            pack.append(body)
            packed += len(body)
            if packed > 60_000:
                units.append((f"zp:{i}", "\n\n=====\n\n".join(pack)))
                pack, packed = [], 0
        if pack:
            units.append(("zp:last", "\n\n=====\n\n".join(pack)))
    jp = os.path.join(work, "journal.txt")
    if os.path.exists(jp):
        j = open(jp).read()
        for k in range(0, len(j), CHUNK):
            units.append((f"j:{k // CHUNK}", "DAEMON JOURNAL\n" + j[k:k + CHUNK]))
    return units


def system_text(work):
    return SYSTEM + open(os.path.join(work, "context.md")).read()


def estimate(work):
    units = work_units(work)
    sys_tok = len(system_text(work)) / 3.5
    in_tok = sum(len(t) for _, t in units) / 3.5
    cost = (in_tok * PRICE_IN + sys_tok * PRICE_IN + sys_tok * len(units) * PRICE_CACHE
            + OUT_PER_UNIT * len(units) * PRICE_OUT) / 1e6
    kinds = collections.Counter(c.split(":")[0] for c, _ in units)
    print(f"{len(units)} units {dict(kinds)}, ~{in_tok / 1e6:.1f}M input tokens + {sys_tok / 1e3:.0f}k cached system; "
          f"estimated ${cost:.0f} ({MODEL}, batch prices, {OUT_PER_UNIT} output tokens/unit assumed)")
    return units, cost


def client():
    import anthropic
    if not KEY:
        sys.exit("set OUTSOURCE_KEY_PATH to the stash path of the API key")
    key = subprocess.run(["kv", "get", KEY], capture_output=True, text=True, check=True).stdout.strip()
    return anthropic.Anthropic(api_key=key)


def submit(a):
    units, cost = estimate(a.work)
    if cost > a.budget:
        sys.exit(f"estimate ${cost:.0f} > budget ${a.budget}: not sent")
    if not a.yes:
        sys.exit("estimate only; rerun with --yes to send")
    st = os.path.join(a.work, "batch.json")
    if os.path.exists(st):
        sys.exit(f"{st} exists: a batch was already sent")
    system = [{"type": "text", "text": system_text(a.work), "cache_control": {"type": "ephemeral"}}]
    reqs = [{"custom_id": cid.replace(":", "_").replace(".", "-")[:64], "params": {
        "model": MODEL, "max_tokens": 16000, "system": system,
        "output_config": {"effort": "medium", "format": {"type": "json_schema", "schema": SCHEMA}},
        "messages": [{"role": "user", "content": text}]}} for cid, text in units]
    b = client().messages.batches.create(requests=reqs)
    json.dump({"id": b.id, "units": {r["custom_id"]: cid for r, (cid, _) in zip(reqs, units)}}, open(st, "w"))
    print("batch", b.id, b.processing_status, len(reqs), "requests")


def collect(a):
    st = json.load(open(os.path.join(a.work, "batch.json")))
    c = client()
    while True:
        b = c.messages.batches.retrieve(st["id"])
        if b.processing_status == "ended":
            break
        print(time.strftime("%H:%M"), b.processing_status, b.request_counts, flush=True)
        time.sleep(120)
    tin = tout = tcache = 0
    bad = []
    with open(os.path.join(a.work, "findings.jsonl"), "w") as out:
        for r in c.messages.batches.results(st["id"]):
            unit = st["units"].get(r.custom_id, r.custom_id)
            if r.result.type != "succeeded":
                bad.append(f"{unit}: {r.result.type}")
                continue
            m = r.result.message
            u = m.usage
            tin += u.input_tokens + (u.cache_creation_input_tokens or 0)
            tcache += u.cache_read_input_tokens or 0
            tout += u.output_tokens
            text = next((x.text for x in m.content if x.type == "text"), "")
            try:
                d = json.loads(text)
            except ValueError:
                bad.append(f"{unit}: unparsable ({m.stop_reason})")
                continue
            d["unit"] = unit
            out.write(json.dumps(d, ensure_ascii=False) + "\n")
    cost = (tin * PRICE_IN + tcache * PRICE_CACHE + tout * PRICE_OUT) / 1e6
    print(f"done: {b.request_counts}; tokens in {tin} cached {tcache} out {tout}; cost ${cost:.2f}")
    for x in bad:
        print("FAILED", x)


def rollup(a):
    rows = [json.loads(l) for l in open(os.path.join(a.work, "findings.jsonl"))]
    lines = []
    for r in rows:
        lines.append(f"## {r['unit']}: {r['summary']}")
        for f in r["findings"]:
            lines.append(f"- [{f['category']}/{f['severity']}] {f['culprit']}: {f['title']} | evidence: {f['evidence']} "
                         f"| cost: {f['cost']} | cause: {f['root_cause']} | fix ({f['fix_target']}): {f['fix']}")
    body = "\n".join(lines)
    prompt = ("Below are per-unit audit findings about an AI agent team (units: t = a role agent's "
              "transcript chunk, z/zp = Zulip topics, j = the wake daemon's journal). Write the digest a "
              "process curator will read instead of the raw history, in Markdown:\n"
              "1. Top problems ranked by total waste: for each, what happens, how often (count units), "
              "which roles, 2-4 evidence pointers copied from the findings, root cause, and the smallest "
              "fix with its target (skill / role card / zm / launcher / owner habit).\n"
              "2. Per role: its recurring mistakes and inefficiencies, in a few lines each.\n"
              "3. Ping-pong and wake waste: patterns, with numbers.\n"
              "4. Owner friction: what the owner had to do that agents should have.\n"
              "5. What works and must be kept.\n"
              "6. Open questions the findings can't settle.\n"
              "Merge duplicates; drop one-off trivia; never invent evidence.\n\n" + body)
    c = client()
    with c.messages.stream(model=MODEL, max_tokens=64000, output_config={"effort": "high"},
                           messages=[{"role": "user", "content": prompt}]) as s:
        m = s.get_final_message()
    text = next((x.text for x in m.content if x.type == "text"), "")
    os.makedirs(a.out, exist_ok=True)
    open(os.path.join(a.out, "digest.md"), "w").write(text)
    open(os.path.join(a.out, "findings.md"), "w").write(body)
    u = m.usage
    print(f"digest -> {a.out}/digest.md; findings -> {a.out}/findings.md; "
          f"cost ${(u.input_tokens * 4 + u.output_tokens * 20) / 1e6:.2f}")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("cmd", choices=["estimate", "submit", "collect", "rollup"])
    ap.add_argument("--work", required=True)
    ap.add_argument("--budget", type=float, default=100)
    ap.add_argument("--yes", action="store_true")
    ap.add_argument("--out")
    a = ap.parse_args()
    {"estimate": lambda: estimate(a.work), "submit": lambda: submit(a),
     "collect": lambda: collect(a), "rollup": lambda: rollup(a)}[a.cmd]()


if __name__ == "__main__":
    main()

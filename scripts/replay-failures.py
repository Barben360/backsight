#!/usr/bin/env python3
"""replay-failures.py: the replay that decides recall.failure_pitfalls.

Usage: python3 -I scripts/replay-failures.py [--projects DIR] [--limit N] [--json]

Reads Claude Code transcripts (DIR/**/*.jsonl, default ~/.claude/projects),
read only, and lists the failed tool calls (a tool_result with is_error that
answers a Bash, Edit, Write, MultiEdit, WebFetch or MCP tool_use, not
backsight's own tools, not user interruptions). It prints the number of
failures and of distinct signatures: the hook looks up each signature once
per session.

Dry mode (nothing set): counts only, no network.

Live mode: with BACKSIGHT_REPLAY_URL (a Hindsight API, e.g.
http://127.0.0.1:8888) and BACKSIGHT_REPLAY_BANK set, each distinct
(session, signature) is searched like the hook does (observations, budget
low, query = tool, command and error, cut to 300 characters) and the script
prints how many would inject at least one memory at a reranker score of 0.8
or more. BACKSIGHT_REPLAY_KEY is the API key, if the server wants one. Point
it at a scratch server or accept that it only reads: recall is a read.
The query here is not redacted: run it only against a server you own.
"""
import argparse
import glob
import hashlib
import json
import os
import re
import sys
import urllib.request

FLOOR = 0.8
QUERY_CHARS = 300
CMD_CHARS = 100
MIN_ERROR = 15
TOOLS = re.compile(r"^(Bash|Edit|Write|MultiEdit|WebFetch|mcp__.*)$")
OWN = "mcp__plugin_backsight_"

EXIT = re.compile(r"^\s*Exit code \d+\s*")
TRUNC = re.compile(r"\.\.\. \[\d+ characters truncated\] \.\.\.")
PATH = re.compile(r"(?:[A-Za-z]:)?[\\/][^\s:\"'()\[\]]+|\b[\w.-]+/[\w./-]+")
HEX = re.compile(r"\b0x[0-9a-f]+\b|\b[0-9a-f]{7,}\b", re.I)
NUM = re.compile(r"\d+")


def signature(tool, err):
    e = EXIT.sub("", TRUNC.sub(" ", err))
    e = "\n".join(e.split("\n")[:3])
    e = NUM.sub("<n>", HEX.sub("<h>", PATH.sub("<p>", e)))
    e = " ".join(e.split()).lower()
    return hashlib.sha256((tool + "\0" + e[:300]).encode()).hexdigest()[:16]


def text_of(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(b.get("text", "") for b in content if isinstance(b, dict))
    return ""


def command_of(inp):
    if isinstance(inp, dict):
        for k in ("command", "file_path", "url", "path", "query"):
            if isinstance(inp.get(k), str) and inp[k]:
                return inp[k]
    return ""


def failures(path):
    uses, out = {}, []
    try:
        fh = open(path, encoding="utf-8", errors="replace")
    except OSError:
        return out
    with fh:
        for line in fh:
            if '"tool_use"' not in line and '"tool_result"' not in line:
                continue
            try:
                msg = json.loads(line).get("message", {})
            except ValueError:
                continue
            content = msg.get("content")
            if not isinstance(content, list):
                continue
            for b in content:
                if not isinstance(b, dict):
                    continue
                if b.get("type") == "tool_use":
                    uses[b.get("id")] = (b.get("name", ""), command_of(b.get("input")))
                elif b.get("type") == "tool_result" and b.get("is_error"):
                    tool, cmd = uses.get(b.get("tool_use_id"), ("", ""))
                    err = text_of(b.get("content")).strip()
                    body = EXIT.sub("", err).strip()
                    if (not TOOLS.match(tool) or tool.startswith(OWN) or len(body) < MIN_ERROR
                            or body.startswith("The user doesn't want to proceed")
                            or "[Request interrupted by user" in err):
                        continue
                    out.append((tool, cmd, err, body))
    return out


def query(tool, cmd, body):
    cmd = " ".join(cmd.split())[:CMD_CHARS]
    return (tool + " failed: " + cmd + "\n" + body)[:QUERY_CHARS]


def recall(url, bank, key, q):
    req = urllib.request.Request(
        url.rstrip("/") + "/v1/default/banks/" + urllib.request.quote(bank, safe="") + "/memories/recall",
        data=json.dumps({"query": q, "types": ["observation"], "budget": "low", "max_tokens": 1500}).encode(),
        headers={"Content-Type": "application/json", **({"Authorization": "Bearer " + key} if key else {})})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r).get("results", [])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--projects", default=os.path.expanduser("~/.claude/projects"))
    ap.add_argument("--limit", type=int, default=0, help="search at most N distinct failures in live mode")
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()
    files = sorted(glob.glob(os.path.join(a.projects, "**", "*.jsonl"), recursive=True))
    total, seen, by_tool = 0, {}, {}
    for f in files:
        session = os.path.basename(f)
        for tool, cmd, err, body in failures(f):
            total += 1
            by_tool[tool if not tool.startswith("mcp__") else "mcp__*"] = by_tool.get(tool if not tool.startswith("mcp__") else "mcp__*", 0) + 1
            seen.setdefault((session, signature(tool, err)), (tool, cmd, body))
    res = {"transcripts": len(files), "failures": total, "distinct_signatures_per_session": len(seen),
           "distinct_signatures": len({s for _, s in seen}), "by_tool": by_tool}
    url, bank = os.environ.get("BACKSIGHT_REPLAY_URL"), os.environ.get("BACKSIGHT_REPLAY_BANK")
    if url and bank:
        key = os.environ.get("BACKSIGHT_REPLAY_KEY", "")
        todo = list(seen.values())[: a.limit or None]
        injecting = errors = 0
        for tool, cmd, body in todo:
            try:
                hits = [h for h in recall(url, bank, key, query(tool, cmd, body)) if h.get("scores", {}).get("reranker", 0) >= FLOOR]
            except Exception as e:  # noqa: BLE001 - a replay reports and goes on
                errors += 1
                print("search failed:", e, file=sys.stderr)
                continue
            injecting += bool(hits)
        res.update(searched=len(todo), would_inject=injecting, search_errors=errors, floor=FLOOR)
    else:
        res["mode"] = "dry (set BACKSIGHT_REPLAY_URL and BACKSIGHT_REPLAY_BANK to search)"
    if a.json:
        print(json.dumps(res, indent=2))
    else:
        for k, v in res.items():
            print(f"{k}: {v}")


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Write a synthetic chat history for the README screenshot.

Usage: corpus.py <home-dir>

The corpus imitates pi, Claude Code and Codex storage under <home-dir>. All
content is invented, so the screenshot shows no real project or person.
"""

import json
import sys
import uuid
from datetime import date, timedelta
from pathlib import Path

HOME = Path(sys.argv[1]).resolve()


def ago(days: int) -> str:
    """A date relative to today, so "last week" in the demo stays true."""
    return (date.today() - timedelta(days=days)).isoformat()


def sid(name: str) -> str:
    return str(uuid.uuid5(uuid.NAMESPACE_URL, "recall-demo/" + name))


def write(path: Path, events: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(e) + "\n" for e in events))


def pi(name: str, project: str, day: str, turns: list[tuple]) -> None:
    """A turn is (role, text), or ("call", tool, args, text) for an assistant
    tool call, or ("result", tool, output) for its result."""
    cwd = str(HOME / project)
    s = sid(name)
    events = [{"type": "session", "id": s, "cwd": cwd, "timestamp": f"{day}T17:00:00Z"}]
    for i, t in enumerate(turns):
        if t[0] == "call":
            msg = {"role": "assistant", "content": [
                {"type": "text", "text": t[3]},
                {"type": "toolCall", "name": t[1], "arguments": t[2]}]}
        elif t[0] == "result":
            msg = {"role": "toolResult", "toolName": t[1],
                   "content": [{"type": "text", "text": t[2]}]}
        else:
            msg = {"role": t[0], "content": [{"type": "text", "text": t[1]}]}
        events.append({"type": "message", "timestamp": f"{day}T17:{i + 1:02d}:00Z", "message": msg})
    slug = "-" + cwd.strip("/").replace("/", "-") + "-"
    write(HOME / ".pi/agent/sessions" / slug / f"{day.replace('-', '')}T090000_{s}.jsonl", events)


def claude(name: str, project: str, day: str, turns: list[tuple[str, str]]) -> None:
    cwd = str(HOME / project)
    events = []
    for i, (role, text) in enumerate(turns):
        content = text if role == "user" else [{"type": "text", "text": text}]
        events.append({
            "type": role, "timestamp": f"{day}T14:{i:02d}:00Z", "cwd": cwd,
            "message": {"role": role, "content": content},
        })
    write(HOME / ".claude/projects" / cwd.replace("/", "-") / f"{sid(name)}.jsonl", events)


def codex(name: str, project: str, day: str, turns: list[tuple[str, str]]) -> None:
    s = sid(name)
    events = [{"type": "session_meta", "timestamp": f"{day}T11:00:00Z",
               "payload": {"id": s, "cwd": str(HOME / project), "timestamp": f"{day}T11:00:00Z"}}]
    for i, (role, text) in enumerate(turns):
        events.append({"type": "response_item", "timestamp": f"{day}T11:{i + 1:02d}:00Z",
                       "payload": {"type": "message", "role": role,
                                   "content": [{"type": "text", "text": text}]}})
    y, m, d = day.split("-")
    write(HOME / ".codex/sessions" / y / m / d / f"rollout-{day}-{s}.jsonl", events)


pi("checkout-flake", "code/shop-api", ago(9), [
    ("user", "The checkout test fails about one run in twenty on CI. Find out why."),
    ("call", "bash", {"command": "go test ./checkout -run TestCheckoutPaid -count=200 -race"},
     "I will run the test 200 times with the race detector."),
    ("result", "bash", "--- FAIL: TestCheckoutPaid (0.31s)\n"
                       "    checkout_test.go:88: order 4417 status = pending, want paid\n"
                       "FAIL: 9 of 200 runs"),
    ("assistant", "The test asserts the order is paid right after it posts the Stripe webhook. "
                  "The webhook handler returns 200 and commits the order in a goroutine, "
                  "so the assertion races the commit."),
    ("user", "Fix it without a sleep."),
    ("call", "edit", {"path": "checkout/webhook.go"},
     "I will commit before the handler returns, and make the test wait for the event."),
    ("result", "edit", "Edited checkout/webhook.go and checkout/checkout_test.go"),
    ("call", "bash", {"command": "go test ./checkout -count=500 -race"},
     "Now I will check the fix."),
    ("result", "bash", "ok  	shop-api/checkout	41.2s"),
    ("assistant", "Fixed in 3f9c2ab. The webhook handler now commits the order before it returns 200, "
                  "and TestCheckoutPaid waits for the order.paid event instead of a sleep. "
                  "500 runs with -race pass."),
])
pi("rate-limit", "code/shop-api", ago(21), [
    ("user", "Add a per-tenant rate limit to the public API."),
    ("assistant", "I added a token bucket per tenant in Redis: 100 requests per second, "
                  "burst 200. Requests over the limit get 429 with a Retry-After header."),
])
pi("pg-migration", "code/shop-api", ago(33), [
    ("user", "The orders migration locked the table for four minutes in staging."),
    ("assistant", "ALTER TABLE took an ACCESS EXCLUSIVE lock. Create the index CONCURRENTLY "
                  "in its own migration, and set lock_timeout to 5s so a blocked migration fails fast."),
])
claude("checkout-ui", "code/web", ago(13), [
    ("user", "The checkout button double-submits on slow networks."),
    ("assistant", "I disabled the button while the request is in flight and added an "
                  "idempotency key, so a retried checkout cannot charge twice."),
])
codex("webhook-retry", "code/shop-api", ago(24), [
    ("user", "Stripe retries our webhook and we create duplicate orders."),
    ("assistant", "I store the event id with a unique index and skip events already seen. "
                  "The checkout flow is now idempotent for webhook retries."),
])
codex("ci-cache", "code/web", ago(18), [
    ("user", "CI takes 14 minutes. Can we cache node_modules?"),
    ("assistant", "I keyed the cache on the lockfile hash. A warm run takes 4 minutes."),
])

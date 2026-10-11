#!/usr/bin/env python3
"""Write a synthetic chat history for the README screenshot.

Usage: corpus.py <home-dir>

The corpus imitates pi, Claude Code and Codex storage under <home-dir>. All
content is invented, so the screenshot shows no real project or person.
"""

import json
import sys
import uuid
from pathlib import Path

HOME = Path(sys.argv[1]).resolve()


def sid(name: str) -> str:
    return str(uuid.uuid5(uuid.NAMESPACE_URL, "recall-demo/" + name))


def write(path: Path, events: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(e) + "\n" for e in events))


def pi(name: str, project: str, day: str, turns: list[tuple[str, str]]) -> None:
    cwd = str(HOME / project)
    s = sid(name)
    events = [{"type": "session", "id": s, "cwd": cwd, "timestamp": f"{day}T09:00:00Z"}]
    for i, (role, text) in enumerate(turns):
        events.append({
            "type": "message",
            "timestamp": f"{day}T09:{i + 1:02d}:00Z",
            "message": {"role": role, "content": [{"type": "text", "text": text}]},
        })
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


pi("checkout-flake", "code/shop-api", "2026-09-14", [
    ("user", "The checkout test fails about one run in twenty on CI. Find out why."),
    ("assistant", "The test asserts the order is paid right after it posts the Stripe webhook. "
                  "The webhook handler commits in a goroutine, so the assertion races the commit."),
    ("user", "Fix it without a sleep."),
    ("assistant", "I made the handler commit before it returns 200, and the test now waits on "
                  "the order.paid event. 500 runs with -race pass."),
])
pi("rate-limit", "code/shop-api", "2026-09-02", [
    ("user", "Add a per-tenant rate limit to the public API."),
    ("assistant", "I added a token bucket per tenant in Redis: 100 requests per second, "
                  "burst 200. Requests over the limit get 429 with a Retry-After header."),
])
pi("pg-migration", "code/shop-api", "2026-08-21", [
    ("user", "The orders migration locked the table for four minutes in staging."),
    ("assistant", "ALTER TABLE took an ACCESS EXCLUSIVE lock. Create the index CONCURRENTLY "
                  "in its own migration, and set lock_timeout to 5s so a blocked migration fails fast."),
])
claude("checkout-ui", "code/web", "2026-09-10", [
    ("user", "The checkout button double-submits on slow networks."),
    ("assistant", "I disabled the button while the request is in flight and added an "
                  "idempotency key, so a retried checkout cannot charge twice."),
])
codex("webhook-retry", "code/shop-api", "2026-08-30", [
    ("user", "Stripe retries our webhook and we create duplicate orders."),
    ("assistant", "I store the event id with a unique index and skip events already seen. "
                  "The checkout flow is now idempotent for webhook retries."),
])
codex("ci-cache", "code/web", "2026-09-05", [
    ("user", "CI takes 14 minutes. Can we cache node_modules?"),
    ("assistant", "I keyed the cache on the lockfile hash. A warm run takes 4 minutes."),
])

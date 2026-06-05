# Lua transform plugins — programmable, sandboxed, no I/O

> The escape hatch is a **pure Lua transform**: recall does all I/O (walk files,
> read bytes, run SQL, checkpoint), the plugin does all *logic* (raw bytes →
> normalized records). Lua never touches the network, filesystem, or OS. That
> single constraint — "define processing, not I/O" — is what makes this safe,
> fast, deterministic, and cheap to maintain.

## The split that makes it work

The earlier doc's tiers conflated two different jobs. Separate them:

```
┌─ I/O layer ────────────── host (Go), declared, versioned ───────────────┐
│  where bytes come from: file glob · SQLite query · offset/checkpoint     │
│  recall owns the hot loop, incremental resume, batching, FTS, MCP        │
└──────────────────────────────────┬──────────────────────────────────────┘
                                    │ raw bytes / rows  (+ small helpers)
┌──────────────────────────────────▼──────────────────────────────────────┐
│  Transform layer ── Lua, sandboxed, PURE ── no net, no fs, no os         │
│  logic only: line/file → {session}, {message…}   deterministic           │
└───────────────────────────────────────────────────────────────────────┘
```

A plugin author writes **pure functions**. They cannot make a network call,
read a file, or shell out — not by policy we have to enforce per-call, but
because **those capabilities are simply never injected into the Lua VM.** The
sandbox is "what we chose to expose," and we expose math/string/table + a few
recall helpers. Nothing else exists.

## Why "no I/O in Lua" is the winning constraint

From `plugin-futures-2030.md`, embedded scripting's failure mode was *"you now
own a second contract (a host API) forever, and it churns."* Pure transform
collapses that:

- **The API surface is tiny and stable.** No `http`, no `open`, no `exec` to
  design, secure, deprecate, or version. The whole contract is: *here are bytes,
  give me records.* That barely changes across years.
- **Deterministic = testable + cacheable.** Same input → same output, always.
  `recall plugin test x sample.jsonl` is exact. We can memoize/skip unchanged
  input with confidence.
- **No trust surface over the wire.** Option A's supply-chain risk (arbitrary
  executables hitting the network) disappears. A malicious plugin can waste CPU;
  it cannot exfiltrate your chat history or phone home. Worst case is bounded by
  a context timeout + memory ceiling.
- **Single static binary preserved** (see runtime choice below).

It keeps Option B's power (real logic for Markdown, odd joins, computed fields)
while shedding Option B's cost. The only thing it gives up vs Option A is
authenticated *remote* sources — handled separately below, still without putting
the wire in Lua.

## Runtime: gopher-lua (pure Go, no CGO)

`github.com/yuin/gopher-lua` is a Lua 5.1 VM implemented in pure Go — **no CGO**,
so recall stays a single static ~10–12 MB binary (the hard constraint in the
README / `go.mod`). Sandboxing is by omission:

```go
L := lua.NewState(lua.Options{SkipOpenLibs: true}) // open NOTHING by default
for _, lib := range []struct{ n string; f lua.LGFunction }{
    {lua.BaseLibName, lua.OpenBase},     // then strip dofile/loadfile/load/print
    {lua.TabLibName, lua.OpenTable},
    {lua.StringLibName, lua.OpenString},
    {lua.MathLibName, lua.OpenMath},
} { L.Push(L.NewFunction(lib.f)); L.Push(lua.LString(lib.n)); L.Call(1, 0) }
// NOT loaded: os, io, package(require), debug, coroutine→net. No way to reach them.
L.SetContext(ctx) // cancellation + per-call timeout; watchdog for runaway loops
```

No filesystem, no `os.execute`, no `require` of arbitrary modules, and Lua has
no built-in networking at all — so "no network" is the default, not a feature we
police. `(Shopify/go-lua, Lua 5.2, also pure Go, is the fallback option.)`

## The plugin contract

recall still owns discovery + the byte-level hot loop. The plugin exposes one of
two functions depending on granularity:

**Line transform** (JSONL agents — Copilot, Gemini, Qwen, custom):

```lua
-- copilot.lua  — pure; only math/string/table + recall.* exist
function recall_line(line, st)
  local t = recall.get(line, "type")            -- sonic path-get in Go; returns a scalar
  if t == "session_meta" then
    st.id      = recall.get(line, "payload.id")
    st.project = recall.get(line, "payload.cwd")
    return nil                                   -- meta, no message emitted
  end
  if t ~= "response_item" then return nil end
  return {                                        -- one normalized message
    role = recall.get(line, "payload.role") or "assistant",
    ts   = recall.time(recall.get(line, "payload.timestamp"), "rfc3339"),
    text = recall.get(line, "payload.content[0].text"),
  }
end
```

**File transform** (whole-file formats declarative specs can't touch — Aider's
Markdown, opencode's multi-file join):

```lua
-- aider.lua — turn .aider.chat.history.md into sessions/messages
function recall_file(doc)            -- doc.text = file contents, doc.path, doc.dir
  local session, msgs, idx = nil, {}, 0
  for _, ln in ipairs(recall.lines(doc.text)) do
    local started = ln:match("^# aider chat started at (.+)$")
    if started then
      session = { id = doc.basename, project = doc.dir,
                  started_at = recall.time(started, "2006-01-02 15:04:05") }
    elseif ln:match("^#### ") then                       -- user turn
      msgs[#msgs+1] = { idx = idx, role = "user",      text = ln:sub(6) }; idx = idx + 1
    elseif ln ~= "" then                                 -- assistant body
      msgs[#msgs+1] = { idx = idx, role = "assistant", text = ln };       idx = idx + 1
    end
  end
  return session, msgs
end
```

**Injected helpers (the entire host API — small on purpose):**

| Helper | Does | Why in Go not Lua |
|---|---|---|
| `recall.get(bytes, "a.b[0]")` | extract one scalar by path | sonic AST get — no full table marshal, keeps it fast |
| `recall.json(bytes)` | decode to a Lua table (escape hatch) | for when path-get isn't enough |
| `recall.lines(s)` | split into a line array/iterator | avoids slow Lua string churn |
| `recall.time(s, fmt)` | parse → unix ms | one timestamp impl, all plugins |
| `recall.truncate(s, n)` | cap text (tool output → 400) | matches existing excerpt logic |

The author writes glue, never a JSON or time parser. recall converts the
returned Lua tables into `Session`/`Message` and runs them through the *existing*
`batchEmitter` → FTS path unchanged. Checkpointing stays in Go: it hands `st`
(line transforms) the file offset state and persists the opaque cursor exactly
as today.

## Performance — keep the 90% off Lua entirely

The `autoresearch.md` primary metric (full-index seconds, dominated by Cursor +
the built-ins) must not move. Plan:

- **Built-in popular agents stay declarative Tier-0 (no Lua).** Claude, Codex,
  pi, Cursor, Windsurf, Gemini, etc. are field-maps interpreted in Go. Lua only
  runs for plugins that *opt in* — the long tail you install, not the hot path.
- **Reuse the VM.** One `*lua.LState` per file/goroutine, never per call.
  Pre-compile the script once to `*lua.FunctionProto` and instantiate cheaply
  per state. (Standard gopher-lua perf practice.)
- **Heavy work stays in Go via `recall.get`.** Lua names the path; sonic does the
  decode and returns a scalar. No marshaling a 1.5 KB blob into a Lua table per
  message.
- **Coarse where possible.** `recall_file` crosses the boundary once per session,
  not per line.
- **`bench.sh` is the gate**, and we benchmark a Lua-based Claude adapter against
  the Go one once, to measure the per-line boundary cost and pick granularity.

So the boundary cost is paid only by opt-in long-tail plugins on data volumes
far smaller than Cursor's 245k bubbles. The metric stays put.

## The one gap: authenticated remote sources

Pure Lua deliberately can't fetch Amp threads or a ChatGPT web export behind a
login. That's fine — **don't put the wire in Lua.** When a source needs network,
the *host* performs the fetch from a declared, user-approved manifest entry
(`fetch: { url, auth_env }`), then hands the downloaded bytes to the same
`recall_file` transform. I/O stays host-owned, declared, and auditable; logic
stays in sandboxed Lua. Network is opt-in per plugin and never reachable from
the script — which is exactly your constraint, extended to cover cloud cleanly.

## Where this lands vs the 2030 options

It's Option **B, de-risked into something close to A's reach**:

| | A proc | **B: pure Lua** | C wasm | D decl |
|---|---|---|---|---|
| Local long tail (Markdown, joins) | ✅ | ✅ | ✅ | ❌ |
| Authenticated remote | ✅ | ⚠️ host-mediated fetch | ✅ | ❌ |
| Trust surface | ⚠️ arbitrary code+net | ✅ pure, no I/O | ✅ sandbox | ✅✅ data |
| Authoring ease | any lang | one small Lua API | toolchain | easy *if it fits* |
| Contract to maintain | 1 | **1 + tiny stable helper set** | 2 (ABI) | 1 |
| Single static binary | ✅ | ✅ (gopher-lua, no CGO) | ✅ bigger | ✅ smallest |
| Perf risk | spawn/run | per-line boundary (opt-in only) | wasm calls | none |

## Migration

1. **Tier-0 declarative first** (unchanged plan): build the `jsonl`/`vscdb`
   engines, port the built-ins to specs, gate on fixtures + `bench.sh`.
2. **Add the Lua transform tier** with gopher-lua: the sandbox, `recall.*`
   helpers, `recall_line`/`recall_file` contract, VM reuse + compiled proto.
3. **Prove it on Aider** — a `recall_file` Markdown plugin — the case declarative
   can't express. Benchmark a Lua Claude adapter vs the Go one to lock granularity.
4. **`recall plugin new --kind lua` / `plugin test`** for the authoring loop;
   `plugin add <repo>` to share `.lua` files.
5. **Host-mediated fetch** for remote sources, feeding bytes into `recall_file`.

Net: declarative data for the easy 90%, sandboxed pure-Lua logic for the local
long tail, host-declared fetch for the remote tail — one small, stable contract,
no CGO, no trust-over-the-wire, and the hot path never pays for any of it.
</content>

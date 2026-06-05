# recall as a plugin host — index any agent, present and future

> The mistake is deciding *which agents to support*. We should decide on a
> **contract** and a **plugin system**, then let agents — ours and the long
> tail and tomorrow's not-yet-invented ones — be plugins against it. recall
> ships zero hard-coded agents. It ships a host.

## The reframing

Today `recall` has four hand-written adapters compiled into the binary. Adding
an agent means writing Go, reviewing a PR, and cutting a release. That doesn't
scale to a world where a new agent trends every month and every company has an
in-house one we'll never see.

Flip it: **recall is a host, agents are plugins.** There are no "built-in
agents" — only plugins, some of which happen to ship in-tree. A plugin's *only*
job is to turn some on-disk (or remote) storage into normalized records. recall
owns everything else: discovery, incremental/resumable checkpointing, FTS,
ranking, the MCP server, the CLI. That division is the entire design.

## The contract (this is the whole thing)

recall already has the contract — it's the `Adapter` interface in `types.go`,
producing `Session` and `Message` rows, with an opaque resumable `checkpoint`
string per source (`streaming.go`'s `EmitFunc`). A plugin is anything that
satisfies that contract. The genius is that recall **already treats checkpoints
as opaque** (`ix.GetMeta("ckpt:"+id)`), so a plugin gets incremental indexing
for free just by honoring the same cursor it was handed last time.

So we don't invent a contract — we **project the existing one onto a boundary a
plugin can live behind**:

```
recall gives a plugin:   the previous checkpoint string (opaque), the mode (full/incremental)
a plugin gives recall:   a stream of {session…}, {message…}, and a new checkpoint
recall does the rest:    batching, FTS ingest, resume-on-interrupt, search, MCP
```

A `Session`/`Message`/`checkpoint` triple is the *only* interface. Three
different ways to produce it, in increasing power and decreasing ease:

## Three tiers of plugin

The landscape research is what makes a tiered system work: ~15 popular agents
collapse into a handful of *shapes* (walk files → parse JSON/SQLite → map a few
fields). So the **easy tier covers ~90%**, and the powerful tiers exist only for
the long tail.

### Tier 0 — Declarative spec (data, no code) — the default

A JSON file describes where the files are and which field is the id / role /
timestamp / text. recall has one generic engine per *shape* (`jsonl`,
`json_tree`, `vscdb`, `sqlite`) that interprets the spec. No toolchain, no
compile, safe by construction, runs in-process at native speed.

This is what most plugins are — including most of ours. Adding Gemini or
Windsurf becomes a reviewable `.json` file, not Go. (Full spec schema +
examples in the appendix.)

```jsonc
// ~/.recall/plugins/gemini.json  — a whole new agent, zero code
{
  "id": "gemini", "kind": "jsonl",
  "roots": ["~/.gemini/tmp/*"],
  "resume": "gemini --resume {id}",
  "jsonl": {
    "glob": "session-*.jsonl", "type_field": "type",
    "meta":    { "match": ["session_metadata"], "session_id": {"path": "sessionId"}, "project": {"path": "cwd"} },
    "message": { "match": ["user","gemini"], "role": {"path":"type"},
                 "timestamp": {"path":"timestamp","format":"unix_ms"},
                 "content": {"path":"content","parts":{"text":{"text":"text"}}} },
    "roles": { "gemini": "assistant", "model": "assistant" }
  }
}
```

### Tier 1 — External executable (any language) — the real "extension"

When a format can't be expressed declaratively (Aider's Markdown, opencode's
3-file join, a cloud API, anything weird), a plugin is **any program** recall
runs over stdio. Discovered by convention — `recall-<id>` on `PATH`, or an
executable in `~/.recall/plugins/<id>/`. recall hands it the previous checkpoint
on stdin; it streams NDJSON records and a new checkpoint on stdout:

```
$ recall-acme            # stdin:  {"checkpoint":"<opaque>","mode":"incremental"}
{"type":"session","id":"…","project":"/repo","title":"…","started_at_ms":…}
{"type":"message","session_id":"…","idx":0,"role":"user","ts_ms":…,"text":"…"}
{"type":"message","session_id":"…","idx":1,"role":"assistant","ts_ms":…,"text":"…"}
{"type":"checkpoint","value":"<new opaque cursor>"}
```

This is the universal escape hatch. Write a plugin in Python, Node, Rust, bash —
whatever you already know. It's the lowest barrier for *arbitrary* logic, it
matches recall's own nature (recall is itself a stdio/MCP program), and it keeps
the host a single static binary with no embedded runtime. The protocol is just
the Tier-0 record types serialized to NDJSON — one schema, two transports.

> Trust note: an external plugin is arbitrary code, same trust level as
> installing any CLI tool. Tier 0 specs are safe data. recall makes the boundary
> explicit (`recall plugin add` shows what it installs) and runs plugins
> read-only by convention.

### Considered and deferred
- **Embedded scripting** (Starlark/Risor — pure Go, no CGO): a middle tier
  between data and a full process. Real, but Tier 0 + Tier 1 already span the
  space; adding a scripting runtime is a dependency and a second mental model
  for marginal gain. Revisit only if a class of formats wants in-process logic.
- **WASM** (wazero, pure Go): sandboxed *and* powerful, but authoring needs a
  WASM toolchain — that violates "easy to write." Keep on the shelf.
- **Go `plugin` package / `.so` files:** rejected — breaks the single static
  binary and is platform/version-locked.

## Why this satisfies "tomorrow's agent"

- **No release coupling.** A new agent trends → someone writes a spec or a
  `recall-x` script → drops it in `~/.recall/plugins/` → `recall index` picks it
  up. We ship nothing. They don't fork.
- **No code for the common case.** 90% of agents are a Tier-0 JSON file because
  they're all the same few shapes.
- **No ceiling for the weird case.** Tier 1 runs anything, in any language,
  including formats and remote sources we never imagined.
- **In-house agents work.** A company indexes its proprietary agent without ever
  telling us it exists.

## Authoring & lifecycle — making plugins *easy*

A plugin system is only as good as its authoring loop. Proposed surface:

```
recall plugin list                 all plugins (in-tree, ~/.recall/plugins), kind, version, root-exists
recall plugin new <id> --kind jsonl scaffold a Tier-0 spec from a template
recall plugin test <id> [file]     run the plugin against a sample, print parsed sessions/messages
recall plugin add <github-repo>    install a shared plugin (mirrors the skills.sh pattern in the README)
recall doctor                      already lists sources — now lists every plugin and flags bad ones
```

- **Discovery by convention.** Scan `~/.recall/plugins/*.json` (Tier 0) and
  `~/.recall/plugins/*/` executables (Tier 1) at startup. No registration.
- **A manifest** per plugin: `id`, `kind`, `version`, `recall_api: 1`, `roots`.
  recall refuses or warns on an incompatible `recall_api` so plugins survive
  host upgrades — the contract is versioned.
- **`recall plugin test`** is the tight loop: point it at one real history file,
  see exactly what sessions/messages come out, before indexing anything.
- **Sharing.** Plugins are just files in a git repo; `recall plugin add
  user/repo` fetches them. A community registry can grow without us gatekeeping,
  the same way the repo already distributes the `recall` skill via `npx skills add`.

## Where the agents we ship fit

They become the *first plugins*, not special cases:

| Agent(s) | Tier | Kind |
|---|---|---|
| Claude Code, Codex, pi, Copilot CLI, Gemini, Qwen | 0 | `jsonl` |
| Cursor, Windsurf, Cody | 0 | `vscdb` |
| Cline / Roo / Kilo, Continue, opencode | 0 | `json_tree` |
| Goose, Zed | 0 | `sqlite` |
| Aider (Markdown), any cloud-only or odd source | 1 | external `recall-<id>` |

`defaultAdapters()` stops being a hard-coded list and becomes "load every
plugin" — built-ins are embedded Tier-0 specs (`//go:embed plugins/*.json`)
plus user plugins from disk. The four hand-written Go adapters get ported to
specs and deleted; `adapterPath()`/`adapterFor()`'s type switches go away.

## Migration (incremental, bench-gated)

1. **Define the contract types + record protocol** (Session/Message/checkpoint
   NDJSON), versioned as `recall_api: 1`.
2. **Build the Tier-0 `jsonl` engine.** Port Claude/Codex/pi to embedded specs.
   Gate on existing fixtures (`*_test.go`) and `bench.sh` — no `full_index_seconds`
   regression (the `autoresearch.md` primary metric). Delete the `parse()` bodies.
3. **Add the `vscdb` engine**, generalize Cursor, add **Windsurf** as a spec.
4. **Ship plugin discovery from `~/.recall/plugins/`** + `recall plugin
   list/new/test`. Now anyone adds a Tier-0 agent with no code.
5. **Build the Tier-1 external-process runner.** Aider becomes the proof — a
   tiny `recall-aider` script — validating the universal escape hatch.
6. **Add `json_tree` + `sqlite` engines** → Cline/Roo/Kilo, Continue, opencode,
   Goose, Zed as specs. **`recall plugin add <repo>`** for community sharing.

### Performance guardrail
The Tier-0 interpreter must not decode each line into `map[string]any`. Use
sonic's AST path-gets (`sonic.Get(line, "payload","cwd")`) to pull only the
fields a spec names, compiled once per spec. Tier-1 spawns one process per
source per index run (not per file), so its cost is a fixed spawn, not a hot
loop. `bench.sh` stays the gate on every step.

---

## Appendix — Tier-0 spec schema

`Kind` selects the engine; the rest is data. A `FieldSrc` says where a value
comes from (a JSON path, a constant, or the filename).

```go
type SourceSpec struct {
    ID     string   `json:"id"`     // "gemini", "windsurf"
    Kind   string   `json:"kind"`   // "jsonl" | "json_tree" | "vscdb" | "sqlite"
    Roots  []string `json:"roots"`  // ~-expanded; all existing roots scanned
    Resume string   `json:"resume"` // OpenURL template, "claude --resume {id}"
    APIVer int      `json:"recall_api"`

    JSONL    *JSONLSpec    `json:"jsonl,omitempty"`
    JSONTree *JSONTreeSpec `json:"json_tree,omitempty"`
    VSCDB    *VSCDBSpec    `json:"vscdb,omitempty"`
    SQLite   *SQLiteSpec   `json:"sqlite,omitempty"`
}

type FieldSrc struct {
    Path     string `json:"path,omitempty"`     // "payload.cwd", "message.content[0].text"
    Const    string `json:"const,omitempty"`
    Filename string `json:"filename,omitempty"` // "stem" | "suffix:_<x>.jsonl"
    Format   string `json:"format,omitempty"`   // "rfc3339" | "unix_ms" | "unix_s"
}

// Content is always "a string, or a list of typed parts" — so this covers all of them.
type ContentSpec struct {
    Path  string              `json:"path"`
    Parts map[string]PartRule `json:"parts,omitempty"` // keyed by part "type"
}
type PartRule struct {
    Text     string `json:"text,omitempty"`     // path within the part to plain text
    Template string `json:"template,omitempty"` // "[tool:{name}]", "[result] {content}"
    MaxLen   int    `json:"max_len,omitempty"`  // truncate tool output → 400
    Skip     bool   `json:"skip,omitempty"`     // drop e.g. pi "thinking"
}
```

The four engines reuse the existing machinery untouched — `jsonl`/`json_tree`
ride on `scanLines`/`fileState`/`batchEmitter`; `vscdb` is Cursor's logic
parameterized by key prefixes + text paths; `sqlite` is bring-your-own-SQL with
a column→field map. (Per-kind sub-structs omitted here for brevity — see the
field tables: glob, type_field, meta{match,session_id,project}, message{match,
role,timestamp,content}, roles for `jsonl`; session_prefix/message_prefix/
headers_path/text_paths for `vscdb`; sessions_sql/messages_sql/columns for
`sqlite`; session_glob/session_file/messages_path for `json_tree`.)
</content>

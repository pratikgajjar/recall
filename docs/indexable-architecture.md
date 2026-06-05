# Making recall index *anything* — a declarative adapter architecture

> Goal: add a new agent's chat history by writing **data, not Go**. Ship the
> common agents as built-in specs, and let users index in-house or brand-new
> agents by dropping a JSON file into `~/.recall/adapters/` — no recompile, no PR.

This note has two halves:

1. **Which agents to integrate next** (the landscape, ranked).
2. **How to architect the indexer** so each new agent is a spec, not bespoke code.

---

## 1. The landscape — what to integrate next

`recall` already reads Cursor, Claude Code, Codex CLI, and pi. Here is where the
other popular agents keep their history locally, ranked by integration value
(popularity × ease of reading from disk):

| Agent | Popularity | Storage | Format | Local? | Shape |
|---|---|---|---|---|---|
| **Windsurf** (Codeium) | Very high | `~/Library/Application Support/Windsurf/User/…/state.vscdb` (Linux: `~/.config/Windsurf`) | SQLite KV blobs | Local | **vscdb** (≈ Cursor) |
| **Cline** | Very high (~5M+) | VS Code globalStorage `saoudrizwan.claude-dev/tasks/<id>/` | JSON per task | Local-only | **json_tree** |
| **Roo Code** | Very high | globalStorage `RooVeterinaryInc.roo-cline/tasks/<id>/` | JSON per task | Local-only | **json_tree** (Cline-identical) |
| **Kilo Code** | High (~1.5M) | globalStorage `kilocode.kilo-code/tasks/<id>/` | JSON per task | Local-only | **json_tree** (Cline-family) |
| **Continue.dev** | High | `~/.continue/sessions/<uuid>.json` + `sessions.json` | JSON per session | Local-only | **json_tree** |
| **GitHub Copilot CLI** | Very high brand | `~/.copilot/session-state/<id>/events.jsonl` (+ SQLite) | JSONL | Local (+opt sync) | **jsonl** |
| **Gemini CLI** | Very high | `~/.gemini/tmp/<project_hash>/logs.json` → `session-*.jsonl` | JSON→JSONL | Local-only | **jsonl** |
| **Qwen Code** | Growing | `~/.qwen/tmp/<project_hash>/*.jsonl` | JSONL | Local-only | **jsonl** (Gemini-style) |
| **opencode** | Growing fast | `~/.local/share/opencode/storage/{session,message,part}/…` | JSON, sharded | Local-only | **json_tree** (3-way join) |
| **Goose** | Popular OSS | `~/.local/share/goose/sessions/sessions.db` (+legacy `*.jsonl`) | SQLite (+JSONL) | Local-only | **sqlite** (+jsonl legacy) |
| **Zed AI** | High | `~/.local/share/zed/db/` threads; `conversations/*.json` | SQLite (+JSON) | Local-only | **sqlite** (schema undocumented) |
| **Aider** | High niche | repo-root `.aider.chat.history.md`, `.aider.input.history` | Markdown / text | Local-only | special (no schema) |
| **Sourcegraph Cody** | Moderate | VS Code `state.vscdb` blobs, exportable `.json` | SQLite KV | Mostly local | **vscdb** |
| **Amp** | Growing | Sourcegraph servers (`ampcode.com/threads`) | Cloud | **Cloud-first** | skip |

**Read this table column-by-column and the architecture writes itself.** Fifteen
agents collapse into **four structural shapes** plus two skips. Every agent
inside a shape differs only in *paths, globs, and field names* — i.e. data.

### Recommended order
1. **Windsurf** — it's `state.vscdb`, so the existing Cursor logic is ~90% reusable.
2. **Cline / Roo / Kilo** — one `json_tree` spec covers all three (huge combined install base).
3. **Continue.dev** — clean per-session JSON with `sessionId` + `workspaceDirectory` already present.
4. **Copilot CLI** — native `events.jsonl`, almost identical to the Codex adapter.
5. **Gemini CLI / Qwen Code** — JSONL; the one wrinkle is mapping the opaque
   `<project_hash>` dir back to a real cwd (read it from session metadata, not the hash).
6. Defer **opencode** (3-file join), **Goose/Zed** (native SQLite schemas), **Aider**
   (Markdown only). Skip **Amp** (cloud-only — nothing on disk to read).

---

## 2. The architecture — specs, not adapters

### 2.1 What's already generic (and stays)

The hard part of indexing is **already shared** and agent-agnostic. None of it
needs to change:

- `Session` / `Message` — the normalized output rows (`types.go`).
- `StreamingAdapter` + `EmitFunc` + `batchEmitter` — batched, resumable ingest
  (`streaming.go`).
- `fileState` + `parseFileCkpt` / `encodeFileCkpt` + `scanLines` — per-file
  offset checkpointing for incremental, mid-file-resumable JSONL reads
  (`checkpoint.go`).
- `IngestBatch`, FTS5, search, MCP — everything downstream of an `Adapter`.

Look at `claude.go`, `codex.go`, and `pi.go` side by side: the `ScanStream`
bodies are **the same function**. They walk a dir, stat each `*.jsonl`, skip
unchanged files, append-parse grown files from `prevSt.Offset`, full-parse new
ones, and emit batches. The *only* real differences are in `parse()`:

| Variation | claude | codex | pi |
|---|---|---|---|
| file glob | `*.jsonl` | `rollout-*.jsonl` | `*.jsonl` |
| session id | filename stem | `payload.id` in `session_meta` | `id` in `session` event |
| project/cwd | `cwd` field | `payload.cwd` | `cwd` |
| type field | `type` | `type` | `type` |
| meta event | — | `session_meta` | `session` |
| message event | `user`,`assistant` | `response_item` | `message` |
| role | `= type` | `payload.role` / derived | `message.role` |
| content | string \| `[]part` | `payload.content[]` | `message.content` str\|`[]part` |
| part kinds | text, tool_use, tool_result | message, function_call, reasoning | text, toolCall, toolResult |

**Every one of those rows is data.** That's the thesis: replace the three
`parse()` functions + event structs with **one engine driven by a spec**.

### 2.2 The spec

A `SourceSpec` declaratively describes one agent. The `Kind` selects which
generic engine interprets the rest:

```go
// SourceSpec describes how to discover and parse one agent's history on disk.
// A generic engine turns it into an Adapter — so a new agent is data, not code.
type SourceSpec struct {
    ID     string   `json:"id"`      // "gemini", "copilot", "windsurf"
    Kind   string   `json:"kind"`    // "jsonl" | "json_tree" | "vscdb" | "sqlite"
    Roots  []string `json:"roots"`   // ~-expanded; all existing roots scanned
    Resume string   `json:"resume"`  // OpenURL template, e.g. "claude --resume {id}"

    JSONL    *JSONLSpec    `json:"jsonl,omitempty"`
    JSONTree *JSONTreeSpec `json:"json_tree,omitempty"`
    VSCDB    *VSCDBSpec    `json:"vscdb,omitempty"`
    SQLite   *SQLiteSpec   `json:"sqlite,omitempty"`
}
```

A **field source** says where a value comes from — a JSON path into an event, a
constant, or the filename:

```go
type FieldSrc struct {
    Path     string `json:"path,omitempty"`     // dotted/indexed: "payload.cwd", "message.content[0].text"
    Const    string `json:"const,omitempty"`    // literal value
    Filename string `json:"filename,omitempty"` // "stem" | "suffix:_<x>.jsonl"
    Format   string `json:"format,omitempty"`   // "rfc3339" | "unix_ms" | "unix_s"
}
```

#### JSONL kind — covers claude, codex, pi, Copilot, Gemini, Qwen

```go
type JSONLSpec struct {
    Glob      string   `json:"glob"`       // "*.jsonl", "rollout-*.jsonl"
    TypeField string   `json:"type_field"` // "type"

    Meta struct {                          // the once-per-file session event (optional)
        Match     []string `json:"match"`     // type values, e.g. ["session_meta"]
        SessionID FieldSrc `json:"session_id"` // or set SessionID.Filename for claude
        Project   FieldSrc `json:"project"`
        StartedAt FieldSrc `json:"started_at"`
    } `json:"meta"`

    SessionID FieldSrc `json:"session_id"` // used when there's no meta event (claude: filename stem)

    Message struct {
        Match     []string    `json:"match"`     // ["response_item"] | ["user","assistant"]
        Role      FieldSrc    `json:"role"`      // Path, or Const, or Filename
        Timestamp FieldSrc    `json:"timestamp"`
        Content   ContentSpec `json:"content"`
    } `json:"message"`

    Roles map[string]string `json:"roles,omitempty"` // normalize: "gemini"->"assistant"
}
```

`ContentSpec` is the one genuinely tricky bit — but it's bounded, because every
agent's content is *"a string, or a list of typed parts."*

```go
type ContentSpec struct {
    Path  string              `json:"path"`            // to the string or the []parts
    Parts map[string]PartRule `json:"parts,omitempty"` // keyed by part "type"
}

type PartRule struct {
    Text     string `json:"text,omitempty"`     // path within the part to plain text
    Template string `json:"template,omitempty"` // "[tool:{name}]", "[result] {content}"
    MaxLen   int    `json:"max_len,omitempty"`  // truncate (tool output → 400)
    Skip     bool   `json:"skip,omitempty"`     // e.g. pi "thinking"
}
```

That reproduces today's `ClaudeMessage.Text()` / `PiMessage.Text()` /
`CodexPayload.flatten()` exactly — the `tool_use → [tool:name]`, the 400-char
truncation, the dropped thinking blocks — all as table entries.

#### VSCDB kind — covers Cursor, Windsurf, Cody

Cursor's adapter is already this shape; Windsurf differs by **path + key names**:

```go
type VSCDBSpec struct {
    GlobalDB      string   `json:"global_db"`       // "globalStorage/state.vscdb"
    WorkspaceGlob string   `json:"workspace_glob"`  // "workspaceStorage/*/state.vscdb"
    SessionPrefix string   `json:"session_prefix"`  // "composerData:"
    MessagePrefix string   `json:"message_prefix"`  // "bubbleId:"
    HeadersPath   string   `json:"headers_path"`    // "fullConversationHeadersOnly[].bubbleId"
    TextPaths     []string `json:"text_paths"`      // ["text","richText","content"] (first non-empty)
    RoleField     string   `json:"role_field"`      // "type"; mapped via Roles
    TitlePath     string   `json:"title_path"`      // "name"
    CreatedPath   string   `json:"created_path"`    // "createdAt"
}
```

#### SQLite kind — covers Goose, Zed (bring-your-own-SQL)

```go
type SQLiteSpec struct {
    DB           string            `json:"db"`            // "sessions.db"
    SessionsSQL  string            `json:"sessions_sql"`  // SELECT id,title,cwd,started_at,…
    MessagesSQL  string            `json:"messages_sql"`  // SELECT session_id,idx,role,ts,text WHERE …
    ColumnMap    map[string]string `json:"columns"`       // result column -> Session/Message field
    Roles        map[string]string `json:"roles,omitempty"`
}
```

#### JSONTree kind — covers Cline/Roo/Kilo, Continue, opencode

A directory of session folders/files; each holds a message array.

```go
type JSONTreeSpec struct {
    SessionGlob string   `json:"session_glob"` // "tasks/*/", "sessions/*.json"
    SessionFile string   `json:"session_file"` // "api_conversation_history.json" ("" if glob IS the file)
    SessionID   FieldSrc `json:"session_id"`   // usually folder/file name
    Project     FieldSrc `json:"project"`      // from a sibling metadata file or a field
    MessagesPath string  `json:"messages_path"`// "history", "" (root array), "messages"
    Message struct {
        Role      FieldSrc    `json:"role"`
        Timestamp FieldSrc    `json:"timestamp"`
        Content   ContentSpec `json:"content"`
    } `json:"message"`
}
```

### 2.3 The engines

One small generic adapter per kind, each implementing the existing `Adapter` +
`StreamingAdapter` interfaces. The `jsonl` and `json_tree` engines **reuse**
`scanLines`, `fileState`, and `batchEmitter` unchanged — they only swap the
hand-written `parse()` for a spec interpreter:

```go
type specAdapter struct {
    spec   SourceSpec
    engine engine // jsonlEngine | jsonTreeEngine | vscdbEngine | sqliteEngine
}

func (a *specAdapter) ID() string              { return a.spec.ID }
func (a *specAdapter) Available() bool         { return a.engine.anyRootExists() }
func (a *specAdapter) OpenURL(id string) string { return render(a.spec.Resume, id) }
func (a *specAdapter) ScanStream(ctx, prev, emit) error { return a.engine.scan(ctx, a.spec, prev, emit) }
```

The interpreter walks `FieldSrc.Path` over each line. To stay within the
project's performance budget (see `autoresearch.md` — full index time is the
primary metric), **do not decode each line into `map[string]any`.** Use sonic's
AST path access (`sonic.Get(line, "payload", "cwd")`) to pull only the handful
of fields a spec names, skipping the rest of the blob. Compile each spec's paths
once at load. This keeps the hot loop allocation-light and close to the current
typed-struct speed.

### 2.4 The registry — this is the "without us adding code" part

```go
func loadSpecs() []SourceSpec {
    var specs []SourceSpec
    specs = append(specs, builtinSpecs()...)             // //go:embed adapters/*.json
    specs = append(specs, userSpecs("~/.recall/adapters/*.json")...) // runtime, user-supplied
    return specs
}

func defaultAdapters() []Adapter {
    var out []Adapter
    for _, s := range loadSpecs() {
        out = append(out, newSpecAdapter(s))
    }
    return out
}
```

Two consequences:

- **Built-in agents ship as embedded JSON specs.** Adding Windsurf/Cline/Gemini
  to the binary is a new `adapters/<id>.json` file — reviewable as data, no Go.
- **Users index anything we've never heard of.** Drop
  `~/.recall/adapters/acme.json` describing your company's in-house agent and
  `recall index` picks it up. No fork, no recompile, no waiting on a release.
  That is literally "anything indexable without us adding specific code."

`recall doctor` lists every spec (built-in + user) and whether its roots exist,
so a malformed or mis-pathed user spec is obvious immediately. `adapterPath()`
and `adapterFor()` in `main.go` lose their hardcoded type switches and read from
the spec instead.

### 2.5 Example specs

**claude** (no meta event — id from filename):

```json
{
  "id": "claude",
  "kind": "jsonl",
  "roots": ["~/.claude/projects"],
  "resume": "claude --resume {id}",
  "jsonl": {
    "glob": "*.jsonl",
    "type_field": "type",
    "session_id": { "filename": "stem" },
    "message": {
      "match": ["user", "assistant"],
      "role": { "path": "type" },
      "timestamp": { "path": "timestamp", "format": "rfc3339" },
      "content": {
        "path": "message.content",
        "parts": {
          "text":        { "text": "text" },
          "tool_use":    { "template": "[tool_use:{name}]" },
          "tool_result": { "template": "[tool_result] {content}", "max_len": 400 }
        }
      }
    }
  }
}
```

**gemini** (new agent — pure data, mirrors the Codex pattern):

```json
{
  "id": "gemini",
  "kind": "jsonl",
  "roots": ["~/.gemini/tmp/*"],
  "resume": "gemini --resume {id}",
  "jsonl": {
    "glob": "session-*.jsonl",
    "type_field": "type",
    "meta": {
      "match": ["session_metadata"],
      "session_id": { "path": "sessionId" },
      "project": { "path": "cwd" }
    },
    "message": {
      "match": ["user", "gemini"],
      "role": { "path": "type" },
      "timestamp": { "path": "timestamp", "format": "unix_ms" },
      "content": { "path": "content", "parts": { "text": { "text": "text" } } }
    },
    "roles": { "gemini": "assistant", "model": "assistant" }
  }
}
```

**windsurf** (reuses the Cursor/vscdb engine — just a different path):

```json
{
  "id": "windsurf",
  "kind": "vscdb",
  "roots": ["~/Library/Application Support/Windsurf/User", "~/.config/Windsurf/User"],
  "resume": "windsurf://…/{id}",
  "vscdb": {
    "global_db": "globalStorage/state.vscdb",
    "workspace_glob": "workspaceStorage/*/state.vscdb",
    "session_prefix": "composerData:",
    "message_prefix": "bubbleId:",
    "headers_path": "fullConversationHeadersOnly[].bubbleId",
    "text_paths": ["text", "richText", "content"],
    "role_field": "type",
    "title_path": "name",
    "created_path": "createdAt"
  }
}
```

---

## 3. Migration path (incremental, zero-regression)

1. **Build the `jsonl` engine + spec types.** Port **claude, codex, pi** to
   embedded specs. Keep the typed adapters until the engine's output matches
   byte-for-byte. Gate on the existing fixtures (`claude_test.go`, `codex_test.go`,
   `pi_test.go`) and on `bench.sh` showing no `full_index_seconds` regression
   (the autoresearch primary metric). Delete the three `parse()` bodies once green.
2. **Add the new JSONL agents as specs only:** Copilot CLI, Gemini, Qwen. No Go.
3. **Generalize Cursor into the `vscdb` engine**, then add **Windsurf** (and
   later Cody) as specs.
4. **Add the `json_tree` engine** → Cline/Roo/Kilo (one spec family) + Continue + opencode.
5. **Add the `sqlite` engine** → Goose + Zed when their schemas are pinned down.
6. **Ship spec loading from `~/.recall/adapters/`** and document the spec format
   so users (and we) add agents without touching Go.

### What stays special-cased
- **Aider** — Markdown transcript, no session ids/timestamps. Needs a tiny
  bespoke reader or a generic `markdown` kind later; not worth a spec field today.
- **Amp** — cloud-only. Out of scope for a read-from-disk indexer by definition.

### Risks / things to watch
- **Performance.** A naïve `map[string]any` interpreter would regress the
  primary metric. Mitigation: sonic AST path-gets + compiled paths (§2.3); keep
  `bench.sh` as the gate.
- **Content flattening is the long tail.** Most agents fit
  string-or-typed-parts, but odd cases (opencode's part sharding, Codex's
  `reasoning` summaries) may need one or two extra `PartRule` knobs. Add them as
  data when a real format demands it, not speculatively.
- **Spec validation.** User specs are untrusted input; validate on load and have
  `recall doctor` surface bad paths/missing roots clearly.
</content>
</invoke>

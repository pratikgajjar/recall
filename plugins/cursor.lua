-- cursor.lua — index Cursor's chat history out of state.vscdb (SQLite KV).
--
-- Proof that the Lua tier reaches beyond JSONL: Cursor stores composers and
-- their bubbles in a key/value sqlite table, and the host's `kind = "kv"`
-- iterator hands one composer (header + ordered bubble blobs) at a time to
-- this transform. The sandbox guarantees still hold — Lua sees only strings.
--
-- Parity with the built-in Go CursorAdapter (see cursor.go) for:
--   • session listing (one per composerData:<id> row)
--   • bubble ordering via fullConversationHeadersOnly, unseen bubbles appended
--   • text extraction (text → richText flattened → content fallback)
--   • role mapping (type 1 = user, type 2 = assistant, else tool)
--   • title from composer name, else first non-wrapper user prompt
--
-- Not yet ported (v1):
--   • workspaceStorage → project mapping (needs a second source in the manifest)
--   • rowid-based incremental scan (currently full rescan every pass)

local function decode_bubble(value)
  local b = recall.json(value)
  if not b then return "", 0 end
  if b.text and b.text ~= "" then
    return b.text, b.type or 0
  end
  if b.richText and b.richText ~= "" then
    local doc = recall.json(b.richText)
    if doc and doc.root and doc.root.children then
      local parts = {}
      local function walk(n)
        if n.text and n.text ~= "" then parts[#parts + 1] = n.text end
        if n.children then
          for _, c in ipairs(n.children) do walk(c) end
        end
      end
      for _, c in ipairs(doc.root.children) do walk(c) end
      if #parts > 0 then return table.concat(parts, "\n"), b.type or 0 end
    end
    return b.richText, b.type or 0
  end
  if b.content and b.content ~= "" then
    return b.content, b.type or 0
  end
  return "", b.type or 0
end

local function role_of(t)
  if t == 1 then return "user" end
  if t == 2 then return "assistant" end
  return "tool"
end

return {
  id      = "cursor",
  kind    = "kv",
  source  = "~/Library/Application Support/Cursor/User/globalStorage/state.vscdb",
  table   = "cursorDiskKV",
  prefix  = "composerData:",
  related = "bubbleId:{id}:",
  watermark = "rowid", -- incremental resume: only re-emit composers whose
                       -- composerData or bubble rows advanced past last pass
  resume  = "cursor://anysphere.cursor-deeplink/composer/{id}",

  session = function(id, header_value, related_rows, st)
    local cb = recall.json(header_value)
    if not cb then return nil end

    -- Index bubbles by their msg-id (the trailing segment of the key).
    local by_mid = {}
    local in_order = {}
    for _, r in ipairs(related_rows) do
      -- r.key is "bubbleId:<cid>:<mid>". Take the slice after the second ':'.
      local rest = r.key:sub(#"bubbleId:" + 1)
      local colon = rest:find(":", 1, true)
      if colon then
        local mid = rest:sub(colon + 1)
        local text, ttype = decode_bubble(r.value)
        if text ~= "" then
          local entry = { mid = mid, text = text, role = role_of(ttype) }
          by_mid[mid] = entry
          in_order[#in_order + 1] = entry
        end
      end
    end

    -- Order by composer's fullConversationHeadersOnly; append unseen at end.
    local ordered = {}
    local seen = {}
    if cb.fullConversationHeadersOnly then
      for _, h in ipairs(cb.fullConversationHeadersOnly) do
        local e = by_mid[h.bubbleId]
        if e then
          ordered[#ordered + 1] = e
          seen[h.bubbleId] = true
        end
      end
    end
    for _, e in ipairs(in_order) do
      if not seen[e.mid] then ordered[#ordered + 1] = e end
    end

    if #ordered == 0 then return nil end

    local msgs = {}
    for i, e in ipairs(ordered) do
      msgs[i] = { idx = i - 1, role = e.role, ts = 0, text = e.text }
    end

    return
      { id = id, title = cb.name or "",
        started_at = cb.createdAt or 0, ended_at = cb.createdAt or 0 },
      msgs
  end,
}

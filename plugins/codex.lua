-- codex.lua — index OpenAI Codex CLI sessions
-- (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl). Pure transform; reproduces
-- the built-in Go adapter (see lua_test.go).

-- arg_summary_raw mirrors argSummary in transcript.go for codex, whose payload
-- carries arguments as a JSON *string* rather than an object. lua_test.go
-- asserts this agrees with the Go adapter.
local ARG_KEYS = { "command", "path", "query", "pattern", "file_path" }
local function arg_summary_raw(raw)
  if type(raw) ~= "string" or raw == "" then return nil end
  local obj = recall.json(raw)
  if type(obj) ~= "table" then return nil end
  for _, k in ipairs(ARG_KEYS) do
    local v = obj[k]
    if type(v) == "string" and v ~= "" then
      v = v:gsub("%s+", " "):gsub("^%s*(.-)%s*$", "%1")
      if #v > 70 then v = v:sub(1, 70) .. "\u{2026}" end
      return v
    end
  end
  return nil
end

local function join_text(parts)
  if type(parts) ~= "table" then return "" end
  local out = {}
  for _, p in ipairs(parts) do
    if p.text and p.text ~= "" then out[#out + 1] = p.text end
  end
  return table.concat(out, "\n")
end

return {
  id = "codex",
  kind = "line",
  roots = { "~/.codex/sessions" },
  glob = "rollout-*.jsonl",
  resume = "codex resume {id}",

  line = function(line, st)
    local t = recall.get(line, "type")
    if t == "session_meta" then
      st.id = recall.get(line, "payload.id") or st.id
      st.project = recall.get(line, "payload.cwd") or st.project
      st.started_at = recall.time(recall.get(line, "payload.timestamp"), "rfc3339")
      return nil
    end
    if t == "turn_context" then
      st.model = recall.get(line, "payload.model") or st.model
      return nil
    end
    if t == "event_msg" then
      -- token_count carries a running total for the whole session, so the
      -- last one wins rather than summing (summing would multiply-count).
      local tu = recall.get(line, "payload.info.total_token_usage")
      if type(tu) == "table" and (tu.total_tokens or 0) > 0 then
        local cached = tu.cached_input_tokens or 0
        st.tokens_in = (tu.input_tokens or 0) - cached
        st.tokens_out = tu.output_tokens or 0
        st.cache_read = cached
      end
      return nil
    end
    if t ~= "response_item" then return nil end

    local it = recall.get(line, "payload.type")
    local role, text
    if it == "message" then
      role = recall.get(line, "payload.role")
      text = join_text(recall.get(line, "payload.content"))
    elseif it == "function_call" then
      local nm = recall.get(line, "payload.name") or ""
      local cid = recall.get(line, "payload.call_id")
      if cid and nm ~= "" then st.calls = st.calls or {}; st.calls[cid] = nm end
      role = "tool:" .. nm
      text = arg_summary_raw(recall.get(line, "payload.arguments")) or
          recall.truncate(recall.get(line, "payload.arguments") or "", 400)
    elseif it == "function_call_output" then
      role = "tool"
      local cid = recall.get(line, "payload.call_id")
      if cid and st.calls and st.calls[cid] then role = "tool:" .. st.calls[cid] end
      local o = recall.get(line, "payload.output")
      local s = ""
      if type(o) == "string" then
        s = o
      elseif type(o) == "table" then
        s = o.content or ""
      end
      text = recall.truncate(s, 400)
    elseif it == "reasoning" then
      role = "assistant"
      text = join_text(recall.get(line, "payload.summary"))
    else
      return nil
    end

    if not text or text == "" then return nil end
    return { role = role, ts = recall.time(recall.get(line, "timestamp"), "rfc3339"), text = text }
  end,
}

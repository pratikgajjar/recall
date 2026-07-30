-- codex.lua — index OpenAI Codex CLI sessions
-- (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl). Pure transform; reproduces
-- the built-in Go adapter (see lua_test.go).

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
      role = "tool"
      text = "[call " .. (recall.get(line, "payload.name") or "") .. "] " ..
          recall.truncate(recall.get(line, "payload.arguments") or "", 400)
    elseif it == "function_call_output" then
      role = "tool"
      local o = recall.get(line, "payload.output")
      local s = ""
      if type(o) == "string" then
        s = o
      elseif type(o) == "table" then
        s = o.content or ""
      end
      text = "[output] " .. recall.truncate(s, 400)
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

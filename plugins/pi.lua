-- pi.lua — index pi agent sessions (~/.pi/agent/sessions/<cwd>/<ts>_<id>.jsonl)
-- Pure transform; reproduces the built-in Go adapter (see lua_test.go).

-- pi content is a string or an array of typed parts (text/toolCall/toolResult).
-- arg_summary picks the field that identifies what a call did and clips it, so
-- a transcript shows "[tool:bash] git log" rather than a bare marker. Mirrors
-- argSummary in transcript.go; lua_test.go asserts the two agree.
local ARG_KEYS = { "command", "path", "query", "pattern", "file_path" }
local function arg_summary(input)
  if type(input) ~= "table" then return nil end
  for _, k in ipairs(ARG_KEYS) do
    local v = input[k]
    if type(v) == "string" and v ~= "" then
      v = v:gsub("%s+", " "):gsub("^%s*(.-)%s*$", "%1")
      if #v > 70 then v = v:sub(1, 70) .. "\u{2026}" end
      return v
    end
  end
  return nil
end

local function flatten(c)
  if type(c) == "string" then return c end
  if type(c) ~= "table" then return "" end
  local out = {}
  for _, p in ipairs(c) do
    if p.type == "text" and p.text and p.text ~= "" then
      out[#out + 1] = p.text
    elseif p.type == "toolCall" and p.name and p.name ~= "" then
      local __m = "[tool:" .. p.name .. "]"
      local __a = arg_summary(p.arguments)
      if __a then __m = __m .. " " .. __a end
      out[#out + 1] = __m
    elseif p.type == "toolResult" then
      local c2 = p.content
      if type(c2) == "string" and c2 ~= "" then
        out[#out + 1] = "[result] " .. recall.truncate(c2, 400)
      elseif type(c2) == "table" then
        for _, inner in ipairs(c2) do
          if inner.text and inner.text ~= "" then
            out[#out + 1] = "[result] " .. recall.truncate(inner.text, 400)
          end
        end
      end
    end
  end
  return table.concat(out, "\n")
end

return {
  id = "pi",
  kind = "line",
  roots = { "~/.pi/agent/sessions" },
  glob = "*.jsonl",
  resume = "{id}",

  line = function(line, st)
    local t = recall.get(line, "type")
    if t == "session" then
      st.id = recall.get(line, "id") or st.id
      st.project = recall.get(line, "cwd") or st.project
      st.started_at = recall.time(recall.get(line, "timestamp"), "rfc3339")
      return nil
    end
    if t == "model_change" then
      -- Last model wins: what the session ended up running on.
      st.model = recall.get(line, "modelId") or st.model
      return nil
    end
    if t ~= "message" then return nil end

    -- pi records real per-message usage on every assistant turn. Sum it:
    -- that is exactly what `/usage` reports inside the agent.
    local u = recall.get(line, "message.usage")
    if type(u) == "table" then
      st.model = recall.get(line, "message.model") or st.model
      st.tokens_in = (st.tokens_in or 0) + (u.input or 0)
      st.tokens_out = (st.tokens_out or 0) + (u.output or 0)
      st.cache_read = (st.cache_read or 0) + (u.cacheRead or 0)
      st.cache_write = (st.cache_write or 0) + (u.cacheWrite or 0)
      if type(u.cost) == "table" then
        st.cost_usd = (st.cost_usd or 0) + (u.cost.total or 0)
      end
    end

    local text = flatten(recall.get(line, "message.content"))
    if text == "" then return nil end
    return {
      role = recall.get(line, "message.role"),
      ts = recall.time(recall.get(line, "timestamp"), "rfc3339"),
      text = text,
    }
  end,
}

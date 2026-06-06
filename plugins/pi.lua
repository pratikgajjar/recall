-- pi.lua — index pi agent sessions (~/.pi/agent/sessions/<cwd>/<ts>_<id>.jsonl)
-- Pure transform; reproduces the built-in Go adapter (see lua_test.go).

-- pi content is a string or an array of typed parts (text/toolCall/toolResult).
local function flatten(c)
  if type(c) == "string" then return c end
  if type(c) ~= "table" then return "" end
  local out = {}
  for _, p in ipairs(c) do
    if p.type == "text" and p.text and p.text ~= "" then
      out[#out + 1] = p.text
    elseif p.type == "toolCall" and p.name and p.name ~= "" then
      out[#out + 1] = "[tool:" .. p.name .. "]"
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
    if t ~= "message" then return nil end
    local text = flatten(recall.get(line, "message.content"))
    if text == "" then return nil end
    return {
      role = recall.get(line, "message.role"),
      ts = recall.time(recall.get(line, "timestamp"), "rfc3339"),
      text = text,
    }
  end,
}

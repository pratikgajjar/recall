-- claude.lua — index Claude Code sessions (~/.claude/projects/<cwd>/<id>.jsonl)
-- A pure transform: recall walks the files and feeds lines; this only maps a
-- line to a record. Reproduces the built-in Go adapter (see lua_test.go).

-- Claude message content is either a string or an array of typed parts.
local function flatten(c)
  if type(c) == "string" then return c end
  if type(c) ~= "table" then return "" end
  local out = {}
  for _, p in ipairs(c) do
    if p.type == "text" and p.text and p.text ~= "" then
      out[#out + 1] = p.text
    elseif p.type == "tool_use" and p.name and p.name ~= "" then
      out[#out + 1] = "[tool_use:" .. p.name .. "]"
    elseif p.type == "tool_result" then
      -- content is a string, or an array of parts (real transcripts use both);
      -- join the array's text, matching the Go adapter.
      local s = ""
      if type(p.content) == "string" then
        s = p.content
      elseif type(p.content) == "table" then
        local inner = {}
        for _, q in ipairs(p.content) do
          if q.text and q.text ~= "" then inner[#inner + 1] = q.text end
        end
        s = table.concat(inner, "\n")
      end
      if s ~= "" then
        out[#out + 1] = "[tool_result] " .. recall.truncate(s, 400)
      end
    end
  end
  return table.concat(out, "\n")
end

return {
  id = "claude",
  kind = "line",
  roots = { "~/.claude/projects" },
  glob = "*.jsonl",
  resume = "claude --resume {id}",

  line = function(line, st)
    -- The session id is the file name; cwd comes from the events themselves.
    if not st.id or st.id == "" then st.id = st.basename end
    local cwd = recall.get(line, "cwd")
    if cwd and cwd ~= "" and (not st.project or st.project == "") then
      st.project = cwd
    end

    local t = recall.get(line, "type")
    if t ~= "user" and t ~= "assistant" then return nil end
    local text = flatten(recall.get(line, "message.content"))
    if text == "" then return nil end
    return {
      role = t,
      ts = recall.time(recall.get(line, "timestamp"), "rfc3339"),
      text = text,
    }
  end,
}

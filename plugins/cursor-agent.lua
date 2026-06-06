-- cursor-agent.lua — index Cursor Agent CLI sessions
-- (~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl).
--
-- The on-disk format is JSONL with one record per turn:
--   {"role":"user"|"assistant", "message":{"content":[ <parts...> ]}}
-- where each part is one of:
--   {"type":"text", "text":"..."}
--   {"type":"tool_use", "name":"...", "input":{...}}
-- Cursor scrubs internal/reasoning blocks to "[REDACTED]" in the on-disk
-- transcript, and the file carries no per-event timestamp — so ts is 0.
-- Tool *results* are not echoed into the file, only the tool calls.

local function flatten(content)
  if type(content) ~= "table" then return "" end
  local out = {}
  for _, p in ipairs(content) do
    if p.type == "text" and p.text and p.text ~= "" then
      out[#out + 1] = p.text
    elseif p.type == "tool_use" and p.name and p.name ~= "" then
      out[#out + 1] = "[tool_use:" .. p.name .. "]"
    end
  end
  return table.concat(out, "\n")
end

-- Project slug ("home-user-recall") is the directory segment sitting right
-- before /agent-transcripts/ in the file's path. It's the absolute cwd with
-- slashes flattened to dashes — lossy to reverse, but a stable facet.
local function slug_from_dir(dir)
  return dir:match("([^/]+)/agent%-transcripts/") or ""
end

return {
  id      = "cursor-agent",
  kind    = "line",
  roots   = { "~/.cursor/projects" },
  glob    = "*.jsonl",
  resume  = "cursor-agent --resume {id}",

  line = function(line, st)
    -- Session id == file basename (already ext-stripped by the host).
    if not st.id or st.id == "" then st.id = st.basename end
    if (not st.project or st.project == "") and st.dir then
      st.project = slug_from_dir(st.dir)
    end

    local role = recall.get(line, "role")
    if role ~= "user" and role ~= "assistant" then return nil end
    local text = flatten(recall.get(line, "message.content"))
    if text == "" then return nil end
    return { role = role, ts = 0, text = text }
  end,
}

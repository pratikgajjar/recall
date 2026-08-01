-- cursor-agent.lua — index Cursor Agent CLI sessions
-- (~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl).
--
-- The on-disk format is JSONL with one record per turn:
--   {"role":"user"|"assistant", "message":{"content":[ <parts...> ]}}
-- where each part is one of:
--   {"type":"text", "text":"..."}
--   {"type":"tool_use", "name":"...", "input":{...}}
-- Cursor scrubs internal/reasoning blocks to "[REDACTED]" and echoes only tool
-- calls (not results). Per-message ts is 0; the file has no per-event time, so
-- the session is dated by file mtime (st.mtime) instead.

-- User turns are wrapped: an optional <timestamp>…</timestamp> preamble then
-- the prompt inside <user_query>…</user_query>. Strip both so the title and
-- indexed text are the bare prompt, not the markup.
local function strip_user_wrappers(s)
  s = s:gsub("<timestamp>.-</timestamp>", "")
  s = s:gsub("</?user_query>", "")
  return (s:gsub("^%s+", ""):gsub("%s+$", ""))
end

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

local function flatten(content, role)
  if type(content) ~= "table" then return "" end
  local out = {}
  for _, p in ipairs(content) do
    if p.type == "text" and p.text and p.text ~= "" then
      local t = role == "user" and strip_user_wrappers(p.text) or p.text
      if t ~= "" then out[#out + 1] = t end
    elseif p.type == "tool_use" and p.name and p.name ~= "" then
      local __m = "[tool_use:" .. p.name .. "]"
      local __a = arg_summary(p.input)
      if __a then __m = __m .. " " .. __a end
      out[#out + 1] = __m
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
    -- No per-event timestamps in the file; date the session by file mtime so
    -- it sorts by recency and honors --since.
    if st.mtime and st.mtime > 0 then
      st.started_at = st.mtime
      st.ended_at = st.mtime
    end

    local role = recall.get(line, "role")
    if role ~= "user" and role ~= "assistant" then return nil end
    local text = flatten(recall.get(line, "message.content"), role)
    if text == "" then return nil end
    return { role = role, ts = 0, text = text }
  end,
}

-- Index Grok Build sessions under ~/.grok/sessions/<percent-encoded-cwd>/<id>/
-- One session per chat_history.jsonl. `content` is a string or an array of
-- {type="text", text=…} parts. Reasoning frames are skipped; tool output is
-- clipped like every other adapter.
local function flatten(content)
  if type(content) == "string" then
    return content
  end
  if type(content) ~= "table" then
    return ""
  end
  local out = {}
  for _, part in ipairs(content) do
    if type(part) == "string" and part ~= "" then
      out[#out + 1] = part
    elseif type(part) == "table" and type(part.text) == "string" and part.text ~= "" then
      out[#out + 1] = part.text
    end
  end
  return table.concat(out, "\n")
end

-- The session directory is the id; its parent is the percent-encoded cwd.
local function session_id(st)
  local dir = st.dir or ""
  return dir:match("([^/]+)$") or st.id
end

local function percent_decode(s)
  return (s:gsub("%%(%x%x)", function(hex)
    return string.char(tonumber(hex, 16))
  end))
end

-- backend_tool_call carries the tool kind and its arguments in `kind`.
local function tool_call_text(line)
  local tool = recall.get(line, "kind.tool_type") or "tool"
  local action = recall.get(line, "kind.action")
  local detail = ""
  if type(action) == "table" then
    for _, key in ipairs({ "query", "url", "path", "command", "prompt" }) do
      local v = action[key]
      if type(v) == "string" and v ~= "" then
        detail = v
        break
      end
    end
  end
  if detail == "" then
    return recall.truncate(tool, 400)
  end
  return recall.truncate(tool .. " " .. detail, 400)
end

return {
  id = "grok",
  kind = "line",
  roots = { "~/.grok/sessions" },
  glob = "chat_history.jsonl",
  resume = "grok --resume {id}",

  line = function(line, st)
    st.id = session_id(st)
    -- Grok writes no timestamps anywhere in chat_history.jsonl (checked across
    -- 2,418 local files), so the log's own mtime is the only clock available.
    -- Without it every grok session indexes at 0 and sorts as 1970.
    if (st.started_at or 0) == 0 and (st.mtime or 0) > 0 then
      st.started_at = st.mtime
    end
    local parent = (st.dir or ""):match("(.+)/[^/]+$") or ""
    local encoded = parent:match("([^/]+)$")
    if encoded and encoded:find("%%") and (not st.project or st.project == "") then
      st.project = percent_decode(encoded)
    end

    local t = recall.get(line, "type")
    local role, text
    if t == "user" or t == "assistant" then
      role = t
      text = flatten(recall.get(line, "content"))
    elseif t == "tool_result" then
      role = "tool"
      text = recall.truncate(flatten(recall.get(line, "content")), 400)
    elseif t == "backend_tool_call" then
      role = "tool"
      text = tool_call_text(line)
    else
      return nil
    end
    if not text or text == "" then
      return nil
    end
    return { role = role, text = text }
  end,
}

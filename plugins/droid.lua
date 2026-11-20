-- Factory Droid sessions at ~/.factory/sessions/<cwd>/<session-id>.jsonl.
-- One JSONL line can carry several content parts. recall's line hook returns
-- one event, so text parts are joined and tool calls are appended to that event.

-- recall.json accepts a string only. Passing a table errors, and recall still
-- checkpoints that file, so a later incremental index will not reread it.
local function arg_summary(args)
  if type(args) == "string" then
    return recall.truncate(args, 400)
  end
  if type(args) ~= "table" then
    return ""
  end
  local keys = { "command", "path", "file_path", "filePath", "pattern", "query", "url", "skill" }
  for _, key in ipairs(keys) do
    if type(args[key]) == "string" and args[key] ~= "" then
      return recall.truncate(args[key], 400)
    end
  end
  local bits = {}
  for key, value in pairs(args) do
    if type(value) == "string" and value ~= "" then
      bits[#bits + 1] = value
    elseif type(value) == "number" or type(value) == "boolean" then
      bits[#bits + 1] = tostring(value)
    end
  end
  return recall.truncate(table.concat(bits, " "), 400)
end

local function part_text(part)
  if type(part) == "string" then
    return part
  end
  if type(part) ~= "table" then
    return ""
  end
  if type(part.text) == "string" then
    return part.text
  end
  if type(part.content) == "string" then
    return part.content
  end
  if type(part.content) ~= "table" then
    return ""
  end
  local bits = {}
  for _, inner in ipairs(part.content) do
    local text = part_text(inner)
    if text ~= "" then
      bits[#bits + 1] = text
    end
  end
  return table.concat(bits, "\n")
end

-- Returns searchable body text and a short tool summary.
local function split_content(content)
  if type(content) == "string" then
    return content, ""
  end
  if type(content) ~= "table" then
    return "", ""
  end
  local texts = {}
  local tools = {}
  for _, part in ipairs(content) do
    if type(part) == "string" then
      if part ~= "" then
        texts[#texts + 1] = part
      end
    elseif type(part) == "table" then
      local kind = part.type
      if kind == "thinking" or kind == "reasoning" then
        -- skipped: model thinking
      elseif kind == "tool_use" or kind == "toolCall" then
        local name = part.name or "tool"
        tools[#tools + 1] = recall.truncate(name .. " " .. arg_summary(part.input or part.arguments), 400)
      elseif kind == "tool_result" then
        local text = part_text(part.content or part)
        if text ~= "" then
          tools[#tools + 1] = recall.truncate(text, 400)
        end
      else
        local text = part_text(part)
        if text ~= "" then
          texts[#texts + 1] = text
        end
      end
    end
  end
  return table.concat(texts, "\n"), table.concat(tools, "\n")
end

local function session_id(st)
  local path = st.path or ""
  local name = path:match("([^/]+)%.jsonl$") or ""
  return name:match("^([0-9a-fA-F%-]+)$") or st.id
end

return {
  id = "droid",
  kind = "line",
  roots = { "~/.factory/sessions" },
  glob = "*.jsonl",
  resume = "droid resume {id}",

  line = function(line, st)
    st.id = session_id(st)
    local t = recall.get(line, "type")
    if t == "session_start" then
      st.id = recall.get(line, "id") or st.id
      st.project = recall.get(line, "cwd") or st.project
      local title = recall.get(line, "title")
      if type(title) == "string" and title ~= "" then
        st.title = title
      end
      return nil
    end
    if t ~= "message" then
      return nil
    end
    local role = recall.get(line, "message.role") or "user"
    local ts = recall.time(recall.get(line, "timestamp") or "", "rfc3339")
    local text, tools = split_content(recall.get(line, "message.content"))
    if text ~= "" then
      if tools ~= "" then
        text = text .. "\n" .. tools
      end
      return { role = role, ts = ts, text = text }
    end
    if tools ~= "" then
      return { role = role, ts = ts, text = tools }
    end
    return nil
  end,
}

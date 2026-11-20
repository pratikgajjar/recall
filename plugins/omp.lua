-- Oh My Pi sessions at ~/.omp/agent/sessions/<cwd>/*_<id>.jsonl.
-- Same one-event-per-line limit as droid: text parts are joined, tool calls appended.

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
  if type(part.text) == "string" and part.text ~= "" then
    return part.text
  end
  if type(part.content) ~= "table" then
    return ""
  end
  local bits = {}
  for _, inner in ipairs(part.content) do
    if type(inner) == "table" and type(inner.text) == "string" and inner.text ~= "" then
      bits[#bits + 1] = inner.text
    end
  end
  return table.concat(bits, "\n")
end

local function split_content(content)
  if type(content) ~= "table" then
    if type(content) == "string" then
      return content, ""
    end
    return "", ""
  end
  local texts = {}
  local tools = {}
  for _, part in ipairs(content) do
    if type(part) ~= "table" then
      -- non-table parts are not omp's wire shape
    elseif part.type == "thinking" or part.type == "reasoning" then
      -- skipped: model thinking
    elseif part.type == "toolCall" or part.type == "tool_use" then
      local name = part.name or "tool"
      tools[#tools + 1] = recall.truncate(name .. " " .. arg_summary(part.arguments or part.input), 400)
    else
      local text = part_text(part)
      if text ~= "" then
        if part.type == "tool_result" or part.type == "toolResult" then
          tools[#tools + 1] = recall.truncate(text, 400)
        else
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
  return name:match("_([0-9A-Za-z%-]+)$") or name:match("^([0-9a-fA-F%-]+)$") or st.id
end

return {
  id = "omp",
  kind = "line",
  roots = { "~/.omp/agent/sessions" },
  glob = "*.jsonl",
  resume = "omp --resume {id}",

  line = function(line, st)
    st.id = session_id(st)
    local t = recall.get(line, "type")
    if t == "session" then
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
    if role == "toolResult" then
      role = "tool"
    end
    local ts = recall.time(recall.get(line, "timestamp") or "", "rfc3339")
    local text, tools = split_content(recall.get(line, "message.content"))
    if text ~= "" and role ~= "tool" then
      if tools ~= "" then
        text = text .. "\n" .. tools
      end
      return { role = role, ts = ts, text = text }
    end
    local body = text ~= "" and text or tools
    if body == "" then
      return nil
    end
    return { role = role, ts = ts, text = recall.truncate(body, 400) }
  end,
}

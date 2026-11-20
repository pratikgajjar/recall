-- Index Kimi Code wire logs:
-- ~/.kimi-code/sessions/<workspace>/session_<id>/agents/<agent>/wire.jsonl
local function flatten(content)
  if type(content) == "string" then
    return content
  end
  if type(content) ~= "table" then
    return ""
  end
  local out = {}
  for _, part in ipairs(content) do
    if type(part) == "table" and type(part.text) == "string" and part.text ~= "" then
      out[#out + 1] = part.text
    end
  end
  return table.concat(out, "\n")
end

local function session_id(st)
  local path = st.path or st.dir or ""
  return path:match("session_([0-9a-fA-F%-]+)") or st.id
end

return {
  id = "kimi",
  kind = "line",
  roots = { "~/.kimi-code/sessions" },
  glob = "wire.jsonl",
  resume = "kimi -S {id}",

  line = function(line, st)
    st.id = session_id(st)
    local cwd = recall.get(line, "cwd")
    if cwd and cwd ~= "" then
      st.project = cwd
    end

    local t = recall.get(line, "type")
    if t == "turn.prompt" then
      local text = flatten(recall.get(line, "input"))
      if text == "" then
        return nil
      end
      return { role = "user", ts = recall.get(line, "time"), text = text }
    end
    if t == "context.append_message" then
      local role = recall.get(line, "message.role") or "unknown"
      local text = flatten(recall.get(line, "message.content"))
      if text == "" then
        return nil
      end
      return { role = role, ts = recall.get(line, "time"), text = text }
    end
    if t == "context.append_loop_event" then
      local et = recall.get(line, "event.type")
      if et == "content.part" and recall.get(line, "event.part.type") == "text" then
        local text = recall.get(line, "event.part.text")
        if type(text) == "string" and text ~= "" then
          return { role = "assistant", ts = recall.get(line, "time"), text = text }
        end
      end
      if et == "tool.call" then
        local name = recall.get(line, "event.name") or "tool"
        local args = recall.get(line, "event.args")
        local text = recall.truncate(name .. " " .. (type(args) == "string" and args or ""), 400)
        return { role = "tool:" .. name, ts = recall.get(line, "time"), text = text }
      end
    end
    return nil
  end,
}

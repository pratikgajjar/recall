-- DSH (DeepSeek Harness) sessions.
--
-- DSH writes each session as a multi-frame zstd JSONL file at
-- ~/.dsh/sessions/<cwd>/<session-id>/session.jsonl.zstd or session.vN.jsonl.zstd.
-- sync_dsh_sessions.sh keeps only the highest N in each directory. The Lua sandbox has no
-- I/O and no decompressor, so scripts/sync_dsh_sessions.sh decodes those logs to
-- plaintext JSONL under ~/.recall/dsh/<session-id>.jsonl and this plugin maps
-- the decoded events. A session that only grows decodes to a byte-identical
-- prefix, so recall's offset resume stays correct.
--
-- Indexed events: user/message, assistant/message, tool/call, tool/result.
-- Skipped on purpose: assistant/chunk plus the packed *-chunks lines (streaming
-- deltas whose text the assembled messages already carry) and the turn / step /
-- request / sandbox plumbing.

-- Collect every text part at any depth. tool/result nests its text one level
-- deeper than user/assistant messages, and every shape carries non-text parts
-- that must not reach the index.
local function collect_text(node, out)
  if type(node) ~= "table" then
    return
  end
  if node.type == "text" and type(node.text) == "string" then
    if node.text ~= "" then
      out[#out + 1] = node.text
    end
    return
  end
  for _, part in ipairs(node) do
    collect_text(part, out)
  end
end

local function text_of(node)
  local out = {}
  collect_text(node, out)
  return table.concat(out, "\n")
end

return {
  id = "dsh",
  kind = "line",
  roots = { "~/.recall/dsh" },
  glob = "*.jsonl",
  resume = "dsh --profile tui --resume {id}",

  line = function(line, st)
    local t = recall.get(line, "type")

    if t == "session" then
      st.id = recall.get(line, "id") or st.id
      st.project = recall.get(line, "cwd") or st.project
      st.started_at = recall.get(line, "createdAt") or st.started_at
      return nil
    end
    if t == "session/title" then
      local title = recall.get(line, "data.title")
      if type(title) == "string" and title ~= "" then
        st.title = title
      end
      return nil
    end

    local ts = recall.get(line, "time")
    if type(ts) ~= "number" then
      ts = 0
    end

    if t == "user/message" then
      local text = text_of(recall.get(line, "data.content"))
      if text == "" then
        return nil
      end
      return { role = "user", ts = ts, text = text }
    end
    if t == "assistant/message" then
      local text = text_of(recall.get(line, "data.message.content"))
      if text == "" then
        return nil
      end
      return { role = "assistant", ts = ts, text = text }
    end
    if t == "tool/call" then
      local name = recall.get(line, "data.name") or "tool"
      local args = recall.get(line, "data.arguments")
      if type(args) ~= "string" then
        args = ""
      end
      return {
        role = "tool:" .. name,
        ts = ts,
        text = recall.truncate(name .. " " .. args, 400),
      }
    end
    if t == "tool/result" then
      local text = text_of(recall.get(line, "data.message.content"))
      if text == "" then
        return nil
      end
      return { role = "tool", ts = ts, text = recall.truncate(text, 400) }
    end
    return nil
  end,
}

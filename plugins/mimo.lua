-- mimo (mimocode) sessions.
--
-- mimocode stores sessions in SQLite (~/.local/share/mimocode/mimocode.db,
-- see `mimo db path`). The Lua sandbox has no I/O and no SQLite, so
-- scripts/sync_mimo_sessions.mjs exports each session to plaintext JSONL at
-- ~/.recall/mimo/<session-id>.jsonl and this plugin maps the exported events.
-- The export is append-only (rowid watermark), so recall's offset resume stays
-- correct.
--
-- Indexed events: text (user/assistant message text) and tool (name + input
-- summary). Skipped on purpose: reasoning, step-start/step-finish, file
-- (base64 images), compaction.

return {
  id = "mimo",
  kind = "line",
  roots = { "~/.recall/mimo" },
  glob = "*.jsonl",
  resume = "mimo --session {id}",

  line = function(line, st)
    local t = recall.get(line, "type")

    if t == "session" then
      st.id = recall.get(line, "id") or st.id
      st.project = recall.get(line, "cwd") or st.project
      local title = recall.get(line, "title")
      if type(title) == "string" and title ~= "" then
        st.title = title
      end
      st.started_at = recall.get(line, "createdAt") or st.started_at
      return nil
    end

    local ts = recall.get(line, "ts")
    if type(ts) ~= "number" then
      ts = 0
    end

    if t == "text" then
      local text = recall.get(line, "text")
      if type(text) ~= "string" or text == "" then
        return nil
      end
      local role = recall.get(line, "role") or "user"
      return { role = role, ts = ts, text = text }
    end
    if t == "tool" then
      local name = recall.get(line, "name") or "tool"
      local text = recall.get(line, "text")
      if type(text) ~= "string" or text == "" then
        return nil
      end
      return { role = "tool:" .. name, ts = ts, text = text }
    end
    return nil
  end,
}

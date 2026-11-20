-- Cursor ACP sessions.
-- Each session is ~/.cursor/acp-sessions/<id>/store.db (content-addressed
-- blobs). That set does not overlap ~/.cursor/chats, which recall's built-in
-- cursor-agent source already indexes. scripts/sync_cursor_acp_sessions.mjs
-- exports append-only JSONL to ~/.recall/cursor-acp/<id>.jsonl.

return {
  id = "cursor-acp",
  kind = "line",
  roots = { "~/.recall/cursor-acp" },
  glob = "*.jsonl",
  resume = "cursor-agent --resume {id}",

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
      return { role = recall.get(line, "role") or "user", ts = ts, text = text }
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

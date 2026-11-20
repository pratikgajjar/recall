-- Antigravity CLI and IDE conversations.
-- CLI steps are protobuf blobs inside per-conversation SQLite, and the IDE
-- store is protobuf. scripts/sync_agy_sessions.mjs extracts prose to
-- ~/.recall/agy/<conversation-id>.jsonl. Role stays "text" because the blob
-- does not label user versus assistant.

return {
  id = "agy",
  kind = "line",
  roots = { "~/.recall/agy" },
  glob = "*.jsonl",
  resume = "agy --conversation {id}",

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
    if t ~= "text" then
      return nil
    end
    local text = recall.get(line, "text")
    if type(text) ~= "string" or text == "" then
      return nil
    end
    local ts = recall.get(line, "ts")
    if type(ts) ~= "number" then
      ts = 0
    end
    return { role = "text", ts = ts, text = text }
  end,
}

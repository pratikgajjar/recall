-- Devin CLI sessions.
-- Sessions live in SQLite (~/.local/share/devin/cli/sessions.db). The Lua
-- sandbox has no SQLite, so scripts/sync_devin_sessions.mjs exports append-only
-- JSONL to ~/.recall/devin/<session-id>.jsonl.

return {
  id = "devin",
  kind = "line",
  roots = { "~/.recall/devin" },
  glob = "*.jsonl",
  resume = "devin --resume {id}",

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

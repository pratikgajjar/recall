-- obsidian.lua — index an Obsidian vault (or any folder of Markdown notes).
--
-- Proof that recall is not chat-specific: the same contract indexes a knowledge
-- base. Each note becomes one record whose text is the note body; recall's FTS
-- makes the vault searchable alongside your agent transcripts. Nothing in Go
-- changed — this is a pure Lua transform.

return {
  id = "obsidian",
  kind = "file",
  roots = { "~/Documents/Obsidian", "~/obsidian", "~/notes" },
  glob = "*.md",
  resume = "obsidian://open?path={id}",

  file = function(doc)
    -- Title: first ATX heading, else the file name.
    local title = doc.basename
    for _, ln in ipairs(recall.lines(doc.text)) do
      local h = ln:match("^#%s+(.+)$")
      if h then
        title = h
        break
      end
    end
    local session = {
      id = doc.path,
      project = doc.dir,
      title = title,
      started_at = doc.mtime,
      ended_at = doc.mtime,
    }
    -- A note is a single "document" record. (A knowledge-graph plugin could
    -- instead emit one record per node, or split on headings.)
    local messages = { { role = "note", ts = doc.mtime, text = doc.text } }
    return session, messages
  end,
}

-- memory.lua — index pi's topic-based persistent memories.
-- MEMORY.md is deliberately skipped: it is a generated index, not source data.

return {
  id = "memory",
  kind = "file",
  roots = { "~/.pi/agent/memory" },
  glob = "*.md",
  resume = "file://{id}",

  file = function(doc)
    if doc.name == "MEMORY.md" then
      return nil, nil
    end

    local title = doc.basename
    local frontmatter = doc.text:match("^%-%-%-\n(.-)\n%-%-%-")
    if frontmatter then
      for line in frontmatter:gmatch("[^\n]+") do
        local key, value = line:match("^([%w_-]+):%s*(.+)$")
        if key == "name" then
          title = value
          break
        end
      end
    end

    local session = {
      id = doc.path,
      project = doc.dir,
      title = title,
      started_at = doc.mtime,
      ended_at = doc.mtime,
    }
    local messages = { { role = "memory", ts = doc.mtime, text = doc.text } }
    return session, messages
  end,
}

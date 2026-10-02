-- skill.lua — index Pi's active skill catalog for FTS-backed discovery.
-- The Pi extension mirrors its aggregated skill list into this cache, preserving
-- each original SKILL.md path as the stable source id.

return {
  id = "skill",
  kind = "file",
  roots = { "~/.cache/pi-skill-search/catalog" },
  glob = "*.md",
  resume = "file://{id}",

  file = function(doc)
    local title = doc.text:match("^#%s+([^\n]+)") or doc.basename
    local source = doc.text:match("\nSource:%s*([^\n]+)") or doc.path
    local base = doc.text:match("\nBase:%s*([^\n]+)") or doc.dir

    local session = {
      id = source,
      project = base,
      title = title,
      started_at = doc.mtime,
      ended_at = doc.mtime,
    }
    local messages = { { role = "skill", ts = doc.mtime, text = doc.text } }
    return session, messages
  end,
}

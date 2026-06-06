package main

import (
	"embed"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// embeddedPlugins bundles the shipped plugins into the binary so users can
// `recall plugin install <name>` without cloning the repo. They are NOT
// auto-loaded — defaultAdapters only reads ~/.recall/plugins — so installing
// recall never silently scans a vault (obsidian) or shadows a built-in Go
// adapter (cursor).
//
//go:embed plugins/*.lua
var embeddedPlugins embed.FS

// embeddedPluginNames lists the bundled plugin ids (file stem), sorted.
func embeddedPluginNames() []string {
	entries, _ := fs.ReadDir(embeddedPlugins, "plugins")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".lua"))
	}
	sort.Strings(names)
	return names
}

// readEmbeddedPlugin returns the source of a bundled plugin by name (no .lua).
func readEmbeddedPlugin(name string) ([]byte, error) {
	return embeddedPlugins.ReadFile(filepath.ToSlash(filepath.Join("plugins", name+".lua")))
}

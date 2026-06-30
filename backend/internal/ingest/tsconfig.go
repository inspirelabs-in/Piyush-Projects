package ingest

import (
	"io/fs"
	"os"
	"path/filepath"

	"project-synapse/backend/internal/parser"
)

// loadTSConfigAliases finds tsconfig.json / jsconfig.json files under root and
// extracts their path-alias rules (e.g. `@/* -> ./*`), so TS/JS imports made
// through those aliases resolve to real files during ingestion instead of
// looking like external packages.
func loadTSConfigAliases(absRoot string) []parser.AliasRule {
	var rules []parser.AliasRule
	_ = filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", ".next", "dist", "build", ".synapse-clones", "vendor", "target":
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != "tsconfig.json" && d.Name() != "jsconfig.json" {
			return nil
		}
		content, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(absRoot, filepath.Dir(p))
		if rerr != nil {
			return nil
		}
		dir := filepath.ToSlash(rel)
		if dir == "." {
			dir = ""
		}
		rules = append(rules, parser.ParseTSConfigPaths(dir, content)...)
		return nil
	})
	return rules
}

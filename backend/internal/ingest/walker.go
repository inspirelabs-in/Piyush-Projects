package ingest

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// supportedExtensions are the source files the ingestion engine parses.
// TypeScript/JavaScript go through the Node tsc-AST subprocess; Go and Rust are
// parsed in-process (see internal/parser).
var supportedExtensions = map[string]bool{
	".ts":  true,
	".tsx": true,
	".js":  true,
	".jsx": true,
	// Go + Rust.
	".go": true,
	".rs": true,
	// Markdown ("myelin" human docs) — ingested as language=markdown.
	".md":       true,
	".markdown": true,
	".mdx":      true,
}

// skipDirs are directory names we never descend into during a walk. These are
// either dependency caches or build artefacts and would otherwise dominate the
// file count without contributing first-party structure.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	".next":        true,
	"dist":         true,
	"build":        true,
	"coverage":     true,
	"out":          true,
	"vendor":       true, // Go vendored deps
	"target":       true, // Rust/Cargo build output
}

// maxFileBytes skips oversized files — almost always minified/generated bundles
// (e.g. drawio's app.min.js), which explode into thousands of junk chunks and
// stall the CPU embedder. Real first-party source is rarely this large.
const maxFileBytes = 600 * 1024

// isGenerated reports a minified/bundled/generated artefact we should not ingest.
func isGenerated(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, ".min.") ||
		strings.Contains(n, ".bundle.") ||
		strings.HasSuffix(n, "-min.js") ||
		strings.HasSuffix(n, ".min.js") ||
		strings.HasSuffix(n, ".d.ts") || // generated type declarations
		strings.HasSuffix(n, ".pb.go") || // protobuf-generated Go
		strings.HasSuffix(n, "_generated.go")
}

// Walk enumerates supported source files under root *sequentially* (depth-first
// via filepath.WalkDir) and sends each discovered relative path into out. It
// closes out when the traversal completes so downstream consumers can range
// over it. Errors reading individual entries are skipped so one unreadable
// directory does not abort the whole ingest.
func Walk(root string, out chan<- string) error {
	defer close(out)

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entry — skip it rather than aborting the walk.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		if !supportedExtensions[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}

		// Skip minified/generated bundles and oversized files — they stall the
		// pipeline (one huge file = thousands of chunks to embed on CPU).
		if isGenerated(d.Name()) {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Size() > maxFileBytes {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		out <- rel
		return nil
	})
}

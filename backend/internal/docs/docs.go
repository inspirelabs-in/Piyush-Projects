// Package docs generates hybrid repository documentation: LLM-written narrative
// sections (Introduction, Architecture) plus auto-derived module/function
// reference sections built straight from the parsed AST. Results are cached per
// repo root until a refresh is requested.
package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"project-synapse/backend/internal/llm"
	"project-synapse/backend/internal/store"
)

// FileDoc is one file's reference entry: its path and symbol-level functions.
type FileDoc struct {
	Path      string              `json:"path"`
	Functions []store.FunctionRow `json:"functions"`
}

// Section is one documentation page. Kind "narrative" carries markdown Content;
// kind "module" carries Files (the auto-derived reference).
type Section struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Kind    string    `json:"kind"`  // narrative | module
	Group   string    `json:"group"` // sidebar grouping
	Content string    `json:"content,omitempty"`
	Files   []FileDoc `json:"files,omitempty"`
}

// Docs is the full generated documentation for one repo.
type Docs struct {
	Repo     string    `json:"repo"` // root_path
	Name     string    `json:"name"`
	Sections []Section `json:"sections"`
}

// Engine builds documentation from the store + an (optional) LLM.
type Engine struct {
	Store *store.Store
	Chat  llm.ChatClient // nil => deterministic fallback narrative

	mu    sync.Mutex
	cache map[string]*Docs
}

// narrativeDoc is the structured set of LLM-written documentation pages.
type narrativeDoc struct {
	Introduction string `json:"introduction"`
	Architecture string `json:"architecture"`
	Concepts     string `json:"concepts"`
	DataFlow     string `json:"data_flow"`
}

const narrativeSystem = `You are a staff engineer writing the OFFICIAL documentation site for a codebase, read by engineers who will work in it. You receive a structured summary: the file tree, external dependencies (the tech stack), exported symbols per file, and HTTP endpoints. Document grounded ONLY in that summary — never invent files, routes, commands, or features.

Respond with ONE JSON object, nothing else:
{
  "introduction": "markdown",
  "architecture": "markdown",
  "concepts": "markdown",
  "data_flow": "markdown"
}

Write like a high-quality technology documentation site: precise, concrete, skimmable. Per section:
- introduction: what this project IS and the problem it solves, then a "## Capabilities" bullet list, then the tech stack inferred from the dependencies. 2-3 tight paragraphs total.
- architecture: the layers/subsystems and how they fit together. Use "##" subheadings per subsystem, name the REAL folders/files in ` + "`code`" + ` spans, and state each one's responsibility and which others it depends on. Include a compact markdown table with columns Layer | Location | Responsibility.
- concepts: the 3-6 most important domain concepts, types, or abstractions a contributor must understand. Each as "### Name" + a 1-2 sentence precise definition grounded in the actual symbols.
- data_flow: trace one or two REAL end-to-end paths through the system (e.g. an HTTP request from entry point to data layer, or the ingestion pipeline) as a numbered list, naming the actual files/functions at each hop.

Reference real paths and symbols in ` + "`code`" + ` spans. Be specific — never use filler. If a section cannot be grounded in the summary, return an empty string for it. Output valid JSON only — no prose, no code fences around the JSON.`

// Generate returns the documentation for one repo root, building (and caching)
// it on first request. refresh forces regeneration.
func (e *Engine) Generate(ctx context.Context, root string, refresh bool) (*Docs, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("repo is required")
	}

	e.mu.Lock()
	if e.cache == nil {
		e.cache = map[string]*Docs{}
	}
	if !refresh {
		if d, ok := e.cache[root]; ok {
			e.mu.Unlock()
			return d, nil
		}
	}
	e.mu.Unlock()

	files, err := e.Store.FilesByRoot(ctx, root)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files found for repo")
	}
	rels, _ := e.Store.RelationshipsByRoot(ctx, root)
	name := repoName(root)

	nd := e.narrative(ctx, name, files, rels)
	sections := []Section{
		{ID: "introduction", Title: "Introduction", Kind: "narrative", Group: "Overview", Content: nd.Introduction},
		{ID: "architecture", Title: "Architecture", Kind: "narrative", Group: "Overview", Content: nd.Architecture},
	}
	if strings.TrimSpace(nd.Concepts) != "" {
		sections = append(sections, Section{ID: "concepts", Title: "Core Concepts", Kind: "narrative", Group: "Overview", Content: nd.Concepts})
	}
	if strings.TrimSpace(nd.DataFlow) != "" {
		sections = append(sections, Section{ID: "data-flow", Title: "Data Flow", Kind: "narrative", Group: "Overview", Content: nd.DataFlow})
	}

	// Auto-derived reference: one section per top-level folder.
	byFolder := map[string][]store.FileRow{}
	var order []string
	for _, f := range files {
		top := topFolder(f.FilePath)
		if _, ok := byFolder[top]; !ok {
			order = append(order, top)
		}
		byFolder[top] = append(byFolder[top], f)
	}
	sort.Strings(order)
	for _, folder := range order {
		fs := byFolder[folder]
		sort.Slice(fs, func(i, j int) bool { return fs[i].FilePath < fs[j].FilePath })
		var fileDocs []FileDoc
		for _, f := range fs {
			fns, _ := e.Store.FileFunctions(ctx, root, f.FilePath)
			fileDocs = append(fileDocs, FileDoc{Path: f.FilePath, Functions: fns})
		}
		title := folder + "/"
		if folder == rootFolder {
			title = "(root)"
		}
		sections = append(sections, Section{
			ID:    "module-" + slug(folder),
			Title: title,
			Kind:  "module",
			Group: "Reference",
			Files: fileDocs,
		})
	}

	d := &Docs{Repo: root, Name: name, Sections: sections}
	e.mu.Lock()
	e.cache[root] = d
	e.mu.Unlock()
	return d, nil
}

func (e *Engine) narrative(ctx context.Context, name string, files []store.FileRow, rels []store.RelRow) narrativeDoc {
	fb := narrativeDoc{Introduction: fallbackIntro(name, files), Architecture: fallbackArch(name, files, rels)}
	if e.Chat == nil {
		return fb
	}
	raw, err := e.Chat.Complete(ctx, narrativeSystem, buildSummary(name, files, rels))
	if err != nil {
		return fb
	}
	var parsed narrativeDoc
	if err := json.Unmarshal([]byte(extractJSON(raw)), &parsed); err != nil || strings.TrimSpace(parsed.Introduction) == "" {
		return fb
	}
	// Repair models that double-escape newlines inside the JSON strings.
	parsed.Introduction = llm.CleanMarkdown(parsed.Introduction)
	parsed.Architecture = llm.CleanMarkdown(parsed.Architecture)
	parsed.Concepts = llm.CleanMarkdown(parsed.Concepts)
	parsed.DataFlow = llm.CleanMarkdown(parsed.DataFlow)
	return parsed
}

// buildSummary renders a compact, bounded summary of the repo for the LLM.
func buildSummary(name string, files []store.FileRow, rels []store.RelRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s\n\nFiles (%d):\n", name, len(files))
	for i, f := range files {
		if i >= 120 {
			fmt.Fprintf(&b, "- …and %d more\n", len(files)-i)
			break
		}
		fmt.Fprintf(&b, "- %s\n", f.FilePath)
	}

	var endpoints, exportsBySrc = []string{}, map[string][]string{}
	extDeps := map[string]bool{}
	for _, r := range rels {
		switch r.RelationshipType {
		case "endpoint":
			endpoints = append(endpoints, fmt.Sprintf("%s  (in %s)", r.TargetSymbol, r.SourceSymbol))
		case "exports":
			exportsBySrc[r.SourceSymbol] = append(exportsBySrc[r.SourceSymbol], r.TargetSymbol)
		case "imports":
			if ext, _ := r.Metadata["external"].(bool); ext {
				if spec, _ := r.Metadata["specifier"].(string); spec != "" {
					extDeps[spec] = true
				}
			}
		}
	}
	if len(extDeps) > 0 {
		deps := make([]string, 0, len(extDeps))
		for d := range extDeps {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		if len(deps) > 50 {
			deps = deps[:50]
		}
		fmt.Fprintf(&b, "\nExternal dependencies (tech-stack signal): %s\n", strings.Join(deps, ", "))
	}
	if len(endpoints) > 0 {
		b.WriteString("\nHTTP endpoints:\n")
		for i, ep := range endpoints {
			if i >= 50 {
				break
			}
			fmt.Fprintf(&b, "- %s\n", ep)
		}
	}
	if len(exportsBySrc) > 0 {
		b.WriteString("\nExported symbols by file:\n")
		srcs := make([]string, 0, len(exportsBySrc))
		for s := range exportsBySrc {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		for i, s := range srcs {
			if i >= 60 {
				break
			}
			syms := exportsBySrc[s]
			if len(syms) > 10 {
				syms = syms[:10]
			}
			fmt.Fprintf(&b, "- %s: %s\n", s, strings.Join(syms, ", "))
		}
	}
	return b.String()
}

func fallbackIntro(name string, files []store.FileRow) string {
	return fmt.Sprintf("# %s\n\nAuto-generated documentation for **%s** — %d source files. "+
		"Configure an LLM provider (Anthropic / OpenAI / OpenRouter / Ollama) for a written overview. "+
		"The Reference section below is derived directly from the parsed code.", name, name, len(files))
}

func fallbackArch(name string, files []store.FileRow, rels []store.RelRow) string {
	byFolder := map[string]int{}
	for _, f := range files {
		byFolder[topFolder(f.FilePath)]++
	}
	folders := make([]string, 0, len(byFolder))
	for f := range byFolder {
		folders = append(folders, f)
	}
	sort.Strings(folders)
	var b strings.Builder
	b.WriteString("## Structure\n\n")
	for _, f := range folders {
		label := f + "/"
		if f == rootFolder {
			label = "(root)"
		}
		fmt.Fprintf(&b, "- `%s` — %d files\n", label, byFolder[f])
	}
	var endpoints int
	for _, r := range rels {
		if r.RelationshipType == "endpoint" {
			endpoints++
		}
	}
	if endpoints > 0 {
		fmt.Fprintf(&b, "\nExposes **%d HTTP endpoint(s)**.\n", endpoints)
	}
	return b.String()
}

// --- helpers ----------------------------------------------------------------

const rootFolder = "(root)"

func topFolder(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return rootFolder
}

func repoName(root string) string {
	r := strings.TrimRight(root, `/\`)
	if i := strings.LastIndexAny(r, `/\`); i >= 0 {
		return r[i+1:]
	}
	return r
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// extractJSON pulls the first {...} object out of a model response.
func extractJSON(raw string) string {
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end <= start {
		return raw
	}
	return raw[start : end+1]
}

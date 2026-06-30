package bugs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"project-synapse/backend/internal/llm"
	"project-synapse/backend/internal/store"
)

// target is one file selected for deep (Tier-2) analysis, with why it was picked.
type target struct {
	file   string
	reason string
}

// pickTargets ranks files by risk for the (token-bounded) LLM pass: Tier-1
// suspects first, then HTTP entry points, then high-fan-in hubs.
func (e *Engine) pickTargets(g *graph, funcs []store.FuncCodeRow, found []Bug) []target {
	maxN := e.MaxLLM
	if maxN <= 0 {
		maxN = 8
	}
	score := map[string]int{}
	reason := map[string]string{}
	bump := func(file string, pts int, why string) {
		if file == "" {
			return
		}
		score[file] += pts
		if _, ok := reason[file]; !ok {
			reason[file] = why
		}
	}
	for _, b := range found {
		bump(b.Location.File, 5, "flagged by a deterministic scan ("+b.Category+")")
	}
	for f := range g.endpoints {
		bump(f, 4, "an HTTP entry point (handles external input)")
	}
	for f, imps := range g.importers {
		if len(imps) >= 3 {
			bump(f, 2, fmt.Sprintf("high fan-in (%d importers)", len(imps)))
		}
	}

	hasCode := map[string]bool{}
	for _, r := range funcs {
		hasCode[r.File] = true
	}
	var ts []target
	for f := range score {
		if hasCode[f] {
			ts = append(ts, target{file: f, reason: reason[f]})
		}
	}
	sort.SliceStable(ts, func(i, j int) bool {
		if score[ts[i].file] != score[ts[j].file] {
			return score[ts[i].file] > score[ts[j].file]
		}
		return ts[i].file < ts[j].file
	})
	if len(ts) > maxN {
		ts = ts[:maxN]
	}
	return ts
}

// buildContext assembles the dense payload for one target: its code, its direct
// dependencies' signatures, and pgvector-similar code/doc chunks from the repo.
func (e *Engine) buildContext(ctx context.Context, root string, t target, g *graph, byFile map[string][]store.FuncCodeRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TARGET FILE: %s\nWhy flagged: %s\n\n", t.file, t.reason)

	// The file head (imports + top-level declarations) tells the model where
	// symbols come from — without it, aliased imports like `import { X as Y }`
	// read as "undefined variable" and produce false positives.
	if head := fileHead(root, t.file, 50); head != "" {
		b.WriteString("=== FILE HEAD (imports & top-level declarations; symbols below resolve against these) ===\n")
		b.WriteString(truncate(head, 2600))
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "=== TARGET CODE (%s) ===\n", baseName(t.file))
	b.WriteString(truncate(concatCode(byFile[t.file]), 6000))

	deps := uniqueStrings(g.importsOf[t.file])
	if len(deps) > 0 {
		b.WriteString("\n\n=== DIRECT DEPENDENCIES (signatures) ===\n")
		for i, d := range deps {
			if i >= 6 {
				break
			}
			fmt.Fprintf(&b, "// %s\n%s\n", d, signatures(byFile[d], 6))
		}
	}

	// pgvector similarity: pull related code + docs the target may interact with.
	if e.Embedder != nil {
		if vecs, err := e.Embedder.Embed(ctx, []string{truncate(concatCode(byFile[t.file]), 4000)}); err == nil && len(vecs) > 0 {
			if hits, herr := e.Store.VectorSearch(ctx, vecs[0], 6, root); herr == nil {
				wrote := false
				for _, h := range hits {
					if h.FilePath == t.file {
						continue
					}
					if !wrote {
						b.WriteString("\n\n=== RELATED CODE / DOCS (semantic search) ===\n")
						wrote = true
					}
					fmt.Fprintf(&b, "\n[%s :: %s]\n%s\n", h.FilePath, h.SymbolName, truncate(store.StripChunkHeader(h.Content), 700))
				}
			}
		}
	}
	return b.String()
}

const redTeamSystem = `You are an adversarial staff security + reliability engineer red-teaming ONE code unit for a CRITICAL, CONCRETE defect. You receive the target file's head (imports + top-level declarations), the target code, its direct dependencies, and related code from the same repository.

Hunt for REAL, specific defects only: logic errors, unhandled errors / nil or null dereferences, race conditions & unsafe concurrency, resource leaks, injection / auth / SSRF / path-traversal, broken invariants, or unhandled states. Ignore style, naming, and purely hypothetical issues. Ground every claim in the code shown — cite the exact symbol.

CRITICAL — you are shown EXTRACTED snippets, not the whole program. You CANNOT see every definition. Therefore you MUST NOT report symbol-resolution issues: do NOT claim a variable/function/type is "undefined", "not defined in the provided code", "missing import", "not declared", or "not in scope". Assume any symbol you cannot see is correctly imported or defined elsewhere. Note that ` + "`import { A as B }`" + ` defines B, and ` + "`import X`" + ` / ` + "`const { x } = …`" + ` define their bindings. Only report a defect when it is provable from the code actually shown — if you are not highly confident it is a real bug, set "found": false.

Respond with ONE JSON object, nothing else:
{
  "found": true,
  "title": "short, specific",
  "severity": "CRITICAL|HIGH|MEDIUM",
  "category": "logic|concurrency|security|resource_leak|error_handling",
  "location": { "file": "exact/path", "line_start": 0, "line_end": 0, "entity": "function or type name" },
  "finding": { "issue": "what is wrong, citing the code", "impact": "what breaks in production", "fix": "the concrete change" }
}
Set "found": false (and leave the other fields empty) if there is no real, high-confidence defect — do NOT invent one. Never fabricate code or paths not shown. Output valid JSON only — no prose, no code fences.`

// adversarial runs the red-team prompt over one target's context and returns a
// validated Bug if a real defect was found.
func (e *Engine) adversarial(ctx context.Context, root string, t target, g *graph, funcs []store.FuncCodeRow) (Bug, bool) {
	byFile := map[string][]store.FuncCodeRow{}
	for _, r := range funcs {
		byFile[r.File] = append(byFile[r.File], r)
	}
	payload := e.buildContext(ctx, root, t, g, byFile)

	raw, err := e.Chat.Complete(ctx, redTeamSystem, payload)
	if err != nil {
		return Bug{}, false
	}
	var p struct {
		Found    bool     `json:"found"`
		Title    string   `json:"title"`
		Severity string   `json:"severity"`
		Category string   `json:"category"`
		Location Location `json:"location"`
		Finding  Finding  `json:"finding"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(raw)), &p); err != nil {
		return Bug{}, false
	}
	if !p.Found || strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Finding.Issue) == "" {
		return Bug{}, false
	}
	sev := strings.ToUpper(strings.TrimSpace(p.Severity))
	if _, ok := severityRank[sev]; !ok {
		sev = "MEDIUM"
	}
	loc := p.Location
	if loc.File == "" {
		loc.File = t.file
	}
	cat := strings.TrimSpace(p.Category)
	if cat == "" {
		cat = "logic"
	}
	return Bug{
		Title:    strings.TrimSpace(p.Title),
		Severity: sev,
		Category: cat,
		Tier:     "llm",
		Location: loc,
		Finding: Finding{
			Issue:  llm.CleanMarkdown(p.Finding.Issue),
			Impact: llm.CleanMarkdown(p.Finding.Impact),
			Fix:    llm.CleanMarkdown(p.Finding.Fix),
		},
		ContextNodes: []string{t.file},
	}, true
}

// --- small text helpers -----------------------------------------------------

// fileHead reads the top of a source file (imports + top-level declarations)
// from the local repo. Best-effort: returns "" if the file can't be read.
func fileHead(root, rel string, maxLines int) string {
	if root == "" || rel == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func concatCode(rows []store.FuncCodeRow) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.Code)
		b.WriteByte('\n')
	}
	return b.String()
}

func signatures(rows []store.FuncCodeRow, n int) string {
	var b strings.Builder
	count := 0
	for _, r := range rows {
		sig := firstCodeLine(r.Code)
		if sig == "" {
			continue
		}
		b.WriteString("  " + truncate(sig, 120) + "\n")
		if count++; count >= n {
			break
		}
	}
	return b.String()
}

func firstCodeLine(code string) string {
	for _, ln := range strings.Split(code, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "#") {
			continue
		}
		return t
	}
	return ""
}

func truncate(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "\n…(truncated)"
	}
	return s
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

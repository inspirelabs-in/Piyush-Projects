// Package bugs is a two-tier critical-bug + anti-pattern detector over the AST
// graph + semantic layer.
//
//	Tier 1 (deterministic, no LLM): graph/code scans run in Go over the
//	relational data — circular dependencies (Tarjan SCC over the import graph) and
//	resource-leak heuristics (allocate-without-release in a function body). These
//	are cheap and run on every scan. (Dead/unreachable code is detected by the
//	sibling `prune` engine and surfaced in the same dashboard, so it isn't
//	duplicated here.)
//
//	Tier 2 (semantic, LLM): for the riskiest nodes, a dense context payload is
//	assembled — the node's code, its direct dependencies, and pgvector-similar
//	chunks — and handed to an adversarial red-teamer prompt that emits findings in
//	a strict JSON schema. Capped + cached to control token cost.
package bugs

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"project-synapse/backend/internal/embed"
	"project-synapse/backend/internal/llm"
	"project-synapse/backend/internal/store"
)

// Location pins a finding to source.
type Location struct {
	File      string `json:"file"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Entity    string `json:"entity"`
}

// Finding is the issue / impact / fix triple.
type Finding struct {
	Issue  string `json:"issue"`
	Impact string `json:"impact"`
	Fix    string `json:"fix"`
}

// Bug is one detected defect (Tier-1 deterministic or Tier-2 LLM).
type Bug struct {
	BugID        string   `json:"bug_id"`   // SYN-YYYY-XXX
	Title        string   `json:"title"`
	Severity     string   `json:"severity"` // CRITICAL | HIGH | MEDIUM
	Category     string   `json:"category"` // circular_dependency | resource_leak | logic | concurrency | security | ...
	Tier         string   `json:"tier"`     // deterministic | llm
	Location     Location `json:"location"`
	Finding      Finding  `json:"finding"`
	ContextNodes []string `json:"context_nodes"`
}

// Report is the full scan for a repo.
type Report struct {
	Repo    string         `json:"repo"`
	Name    string         `json:"name"`
	Scanned int            `json:"scanned"` // code files scanned
	Bugs    []Bug          `json:"bugs"`
	Summary map[string]int `json:"summary"` // counts by severity
	Notes   []string       `json:"notes"`
}

// Engine runs the two-tier scan. Embedder + Chat enable Tier 2; nil => Tier 1 only.
type Engine struct {
	Store    *store.Store
	Embedder embed.Embedder
	Chat     llm.ChatClient
	LLM      bool // enable Tier 2
	MaxLLM   int  // cap on Tier-2 targets (token budget)

	mu    sync.Mutex
	cache map[string]*Report
}

// Scan runs (and caches) the full analysis for a repo root.
func (e *Engine) Scan(ctx context.Context, root string, refresh bool) (*Report, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("repo is required")
	}
	e.mu.Lock()
	if e.cache == nil {
		e.cache = map[string]*Report{}
	}
	if !refresh {
		if r, ok := e.cache[root]; ok {
			e.mu.Unlock()
			return r, nil
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
	funcs, _ := e.Store.FunctionsWithCodeByRoot(ctx, root)

	g := buildGraph(files, rels)
	rep := &Report{Repo: root, Name: repoName(root), Scanned: g.codeCount, Summary: map[string]int{}}
	seq := 0
	add := func(b Bug) {
		seq++
		b.BugID = fmt.Sprintf("SYN-%d-%03d", time.Now().Year(), seq)
		rep.Bugs = append(rep.Bugs, b)
		rep.Summary[b.Severity]++
	}

	// --- Tier 1: deterministic graph + code scans ----------------------------
	for _, c := range detectCycles(g) {
		add(c)
	}
	for _, l := range detectResourceLeaks(funcs) {
		add(l)
	}

	tier1 := len(rep.Bugs)

	// --- Tier 2: adversarial LLM analysis of the riskiest nodes --------------
	analyzed := 0
	if e.LLM && e.Chat != nil {
		targets := e.pickTargets(g, funcs, rep.Bugs)
		analyzed = len(targets)
		for _, t := range targets {
			if b, ok := e.adversarial(ctx, root, t, g, funcs); ok {
				add(b)
			}
		}
	}

	sortBugs(rep.Bugs)
	rep.Notes = g.notes(e.LLM && e.Chat != nil)
	if rep.Bugs == nil {
		rep.Bugs = []Bug{}
	}
	log.Printf("bugs: %s — %d findings (tier1=%d, tier2=%d from %d nodes analyzed) over %d files",
		rep.Name, len(rep.Bugs), tier1, len(rep.Bugs)-tier1, analyzed, rep.Scanned)

	e.mu.Lock()
	e.cache[root] = rep
	e.mu.Unlock()
	return rep, nil
}

// --- graph ------------------------------------------------------------------

type graph struct {
	codeFiles []string
	codeCount int
	lang      map[string]string
	importsOf map[string][]string // file -> internal files it imports
	importers map[string][]string // file -> files importing it
	endpoints map[string]bool     // file has an HTTP endpoint
}

func buildGraph(files []store.FileRow, rels []store.RelRow) *graph {
	g := &graph{
		lang: map[string]string{}, importsOf: map[string][]string{},
		importers: map[string][]string{}, endpoints: map[string]bool{},
	}
	isCode := map[string]bool{}
	for _, f := range files {
		g.lang[f.FilePath] = f.Language
		if f.Language == "markdown" {
			continue
		}
		isCode[f.FilePath] = true
		g.codeFiles = append(g.codeFiles, f.FilePath)
	}
	sort.Strings(g.codeFiles)
	g.codeCount = len(g.codeFiles)

	seen := map[string]bool{}
	for _, r := range rels {
		switch r.RelationshipType {
		case "imports":
			if ext, _ := r.Metadata["external"].(bool); ext {
				continue
			}
			s, t := r.SourceSymbol, r.TargetSymbol
			if !isCode[s] || !isCode[t] || s == t {
				continue
			}
			key := s + ">" + t
			if seen[key] {
				continue
			}
			seen[key] = true
			g.importsOf[s] = append(g.importsOf[s], t)
			g.importers[t] = append(g.importers[t], s)
		case "endpoint":
			if isCode[r.SourceSymbol] {
				g.endpoints[r.SourceSymbol] = true
			}
		}
	}
	return g
}

func (g *graph) notes(llmOn bool) []string {
	n := []string{
		"Findings are candidates: deterministic scans are heuristic and the adversarial LLM can be wrong — review before acting.",
	}
	if !llmOn {
		n = append(n, "Tier 2 (LLM adversarial analysis) is disabled — only deterministic graph/code scans ran. Configure an LLM provider + SYNAPSE_BUGS_LLM=true for deep logic/security review.")
	}
	return n
}

// --- Tier 1a: circular dependencies (Tarjan SCC) ----------------------------

func detectCycles(g *graph) []Bug {
	comps := tarjanSCC(g.codeFiles, g.importsOf)
	var bugs []Bug
	for _, comp := range comps {
		if len(comp) < 2 {
			continue // single node = no cycle (self-imports already filtered)
		}
		sort.Strings(comp)
		b := Bug{
			Severity: "HIGH",
			Category: "circular_dependency",
			Tier:     "deterministic",
			Location: Location{File: comp[0], Entity: "module import cycle"},
			Finding: Finding{
				Impact: "Cycles cause fragile initialization order, hinder tree-shaking/testing, and can deadlock or leak at startup. In Go they won't compile; in TS/JS they yield undefined-at-import-time bugs.",
				Fix:    "Break the loop by extracting the shared types/utilities into a leaf module that all of these import, or invert one dependency (depend on an interface, not a concrete module).",
			},
			ContextNodes: comp,
		}
		if len(comp) <= 8 {
			b.Title = fmt.Sprintf("Circular dependency across %d files", len(comp))
			b.Finding.Issue = "These files form an import cycle: " + cycleArrows(comp) + "."
		} else {
			// A large strongly-connected component is a tangle, not a clean loop —
			// report it as one cluster with a capped sample, not 100+ nodes.
			b.Title = fmt.Sprintf("Large circular-dependency tangle (%d files)", len(comp))
			b.Finding.Issue = fmt.Sprintf("%d files are mutually entangled in one strongly-connected import cluster — from any of them you can reach any other and loop back. Worst offenders: %s.",
				len(comp), strings.Join(shortNames(comp, 10), ", "))
			b.Finding.Fix = "Untangle incrementally: find the few edges whose removal would split the cluster (often a barrel `index` file or a shared context that imports its own consumers), and invert or break those first."
			b.ContextNodes = comp[:15]
		}
		bugs = append(bugs, b)
	}
	return bugs
}

func cycleArrows(files []string) string {
	short := make([]string, len(files))
	for i, f := range files {
		short[i] = baseName(f)
	}
	return strings.Join(short, " → ") + " → " + short[0]
}

func shortNames(files []string, n int) []string {
	if len(files) > n {
		files = files[:n]
	}
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = baseName(f)
	}
	return out
}

// tarjanSCC returns the strongly-connected components of the directed graph.
func tarjanSCC(nodes []string, adj map[string][]string) [][]string {
	index := 0
	idx := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var comps [][]string

	var strong func(v string)
	strong = func(v string) {
		idx[v] = index
		low[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if _, ok := idx[w]; !ok {
				strong(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] && idx[w] < low[v] {
				low[v] = idx[w]
			}
		}
		if low[v] == idx[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			comps = append(comps, comp)
		}
	}
	for _, v := range nodes {
		if _, ok := idx[v]; !ok {
			strong(v)
		}
	}
	return comps
}

// --- Tier 1b: resource leaks (allocate-without-release in a function) --------

type leakRule struct {
	name    string
	what    string
	alloc   *regexp.Regexp
	release *regexp.Regexp
	langs   map[string]bool
}

var goLangs = map[string]bool{"go": true}
var jsLangs = map[string]bool{"typescript": true, "javascript": true}

var leakRules = []leakRule{
	{
		name: "unclosed result set", what: "a database result set",
		// Anchor on the idiomatic `rows, err := X.Query(...)` so SQL result sets
		// match but unrelated `.Query()` methods (URL params, RAG `orch.Query`,
		// `QueryRow`) do not. Precision over recall — Tier 2 catches the rest.
		alloc:   regexp.MustCompile(`rows\b[\w ,\t]*:?=\s*[\w.]+\.Query(Context)?\(`),
		release: regexp.MustCompile(`\.Close\(`),
		langs:   goLangs,
	},
	{
		name: "unfinished transaction", what: "a database transaction",
		// Anchor on `tx, err := X.Begin(...)`. Commit/Rollback may take a ctx arg
		// (pgx), so the release pattern doesn't require empty parens.
		alloc:   regexp.MustCompile(`tx\b[\w ,\t]*:?=\s*[\w.]+\.Begin(Tx)?\(`),
		release: regexp.MustCompile(`\.Commit\(|\.Rollback\(`),
		langs:   goLangs,
	},
	{
		name: "unclosed file/handle", what: "a file handle",
		alloc:   regexp.MustCompile(`:?=\s*os\.(Open|Create|OpenFile)\(`),
		release: regexp.MustCompile(`\.Close\(`),
		langs:   goLangs,
	},
	{
		name: "leaked interval timer", what: "a timer",
		alloc:   regexp.MustCompile(`setInterval\(`),
		release: regexp.MustCompile(`clearInterval\(`),
		langs:   jsLangs,
	},
	{
		name: "unremoved event listener", what: "an event listener / subscription",
		// JS/RN has many cleanup idioms: DOM removeEventListener, RN
		// subscription.remove(), EventEmitter removeListener, NetInfo's unsubscribe()
		// fn, AbortController.abort(), or simply a `return () => …` effect cleanup.
		// Treat the presence of any of them as "handled" (precision over recall).
		alloc:   regexp.MustCompile(`\.addEventListener\(`),
		release: regexp.MustCompile(`removeEventListener\(|\.remove\(|removeListener\(|removeAllListeners\(|unsubscribe|\.abort\(|return\s*\(\s*\)\s*=>`),
		langs:   jsLangs,
	},
}

func detectResourceLeaks(funcs []store.FuncCodeRow) []Bug {
	// Reassemble whole functions (chunk splits) per file+symbol.
	type fn struct {
		file, symbol, lang, code string
		start, end               int
	}
	byKey := map[string]*fn{}
	var order []string
	for _, r := range funcs {
		base := baseSym(r.Symbol)
		key := r.File + "\x00" + base
		f, ok := byKey[key]
		if !ok {
			f = &fn{file: r.File, symbol: base, start: r.StartLine, end: r.EndLine}
			byKey[key] = f
			order = append(order, key)
		}
		f.code += "\n" + r.Code
		if r.StartLine < f.start {
			f.start = r.StartLine
		}
		if r.EndLine > f.end {
			f.end = r.EndLine
		}
	}

	var bugs []Bug
	for _, key := range order {
		f := byKey[key]
		lang := langOf(f.file)
		for _, rule := range leakRules {
			if !rule.langs[lang] {
				continue
			}
			if rule.alloc.MatchString(f.code) && !rule.release.MatchString(f.code) {
				bugs = append(bugs, Bug{
					Title:    "Possible resource leak: " + rule.name,
					Severity: "MEDIUM",
					Category: "resource_leak",
					Tier:     "deterministic",
					Location: Location{File: f.file, Entity: f.symbol, LineStart: f.start, LineEnd: f.end},
					Finding: Finding{
						Issue:  fmt.Sprintf("`%s` opens %s but no matching release (`%s`) is visible in its body.", f.symbol, rule.what, releaseHint(rule)),
						Impact: "Unreleased resources accumulate under load — exhausting the connection pool / file descriptors / memory and eventually stalling the service.",
						Fix:    "Release it on every path — `defer x.Close()` (Go) right after acquiring, or a cleanup in the effect's teardown (JS). Verify the resource isn't returned to a caller that owns closing it.",
					},
					ContextNodes: []string{f.file},
				})
				break // one leak finding per function is enough
			}
		}
	}
	return bugs
}

func releaseHint(r leakRule) string {
	switch r.name {
	case "unfinished transaction":
		return "Commit/Rollback"
	case "leaked interval timer":
		return "clearInterval"
	case "unremoved event listener":
		return "removeEventListener"
	default:
		return "Close()"
	}
}

// --- helpers ----------------------------------------------------------------

func baseSym(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i]
	}
	return s
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func langOf(file string) string {
	switch {
	case strings.HasSuffix(file, ".go"):
		return "go"
	case strings.HasSuffix(file, ".ts"), strings.HasSuffix(file, ".tsx"),
		strings.HasSuffix(file, ".js"), strings.HasSuffix(file, ".jsx"),
		strings.HasSuffix(file, ".mjs"), strings.HasSuffix(file, ".cjs"):
		// MultiParser labels these typescript/javascript; for leak rules we only
		// need the js-family bucket.
		return "typescript"
	default:
		return ""
	}
}

var severityRank = map[string]int{"CRITICAL": 0, "HIGH": 1, "MEDIUM": 2}

func sortBugs(bs []Bug) {
	sort.SliceStable(bs, func(i, j int) bool {
		if severityRank[bs[i].Severity] != severityRank[bs[j].Severity] {
			return severityRank[bs[i].Severity] < severityRank[bs[j].Severity]
		}
		return bs[i].Location.File < bs[j].Location.File
	})
}

func repoName(root string) string {
	r := strings.TrimRight(root, `/\`)
	if i := strings.LastIndexAny(r, `/\`); i >= 0 {
		return r[i+1:]
	}
	return r
}

func extractJSONObject(raw string) string {
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end <= start {
		return raw
	}
	return raw[start : end+1]
}

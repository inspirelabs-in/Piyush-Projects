package blueprint

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"project-synapse/backend/internal/embed"
	"project-synapse/backend/internal/llm"
	"project-synapse/backend/internal/store"
)

// Engine runs feature discovery: extract intents, score each against the
// codebase concurrently, and assemble the blueprint.
type Engine struct {
	Store       *store.Store
	Embedder    embed.Embedder
	Extractor   *Extractor
	Concurrency int // bounded to stay within the DB connection pool
	TopK        int
}

type target struct {
	kind string // entity | action
	name string
}

// Discover produces the reuse blueprint for a feature description, scoped to one
// repo (root == "" scores against every ingested repo).
func (e *Engine) Discover(ctx context.Context, description, root string) (*Response, error) {
	intents := e.Extractor.Extract(ctx, description)

	var targets []target
	for _, en := range intents.Entities {
		targets = append(targets, target{"entity", en.Name})
	}
	for _, ac := range intents.Actions {
		targets = append(targets, target{"action", ac.Name})
	}

	matches := make([]Match, len(targets))

	concurrency := e.Concurrency
	if concurrency <= 0 {
		concurrency = 6
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			matches[i] = e.score(ctx, targets[i], root)
		}(i)
	}
	wg.Wait()

	return assemble(description, intents, matches), nil
}

const narrateSystem = `You are a staff software architect briefing an engineer on a code-reuse analysis for a proposed feature.
Given the structured analysis (what already exists to REUSE, what to EXTEND, and what to BUILD new), write a short, decisive briefing of 3-5 sentences in GitHub-flavored markdown.
Be concrete: name the specific entities/actions and the files involved, lead with the highest-leverage reuse, and end with the net build effort. Output the markdown briefing only — no JSON, no headings.`

// StreamNarrative streams a natural-language reuse briefing for an assembled
// blueprint via onToken. It uses the LLM when configured (token-by-token when
// the provider supports streaming) and falls back to a deterministic summary.
func (e *Engine) StreamNarrative(ctx context.Context, resp *Response, onToken func(string)) error {
	chat := e.chat()
	if chat == nil {
		onToken(deterministicNarrative(resp))
		return nil
	}
	prompt := buildNarratePrompt(resp)
	if sc, ok := chat.(llm.StreamingChatClient); ok {
		_, err := sc.Stream(ctx, narrateSystem, prompt, onToken)
		return err
	}
	raw, err := chat.Complete(ctx, narrateSystem, prompt)
	if err != nil {
		return err
	}
	onToken(raw)
	return nil
}

func (e *Engine) chat() llm.ChatClient {
	if e.Extractor == nil {
		return nil
	}
	return e.Extractor.Chat
}

func buildNarratePrompt(resp *Response) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Feature description: %s\n\n", resp.Description)
	fmt.Fprintf(&b, "Reuse score: %.0f%% (%d reuse / %d extend / %d build of %d intents)\n\n",
		resp.Summary.ReuseScore*100, resp.Summary.Green, resp.Summary.Yellow, resp.Summary.Red, resp.Summary.Total)

	byCat := func(label string, cat Category) {
		var lines []string
		for _, m := range resp.Matches {
			if m.Category != cat {
				continue
			}
			files := "no direct file"
			if len(m.Files) > 0 {
				files = strings.Join(m.Files, ", ")
			}
			lines = append(lines, fmt.Sprintf("  - %s (%s) → %s", m.Name, m.Kind, files))
		}
		if len(lines) > 0 {
			fmt.Fprintf(&b, "%s:\n%s\n", label, strings.Join(lines, "\n"))
		}
	}
	byCat("REUSE (already exists)", CategoryGreen)
	byCat("EXTEND (partial coverage)", CategoryYellow)
	byCat("BUILD (missing)", CategoryRed)

	if len(resp.Gaps) > 0 {
		var g []string
		for _, gap := range resp.Gaps {
			g = append(g, fmt.Sprintf("  - %s → new file %s", gap.Label, gap.SuggestedFile))
		}
		fmt.Fprintf(&b, "\nSuggested new files:\n%s\n", strings.Join(g, "\n"))
	}
	return b.String()
}

// deterministicNarrative is the offline briefing used when no LLM is configured.
func deterministicNarrative(resp *Response) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Reuse analysis** — overall reuse score **%.0f%%** across %d intents.\n\n",
		resp.Summary.ReuseScore*100, resp.Summary.Total)
	if resp.Summary.Green > 0 {
		fmt.Fprintf(&b, "- **Reuse (%d):** existing structures already cover these capabilities.\n", resp.Summary.Green)
	}
	if resp.Summary.Yellow > 0 {
		fmt.Fprintf(&b, "- **Extend (%d):** partial coverage exists — extend the highlighted files.\n", resp.Summary.Yellow)
	}
	if resp.Summary.Red > 0 {
		fmt.Fprintf(&b, "- **Build (%d):** no existing structure — create the suggested new files.\n", resp.Summary.Red)
	}
	if resp.Summary.Green == 0 && resp.Summary.Yellow == 0 && resp.Summary.Red == 0 {
		b.WriteString("No intents were extracted from the description.")
	}
	b.WriteString("\n_(Offline summary — configure an LLM key for a full natural-language briefing.)_")
	return b.String()
}

// score runs concurrent-by-caller semantic + relational search for one intent
// and categorises it, scoped to one repo (root == "" = all repos).
func (e *Engine) score(ctx context.Context, t target, root string) Match {
	variants := termVariants(t.name)
	patterns := make([]string, 0, len(variants))
	for _, v := range variants {
		patterns = append(patterns, "%"+v+"%")
	}

	// Semantic search.
	var bestSim float64
	if vecs, err := e.Embedder.Embed(ctx, []string{t.name}); err == nil && len(vecs) > 0 {
		if hits, err := e.Store.VectorSearch(ctx, vecs[0], e.TopK, root); err == nil && len(hits) > 0 {
			bestSim = 1 - hits[0].Distance
		}
	}

	// Relational search.
	files, _ := e.Store.KeywordSearchFiles(ctx, patterns, root)
	rels, _ := e.Store.KeywordSearchRelationships(ctx, patterns, root)

	containsVariant := func(s string) bool {
		ls := strings.ToLower(s)
		for _, v := range variants {
			if strings.Contains(ls, v) {
				return true
			}
		}
		return false
	}

	// Split evidence into "structural" (a dedicated symbol/endpoint/filename
	// matches the term) vs "mention" (the term only appears in file content or
	// is merely imported/used). Green highlights use the structural set so a
	// broad content term doesn't paint the whole graph green.
	structuralSet := map[string]bool{}
	mentionSet := map[string]bool{}
	var structural, mention, evSymbols, evEndpoints []string
	exportedSymbols := []string{}
	endpointRoutes := []string{}

	addStructural := func(p string) {
		if p != "" && !structuralSet[p] {
			structuralSet[p] = true
			structural = append(structural, p)
		}
	}
	addMention := func(p string) {
		if p != "" && !mentionSet[p] {
			mentionSet[p] = true
			mention = append(mention, p)
		}
	}

	symSeen := map[string]bool{}
	for _, r := range rels {
		switch r.RelationshipType {
		case "exports":
			exportedSymbols = append(exportedSymbols, r.TargetSymbol)
			if containsVariant(r.TargetSymbol) {
				addStructural(r.SourceSymbol)
				if !symSeen[r.TargetSymbol] {
					symSeen[r.TargetSymbol] = true
					evSymbols = append(evSymbols, r.TargetSymbol)
				}
			}
		case "endpoint":
			endpointRoutes = append(endpointRoutes, r.TargetSymbol)
			if containsVariant(r.TargetSymbol) {
				addStructural(r.SourceSymbol)
				evEndpoints = append(evEndpoints, r.TargetSymbol)
			}
		case "imports":
			addMention(r.SourceSymbol) // a usage edge, not a definition
		}
	}
	for _, f := range files {
		if containsVariant(f.Filename) || containsVariant(f.FilePath) {
			addStructural(f.FilePath)
		} else {
			addMention(f.FilePath) // matched via raw content only
		}
	}

	// Combined evidence (structural first), with mentions not already structural.
	evFiles := append([]string{}, structural...)
	for _, m := range mention {
		if !structuralSet[m] {
			evFiles = append(evFiles, m)
		}
	}

	green := isGreen(variants, exportedSymbols, files, endpointRoutes)
	hasSignal := len(files) > 0 || len(rels) > 0 || bestSim >= 0.25

	var category Category
	var confidence float64
	switch {
	case green:
		category = CategoryGreen
		confidence = clamp(0.85+bestSim*0.2, 0.85, 0.99)
	case hasSignal:
		category = CategoryYellow
		strength := float64(len(files)+len(rels)) / 3.0
		confidence = clamp(0.40+0.30*minF(1, strength)+0.14*bestSim, 0.40, 0.84)
	default:
		category = CategoryRed
		confidence = minF(0.39, bestSim)
	}

	return Match{
		Kind:           t.kind,
		Name:           t.name,
		Category:       category,
		Confidence:     round2(confidence),
		Files:          cap6(evFiles),
		Symbols:        cap6(evSymbols),
		Endpoints:      cap6(evEndpoints),
		Recommendation: recommend(category, t.name),
		structural:     cap6(structural),
	}
}

// isGreen reports an exact/dedicated structure: an exported symbol whose name
// is the term (+ a common suffix), a filename headed by the term, or an
// endpoint route segment equal to the term.
func isGreen(variants []string, exportedSymbols []string, files []store.FileRow, endpointRoutes []string) bool {
	forms := greenForms(variants)

	for _, s := range exportedSymbols {
		if forms[strings.ToLower(s)] {
			return true
		}
	}
	for _, f := range files {
		base := strings.ToLower(strings.TrimSuffix(f.Filename, ext(f.Filename)))
		if forms[base] {
			return true
		}
	}
	for _, route := range endpointRoutes {
		// route looks like "GET /category"
		for _, seg := range strings.Split(route, "/") {
			seg = strings.ToLower(strings.TrimSpace(seg))
			for _, v := range variants {
				if seg == v {
					return true
				}
			}
		}
	}
	return false
}

var commonSuffixes = []string{"", "s", "controller", "service", "model", "repository", "handler", "schema", "table", "router", "routes"}

// greenForms is the set of concatenated lowercase names that count as a
// dedicated structure for the term variants.
func greenForms(variants []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range variants {
		for _, suf := range commonSuffixes {
			out[v+suf] = true
		}
	}
	return out
}

// --- assembly ---------------------------------------------------------------

func assemble(description string, intents IntentBreakdown, matches []Match) *Response {
	resp := &Response{
		Description: description,
		Intents:     intents,
		Matches:     matches,
		Highlights:  Highlights{Green: []string{}, Yellow: []string{}},
		Gaps:        []GapNode{},
		GapEdges:    []GapEdge{},
		DiffSummary: []DiffItem{},
	}

	greenSet := map[string]bool{}
	yellowSet := map[string]bool{}
	fileFreq := map[string]int{}
	diffSeen := map[string]bool{}

	for _, m := range matches {
		switch m.Category {
		case CategoryGreen:
			resp.Summary.Green++
			greenFiles := m.structural
			if len(greenFiles) == 0 {
				greenFiles = m.Files
			}
			for _, f := range greenFiles {
				greenSet[f] = true
				fileFreq[f]++
			}
		case CategoryYellow:
			resp.Summary.Yellow++
			for _, f := range m.Files {
				yellowSet[f] = true
				fileFreq[f]++
				key := f + "|" + m.Name
				if !diffSeen[key] {
					diffSeen[key] = true
					resp.DiffSummary = append(resp.DiffSummary, DiffItem{
						File:       f,
						ChangeType: "extend",
						Category:   CategoryYellow,
						Detail:     fmt.Sprintf("Extend %s to support %q (%s).", f, m.Name, m.Kind),
					})
				}
			}
		case CategoryRed:
			resp.Summary.Red++
			gapID := "gap:" + slug(m.Name)
			suggested := suggestedFile(m.Kind, m.Name)
			resp.Gaps = append(resp.Gaps, GapNode{
				ID:            gapID,
				Label:         m.Name,
				Kind:          m.Kind,
				Reason:        fmt.Sprintf("No existing structure covers this %s.", m.Kind),
				SuggestedFile: suggested,
			})
			resp.DiffSummary = append(resp.DiffSummary, DiffItem{
				File:       suggested,
				ChangeType: "create",
				Category:   CategoryRed,
				Detail:     fmt.Sprintf("Create new %s structure for %q.", m.Kind, m.Name),
			})
		}
	}

	for f := range greenSet {
		resp.Highlights.Green = append(resp.Highlights.Green, nodeID(f))
	}
	for f := range yellowSet {
		if greenSet[f] {
			continue // green takes precedence
		}
		resp.Highlights.Yellow = append(resp.Highlights.Yellow, nodeID(f))
	}

	// Anchor gaps to the most-referenced existing file (where they'd wire in).
	anchor := ""
	best := 0
	for f, n := range fileFreq {
		if n > best {
			best = n
			anchor = f
		}
	}
	if anchor != "" {
		for _, g := range resp.Gaps {
			resp.GapEdges = append(resp.GapEdges, GapEdge{Source: g.ID, Target: nodeID(anchor)})
		}
	}

	resp.Summary.Total = len(matches)
	if resp.Summary.Total > 0 {
		resp.Summary.ReuseScore = round2(
			(float64(resp.Summary.Green) + 0.5*float64(resp.Summary.Yellow)) / float64(resp.Summary.Total),
		)
	}
	return resp
}

// --- helpers ----------------------------------------------------------------

var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

func nodeID(filePath string) string { return "file:" + filePath }

func termVariants(name string) []string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, "_", "")
	set := map[string]bool{n: true}
	switch {
	case strings.HasSuffix(n, "ies") && len(n) > 4:
		set[n[:len(n)-3]+"y"] = true
	case strings.HasSuffix(n, "s") && len(n) > 3:
		set[n[:len(n)-1]] = true
	default:
		set[n+"s"] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func recommend(c Category, name string) string {
	switch c {
	case CategoryGreen:
		return fmt.Sprintf("Reuse the existing implementation for %q.", name)
	case CategoryYellow:
		return fmt.Sprintf("Extend existing structures to cover %q.", name)
	default:
		return fmt.Sprintf("Build new — nothing in the codebase covers %q.", name)
	}
}

func suggestedFile(kind, name string) string {
	s := slug(name)
	s = strings.ReplaceAll(s, "-", "")
	if kind == "action" {
		return s + "Handler.ts"
	}
	return s + ".ts"
}

func slug(name string) string {
	s := nonAlnumRe.ReplaceAllString(strings.ToLower(name), "-")
	return strings.Trim(s, "-")
}

func ext(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i:]
	}
	return ""
}

func cap6(s []string) []string {
	if len(s) > 6 {
		return s[:6]
	}
	return s
}

func clamp(v, lo, hi float64) float64 { return maxF(lo, minF(hi, v)) }
func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

// Package rag implements the hybrid search + context orchestration layer.
//
// A query runs two retrieval strategies in parallel-by-design:
//   - Vector search: the question is embedded and matched (cosine) against
//     vector_chunks for the top-K most semantically similar code blocks.
//   - Keyword/graph match: literal symbols/routes mentioned in the question
//     (e.g. "/category", "fetchCategories") are matched against code_files and
//     ast_relationships.
//
// Results are de-duplicated and merged into a single context window that pairs
// absolute architectural facts (AST edges) with semantic code fragments, then
// handed to the LLM (or the offline template responder) which must answer in a
// strict JSON contract.
package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"project-synapse/backend/internal/embed"
	"project-synapse/backend/internal/llm"
	"project-synapse/backend/internal/store"
)

// QueryAnswer is the mandatory response contract consumed by the frontend.
type QueryAnswer struct {
	Answer           string        `json:"answer"`
	HighlightedFiles []string      `json:"highlighted_files"`
	ExecutionFlow    []string      `json:"execution_flow"`
	Functions        []FunctionHit `json:"functions"` // retrieved symbol-level code
}

// FunctionHit is a symbol-level chunk surfaced to the UI: the function/class the
// query matched, with its source code (header stripped) for the expandable view.
type FunctionHit struct {
	File      string `json:"file"`
	Symbol    string `json:"symbol"`
	ChunkType string `json:"chunk_type"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Code      string `json:"code"`
}

// buildFunctions maps the semantically-retrieved chunks into UI function hits.
func buildFunctions(rc retrieved) []FunctionHit {
	out := make([]FunctionHit, 0, len(rc.Chunks))
	for _, h := range rc.Chunks {
		out = append(out, FunctionHit{
			File:      h.FilePath,
			Symbol:    h.SymbolName,
			ChunkType: h.ChunkType,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			Code:      store.StripChunkHeader(h.Content),
		})
	}
	return out
}

// Orchestrator wires retrieval + generation.
type Orchestrator struct {
	Store    *store.Store
	Embedder embed.Embedder
	Chat     llm.ChatClient // nil => offline template responder
	TopK     int
}

// retrieved holds the merged, de-duplicated context for one query.
type retrieved struct {
	Chunks    []store.ChunkHit
	Endpoints []store.RelRow
	Imports   []store.RelRow
	Exports   []store.RelRow
	Files     []string // ordered, de-duplicated candidate file paths
	Facts     []string // human-readable architectural facts
}

const systemPrompt = `You are Project Synapse, a codebase intelligence engine.
Answer the user's question using ONLY the provided context, which combines absolute architectural facts (from an AST dependency graph) with semantic code fragments.

Respond with a SINGLE JSON object and nothing else, matching exactly this shape:
{
  "answer": "A precise markdown explanation answering the question.",
  "highlighted_files": ["array", "of", "exact", "file_paths", "involved"],
  "execution_flow": ["step-by-step", "file", "execution", "path"]
}

Rules:
- Use exact file paths exactly as they appear in the context.
- Do not invent files, routes, or symbols that are not in the context.
- If you cannot determine a field, use an empty array (or a short note for "answer").
- Output valid JSON only — no prose before or after, no markdown code fences.`

// Query runs the hybrid search + generation pipeline, scoped to one repo
// (root == "" searches across every ingested repo).
func (o *Orchestrator) Query(ctx context.Context, question, root string) (*QueryAnswer, error) {
	topK := o.TopK
	if topK <= 0 {
		topK = 5
	}

	// (a) Vector search.
	var hits []store.ChunkHit
	if vecs, err := o.Embedder.Embed(ctx, []string{question}); err == nil && len(vecs) > 0 {
		if h, verr := o.Store.VectorSearch(ctx, vecs[0], topK, root); verr == nil {
			hits = h
		}
	}

	// (b) Keyword / graph match.
	patterns := toPatterns(extractTokens(question))
	kwFiles, _ := o.Store.KeywordSearchFiles(ctx, patterns, root)
	kwRels, _ := o.Store.KeywordSearchRelationships(ctx, patterns, root)

	rc := mergeContext(hits, kwFiles, kwRels)

	var ans *QueryAnswer
	if o.Chat == nil {
		ans = templateAnswer(question, rc)
	} else {
		var err error
		ans, err = o.llmAnswer(ctx, question, rc)
		if err != nil {
			return nil, err
		}
	}
	// Attach the retrieved symbol-level code regardless of responder, so the UI
	// can show the responsible functions with expandable source.
	ans.Functions = buildFunctions(rc)
	return ans, nil
}

// systemPromptStream asks for plain markdown prose (not JSON) so the answer can
// be streamed token-by-token directly to the UI. The structured fields
// (highlighted_files / execution_flow / functions) are derived from retrieval
// and sent ahead of the stream, so the model only produces the explanation.
const systemPromptStream = `You are Project Synapse, a codebase intelligence engine answering an engineer's question about THEIR specific codebase. You are given architectural facts from an AST dependency graph plus the most relevant real code fragments retrieved from the repo.

Write a precise, actionable answer in GitHub-flavored markdown that an engineer working in this repo could use immediately:
- Open with a direct one or two sentence answer — no preamble, no restating the question.
- Then explain concretely, citing the EXACT file paths and symbols from the context (wrap each in backticks, e.g. ` + "`internal/api/server.go`" + ` or ` + "`handleQuery`" + `). When the question is about behaviour ("how does X work"), trace the actual control/data flow file-by-file using the cited code.
- Lead with specifics visible in the fragments — function names, types, routes, struct fields, SQL — not generic descriptions. Use a short heading, bullets, or a numbered flow only when it genuinely improves clarity.
- If the context does not contain the answer, say so in one line and point at the closest relevant file rather than guessing. Never invent files, routes, or symbols that are not in the context.

Do not pad with generic software-engineering advice. Output the markdown answer only — no JSON, no surrounding code fences.`

// QueryStream runs the hybrid retrieval, emits the structured metadata
// (highlighted files / execution flow / retrieved functions) via onMeta, then
// streams the natural-language answer token-by-token via onToken.
func (o *Orchestrator) QueryStream(
	ctx context.Context,
	question, root string,
	onMeta func(*QueryAnswer),
	onToken func(string),
) error {
	topK := o.TopK
	if topK <= 0 {
		topK = 5
	}

	var hits []store.ChunkHit
	if vecs, err := o.Embedder.Embed(ctx, []string{question}); err == nil && len(vecs) > 0 {
		if h, verr := o.Store.VectorSearch(ctx, vecs[0], topK, root); verr == nil {
			hits = h
		}
	}
	patterns := toPatterns(extractTokens(question))
	kwFiles, _ := o.Store.KeywordSearchFiles(ctx, patterns, root)
	kwRels, _ := o.Store.KeywordSearchRelationships(ctx, patterns, root)
	rc := mergeContext(hits, kwFiles, kwRels)

	meta := &QueryAnswer{
		HighlightedFiles: nonNil(rc.Files),
		ExecutionFlow:    nonNil(executionFlow(rc)),
		Functions:        buildFunctions(rc),
	}
	if onMeta != nil {
		onMeta(meta)
	}

	// Offline template responder: emit the deterministic answer in one piece.
	if o.Chat == nil {
		onToken(templateAnswer(question, rc).Answer)
		return nil
	}

	user := assembleContext(rc) + "\n\nQuestion: " + question
	if sc, ok := o.Chat.(llm.StreamingChatClient); ok {
		_, err := sc.Stream(ctx, systemPromptStream, user, onToken)
		return err
	}
	// Non-streaming provider: complete then emit whole.
	raw, err := o.Chat.Complete(ctx, systemPromptStream, user)
	if err != nil {
		return err
	}
	onToken(raw)
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// llmAnswer assembles the context window, calls the model, and parses its JSON.
func (o *Orchestrator) llmAnswer(ctx context.Context, question string, rc retrieved) (*QueryAnswer, error) {
	user := assembleContext(rc) + "\n\nQuestion: " + question
	raw, err := o.Chat.Complete(ctx, systemPrompt, user)
	if err != nil {
		return nil, fmt.Errorf("llm complete: %w", err)
	}

	ans, perr := parseAnswer(raw)
	if perr != nil {
		// The model didn't return clean JSON — degrade gracefully rather than
		// failing the request, preserving the contract for the frontend.
		return &QueryAnswer{
			Answer:           strings.TrimSpace(raw),
			HighlightedFiles: rc.Files,
			ExecutionFlow:    executionFlow(rc),
		}, nil
	}
	if len(ans.HighlightedFiles) == 0 {
		ans.HighlightedFiles = rc.Files
	}
	if len(ans.ExecutionFlow) == 0 {
		ans.ExecutionFlow = executionFlow(rc)
	}
	return ans, nil
}

// parseAnswer extracts the first JSON object from the model output.
func parseAnswer(raw string) (*QueryAnswer, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found")
	}
	var ans QueryAnswer
	if err := json.Unmarshal([]byte(raw[start:end+1]), &ans); err != nil {
		return nil, err
	}
	ans.Answer = llm.CleanMarkdown(ans.Answer)
	return &ans, nil
}

// --- context merge / assembly ----------------------------------------------

var fileExtRe = regexp.MustCompile(`\.(ts|tsx|js|jsx|mjs|cjs)$`)

func looksLikeFile(s string) bool { return fileExtRe.MatchString(s) }

// mergeContext de-duplicates vector hits, keyword files, and graph edges into a
// single ordered context, and derives architectural facts + the candidate file
// set.
func mergeContext(hits []store.ChunkHit, kwFiles []store.FileRow, rels []store.RelRow) retrieved {
	rc := retrieved{Chunks: hits}

	fileSet := newOrderedSet()

	// Endpoint-owning files first (entry points), then their relationships.
	for _, r := range rels {
		switch r.RelationshipType {
		case "endpoint":
			rc.Endpoints = append(rc.Endpoints, r)
			fileSet.add(r.SourceSymbol)
			rc.Facts = append(rc.Facts, factForEndpoint(r))
		}
	}
	for _, r := range rels {
		switch r.RelationshipType {
		case "imports":
			rc.Imports = append(rc.Imports, r)
			fileSet.add(r.SourceSymbol)
			if looksLikeFile(r.TargetSymbol) {
				fileSet.add(r.TargetSymbol)
			}
			rc.Facts = append(rc.Facts, factForImport(r))
		case "exports":
			rc.Exports = append(rc.Exports, r)
			fileSet.add(r.SourceSymbol)
			rc.Facts = append(rc.Facts, fmt.Sprintf("%s exports %s", r.SourceSymbol, r.TargetSymbol))
		}
	}

	// Vector-hit files next.
	for _, h := range hits {
		fileSet.add(h.FilePath)
	}
	// Keyword file matches last.
	for _, f := range kwFiles {
		fileSet.add(f.FilePath)
	}

	rc.Files = fileSet.items()
	return rc
}

func factForEndpoint(r store.RelRow) string {
	method, _ := r.Metadata["method"].(string)
	path, _ := r.Metadata["path"].(string)
	handler, _ := r.Metadata["handler"].(string)
	src, _ := r.Metadata["source"].(string)
	fact := fmt.Sprintf("%s declares HTTP endpoint %s %s", r.SourceSymbol, method, path)
	if handler != "" {
		fact += fmt.Sprintf(" (handler: %s)", handler)
	}
	if src != "" {
		fact += fmt.Sprintf(" [%s]", src)
	}
	return fact
}

func factForImport(r store.RelRow) string {
	external, _ := r.Metadata["external"].(bool)
	if external {
		return fmt.Sprintf("%s imports external module %s", r.SourceSymbol, r.TargetSymbol)
	}
	return fmt.Sprintf("%s imports %s", r.SourceSymbol, r.TargetSymbol)
}

// assembleContext renders the merged context into the LLM context window.
func assembleContext(rc retrieved) string {
	var b strings.Builder
	b.WriteString("ARCHITECTURAL FACTS (from the AST dependency graph — ground truth, not guesses):\n")
	if len(rc.Facts) == 0 {
		b.WriteString("- (none matched)\n")
	}
	for i, f := range rc.Facts {
		if i >= 40 {
			break
		}
		b.WriteString("- " + f + "\n")
	}

	b.WriteString("\nRELEVANT CODE FRAGMENTS (semantic search over the repo, most relevant first):\n")
	if len(rc.Chunks) == 0 {
		b.WriteString("(none)\n")
	}
	for i, h := range rc.Chunks {
		content := h.Content
		if len(content) > 1800 {
			content = content[:1800] + "\n…(truncated)"
		}
		loc := h.FilePath
		if h.SymbolName != "" {
			loc += " :: " + h.SymbolName
		}
		if h.StartLine > 0 {
			loc += fmt.Sprintf(" (lines %d-%d)", h.StartLine, h.EndLine)
		}
		b.WriteString(fmt.Sprintf("\n[fragment %d — %s]\n%s\n", i+1, loc, content))
	}
	return b.String()
}

// executionFlow derives an ordered file execution path from the graph edges.
func executionFlow(rc retrieved) []string {
	var flow []string
	for _, e := range rc.Endpoints {
		method, _ := e.Metadata["method"].(string)
		path, _ := e.Metadata["path"].(string)
		flow = append(flow, fmt.Sprintf("Request → %s %s handled in %s", method, path, e.SourceSymbol))
	}
	for _, imp := range rc.Imports {
		if looksLikeFile(imp.TargetSymbol) {
			flow = append(flow, fmt.Sprintf("%s → %s", imp.SourceSymbol, imp.TargetSymbol))
		}
	}
	if len(flow) == 0 {
		flow = append([]string{}, rc.Files...)
	}
	return flow
}

// templateAnswer builds the contract JSON deterministically (offline mode),
// synthesising a readable markdown answer from the retrieved graph + chunks.
func templateAnswer(question string, rc retrieved) *QueryAnswer {
	var b strings.Builder
	fmt.Fprintf(&b, "Based on the codebase knowledge graph and %d semantic code fragment(s):\n\n", len(rc.Chunks))

	if len(rc.Endpoints) > 0 {
		b.WriteString("**Matching endpoints**\n")
		for _, e := range rc.Endpoints {
			method, _ := e.Metadata["method"].(string)
			path, _ := e.Metadata["path"].(string)
			handler, _ := e.Metadata["handler"].(string)
			fmt.Fprintf(&b, "- `%s %s` is handled in `%s`", method, path, e.SourceSymbol)
			if handler != "" {
				fmt.Fprintf(&b, " by `%s`", handler)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(rc.Files) > 0 {
		b.WriteString("**Files involved:** ")
		b.WriteString("`" + strings.Join(rc.Files, "`, `") + "`\n\n")
	}

	if len(rc.Imports) > 0 {
		b.WriteString("**Dependency edges**\n")
		shown := 0
		for _, imp := range rc.Imports {
			b.WriteString("- " + factForImport(imp) + "\n")
			if shown++; shown >= 8 {
				break
			}
		}
		b.WriteString("\n")
	}

	if len(rc.Endpoints) == 0 && len(rc.Files) == 0 && len(rc.Chunks) == 0 {
		b.WriteString("No matching code structures were found for this question. Try ingesting the relevant directory or rephrasing with a concrete file, route, or symbol name.\n")
	}

	b.WriteString("\n_(Offline deterministic synthesis — configure ANTHROPIC_API_KEY or OPENAI_API_KEY for a full natural-language answer.)_")

	return &QueryAnswer{
		Answer:           b.String(),
		HighlightedFiles: rc.Files,
		ExecutionFlow:    executionFlow(rc),
	}
}

// --- token extraction -------------------------------------------------------

var (
	routeRe = regexp.MustCompile(`/[A-Za-z0-9_\-/.]+`)
	identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)
)

var stopwords = map[string]bool{
	"where": true, "what": true, "which": true, "how": true, "the": true, "and": true,
	"are": true, "for": true, "with": true, "this": true, "that": true, "does": true,
	"route": true, "routes": true, "handled": true, "handle": true, "file": true,
	"files": true, "code": true, "find": true, "show": true, "from": true, "into": true,
	"function": true, "functions": true, "method": true, "class": true, "between": true,
	"about": true, "when": true, "used": true, "uses": true, "have": true, "has": true,
}

// extractTokens pulls candidate literals from the question: route-like paths
// (always kept) and identifiers (minus generic stopwords).
func extractTokens(question string) []string {
	set := newOrderedSet()
	for _, r := range routeRe.FindAllString(question, -1) {
		set.add(strings.TrimRight(r, "."))
	}
	for _, w := range identRe.FindAllString(question, -1) {
		if !stopwords[strings.ToLower(w)] {
			set.add(w)
		}
	}
	items := set.items()
	if len(items) > 12 {
		items = items[:12]
	}
	return items
}

func toPatterns(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, "%"+t+"%")
	}
	return out
}

// --- small ordered-set helper ----------------------------------------------

type orderedSet struct {
	seen  map[string]bool
	order []string
}

func newOrderedSet() *orderedSet { return &orderedSet{seen: map[string]bool{}} }

func (s *orderedSet) add(v string) {
	if v == "" || s.seen[v] {
		return
	}
	s.seen[v] = true
	s.order = append(s.order, v)
}

func (s *orderedSet) items() []string { return s.order }

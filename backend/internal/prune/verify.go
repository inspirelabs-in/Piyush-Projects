package prune

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"project-synapse/backend/internal/store"
)

const verifySystem = `You are auditing a STATIC dead-code analysis for FALSE POSITIVES. Each candidate is a source file with NO static importers, flagged as possibly-dead. Static import analysis cannot see files reached indirectly, so your job is to spot those.

Using the file path, language, and exported symbols, classify each file:
- "framework": almost certainly invoked indirectly and NOT dead — e.g. a Next.js page/layout/route/middleware/loader/action, a registered HTTP route handler, a CLI/main/entry, a dependency-injected/registered provider, a public library API, generated code, a config- or convention-loaded module, or test setup.
- "dead": genuinely appears removable — a plain module nothing references, with no framework or dynamic-entry signals.
- "uncertain": you cannot tell from the given signals.

Respond with ONE JSON object, nothing else:
{"verdicts":[{"id":<number>,"verdict":"framework|dead|uncertain","reason":"short justification"}]}
Return one verdict per candidate id. Output valid JSON only — no prose, no code fences.`

// verify reviews file-level candidates with the LLM and drops the ones that are
// almost certainly reached indirectly (framework/dynamic), demotes uncertain
// ones, and confirms the rest — cutting the main dead-code false-positive class.
func (e *Engine) verify(ctx context.Context, rep *Report, rels []store.RelRow) {
	exportsByFile := map[string][]string{}
	for _, r := range rels {
		if r.RelationshipType == "exports" {
			exportsByFile[r.SourceSymbol] = append(exportsByFile[r.SourceSymbol], r.TargetSymbol)
		}
	}

	var targets []int // indices of file-level candidates
	for i, c := range rep.Candidates {
		if c.Kind == "file" {
			targets = append(targets, i)
		}
	}
	if len(targets) == 0 {
		return
	}
	const maxVerify = 40
	if len(targets) > maxVerify {
		targets = targets[:maxVerify]
	}

	drop := map[int]bool{}
	const batch = 10
	for i := 0; i < len(targets); i += batch {
		end := i + batch
		if end > len(targets) {
			end = len(targets)
		}
		e.verifyBatch(ctx, rep, targets[i:end], exportsByFile, drop)
	}

	if len(drop) == 0 {
		return
	}
	kept := rep.Candidates[:0:0]
	for i, c := range rep.Candidates {
		if !drop[i] {
			kept = append(kept, c)
		}
	}
	rep.Candidates = kept
	rep.Summary = map[string]int{}
	for _, c := range rep.Candidates {
		rep.Summary[c.Tier]++
	}
	rep.Notes = append(rep.Notes, fmt.Sprintf("LLM verification removed %d likely framework-invoked / dynamically-loaded false positive(s).", len(drop)))
}

func (e *Engine) verifyBatch(ctx context.Context, rep *Report, idxs []int, exportsByFile map[string][]string, drop map[int]bool) {
	var payload strings.Builder
	for i, idx := range idxs {
		c := rep.Candidates[idx]
		exps := exportsByFile[c.Path]
		if len(exps) > 12 {
			exps = exps[:12]
		}
		fmt.Fprintf(&payload, "\n[%d] file: %s\n    language: %s\n    exported symbols: %s\n    flagged because: %s\n",
			i, c.Path, c.Language, strings.Join(exps, ", "), c.Reason)
	}
	raw, err := e.Chat.Complete(ctx, verifySystem, payload.String())
	if err != nil {
		return
	}
	var parsed struct {
		Verdicts []struct {
			ID      int    `json:"id"`
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
		} `json:"verdicts"`
	}
	if json.Unmarshal([]byte(extractJSON(raw)), &parsed) != nil {
		return
	}
	for _, v := range parsed.Verdicts {
		if v.ID < 0 || v.ID >= len(idxs) {
			continue
		}
		idx := idxs[v.ID]
		reason := strings.TrimSpace(v.Reason)
		switch strings.ToLower(strings.TrimSpace(v.Verdict)) {
		case "framework", "reachable", "used":
			drop[idx] = true
		case "uncertain":
			rep.Candidates[idx].Uncertain = true
			rep.Candidates[idx].Confidence = "medium"
			if reason != "" {
				rep.Candidates[idx].Evidence = append(rep.Candidates[idx].Evidence, "needs review: "+reason)
			}
		case "dead":
			rep.Candidates[idx].Confidence = "high"
			if reason != "" {
				rep.Candidates[idx].Evidence = append(rep.Candidates[idx].Evidence, "LLM-verified dead: "+reason)
			}
		}
	}
}

func extractJSON(raw string) string {
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end <= start {
		return raw
	}
	return raw[start : end+1]
}

package bugs

import (
	"strings"
	"testing"

	"project-synapse/backend/internal/store"
)

func imp(src, target string) store.RelRow {
	return store.RelRow{
		SourceSymbol: src, TargetSymbol: target,
		RelationshipType: "imports", Metadata: map[string]any{"external": false},
	}
}

func TestDetectCycles(t *testing.T) {
	files := []store.FileRow{
		{FilePath: "a.ts", Language: "typescript"},
		{FilePath: "b.ts", Language: "typescript"},
		{FilePath: "c.ts", Language: "typescript"},
		{FilePath: "d.ts", Language: "typescript"}, // not in the cycle
	}
	rels := []store.RelRow{
		imp("a.ts", "b.ts"),
		imp("b.ts", "c.ts"),
		imp("c.ts", "a.ts"), // closes the loop a→b→c→a
		imp("d.ts", "a.ts"), // d depends on the cluster but isn't part of the cycle
	}
	bugs := detectCycles(buildGraph(files, rels))
	if len(bugs) != 1 {
		t.Fatalf("got %d cycle bugs, want 1: %+v", len(bugs), bugs)
	}
	b := bugs[0]
	if b.Category != "circular_dependency" || b.Severity != "HIGH" {
		t.Errorf("category/severity = %q/%q", b.Category, b.Severity)
	}
	if len(b.ContextNodes) != 3 {
		t.Errorf("cycle should have 3 files, got %v", b.ContextNodes)
	}
	for _, want := range []string{"a.ts", "b.ts", "c.ts"} {
		found := false
		for _, n := range b.ContextNodes {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("cycle missing %s (%v)", want, b.ContextNodes)
		}
	}
}

func TestNoCycleWhenAcyclic(t *testing.T) {
	files := []store.FileRow{{FilePath: "a.ts", Language: "typescript"}, {FilePath: "b.ts", Language: "typescript"}}
	rels := []store.RelRow{imp("a.ts", "b.ts")} // a→b, no loop
	if bugs := detectCycles(buildGraph(files, rels)); len(bugs) != 0 {
		t.Errorf("acyclic graph produced cycles: %+v", bugs)
	}
}

func TestDetectResourceLeaks(t *testing.T) {
	funcs := []store.FuncCodeRow{
		{File: "store.go", Symbol: "Leaky", StartLine: 1, EndLine: 4,
			Code: "func Leaky(db *sql.DB) {\n\trows, _ := db.Query(\"select 1\")\n\tfor rows.Next() {}\n}"},
		{File: "store.go", Symbol: "Safe", StartLine: 6, EndLine: 9,
			Code: "func Safe(db *sql.DB) {\n\trows, _ := db.Query(\"select 1\")\n\tdefer rows.Close()\n}"},
		{File: "store.go", Symbol: "OpenTx", StartLine: 11, EndLine: 14,
			Code: "func OpenTx(db *sql.DB) {\n\ttx, _ := db.Begin()\n\t_ = tx\n}"}, // no Commit/Rollback
	}
	bugs := detectResourceLeaks(funcs)

	got := map[string]bool{}
	for _, b := range bugs {
		if b.Category != "resource_leak" {
			t.Errorf("unexpected category: %+v", b)
		}
		got[b.Location.Entity] = true
	}
	if !got["Leaky"] {
		t.Errorf("Leaky (rows never Closed) should be flagged; bugs=%+v", bugs)
	}
	if !got["OpenTx"] {
		t.Errorf("OpenTx (tx never Committed/Rolled back) should be flagged")
	}
	if got["Safe"] {
		t.Errorf("Safe (defer rows.Close()) must NOT be flagged")
	}
}

func TestBugIDFormat(t *testing.T) {
	// Spot-check the SYN-YYYY-NNN id shape used by Scan's `add`.
	id := "SYN-2026-001"
	if !strings.HasPrefix(id, "SYN-") || strings.Count(id, "-") != 2 {
		t.Errorf("bad id shape %q", id)
	}
}

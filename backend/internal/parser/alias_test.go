package parser

import "testing"

func TestTSConfigAliasResolution(t *testing.T) {
	// JSONC: a line comment + a trailing comma, both of which must be tolerated.
	tsconfig := []byte(`{
        // editor settings
        "compilerOptions": {
            "baseUrl": ".",
            "paths": { "@/*": ["./*"] },
        }
    }`)
	rules := ParseTSConfigPaths("frontend", tsconfig)
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1 (%+v)", len(rules), rules)
	}

	idx := BuildIndexWithAliases([]string{
		"frontend/auth.ts",
		"frontend/app/page.tsx",
		"frontend/lib/api.ts",
	}, rules)

	fa := &FileAnalysis{Language: "typescript", RelPath: "frontend/app/page.tsx", Imports: []ImportRef{
		{Specifier: "@/auth"},
		{Specifier: "@/lib/api"},
		{Specifier: "react"},
	}}
	ResolveImports(fa, idx)

	if fa.Imports[0].Resolved != "frontend/auth.ts" || !fa.Imports[0].ResolvedOK || fa.Imports[0].External {
		t.Errorf("@/auth resolved wrong: %+v", fa.Imports[0])
	}
	if fa.Imports[1].Resolved != "frontend/lib/api.ts" || !fa.Imports[1].ResolvedOK {
		t.Errorf("@/lib/api resolved wrong: %+v", fa.Imports[1])
	}
	if !fa.Imports[2].External {
		t.Errorf("react should still be external: %+v", fa.Imports[2])
	}
}

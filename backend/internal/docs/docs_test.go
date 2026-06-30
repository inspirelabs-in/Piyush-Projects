package docs

import (
	"context"
	"strings"
	"testing"
)

// mockChat returns a fixed completion — here, JSON whose markdown newlines are
// DOUBLE-escaped (\\n), reproducing the model behaviour that rendered visible
// "\n" and unparsed "## Heading" in the docs UI.
type mockChat struct{ resp string }

func (m mockChat) Complete(_ context.Context, _, _ string) (string, error) { return m.resp, nil }
func (m mockChat) Name() string                                           { return "mock" }

func TestNarrativeRepairsDoubleEscapedNewlines(t *testing.T) {
	// In this raw Go string, `\\n` is backslash-backslash-n; as JSON that decodes
	// to a literal backslash-n in the field value — exactly the reported bug.
	raw := `{"introduction":"GrabOn_App is a mobile app.\n\n## Capabilities\n- Retrieve deals\n- Update itself","architecture":"## Layers\n- a","concepts":"### Concept\nText.","data_flow":"1. step one\n2. step two"}`

	e := &Engine{Chat: mockChat{resp: raw}}
	nd := e.narrative(context.Background(), "GrabOn_App", nil, nil)

	if strings.Contains(nd.Introduction, `\n`) {
		t.Errorf("introduction still contains literal \\n: %q", nd.Introduction)
	}
	if !strings.Contains(nd.Introduction, "\n## Capabilities") {
		t.Errorf("heading is not on its own line:\n%s", nd.Introduction)
	}
	for name, got := range map[string]string{
		"architecture": nd.Architecture,
		"concepts":     nd.Concepts,
		"data_flow":    nd.DataFlow,
	} {
		if strings.Contains(got, `\n`) {
			t.Errorf("%s still contains literal \\n: %q", name, got)
		}
		if !strings.Contains(got, "\n") {
			t.Errorf("%s has no real newline: %q", name, got)
		}
	}
}

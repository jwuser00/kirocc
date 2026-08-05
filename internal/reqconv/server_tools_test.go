package reqconv

import (
	"testing"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiromcp"
	"github.com/d-kuro/kirocc/internal/toolsearch"
)

func webSearchServerTool() anthropic.Tool {
	// Exactly what Claude Code sends: a versioned type, a name, no input_schema.
	return anthropic.Tool{Type: "web_search_20250305", Name: "web_search"}
}

func clientTool(name string) anthropic.Tool {
	return anthropic.Tool{
		Name:        name,
		Description: "d",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func TestRewriteServerTools_StripsServerToolsAndFlagsWebSearch(t *testing.T) {
	tools := []anthropic.Tool{
		clientTool("Read"),
		webSearchServerTool(),
		{Type: "web_fetch_20250910", Name: "web_fetch"},
		{Type: "code_execution_20250522", Name: "code_execution"},
		clientTool("Write"),
	}

	kept, want := RewriteServerTools(tools, true)
	if !want {
		t.Error("wantWebSearch = false, want true")
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d tools, want 2: %+v", len(kept), kept)
	}
	for _, k := range kept {
		if IsAnthropicServerTool(k) {
			t.Errorf("server tool survived: %+v", k)
		}
	}
}

func TestRewriteServerTools_DisabledStillStrips(t *testing.T) {
	// With the feature off the declaration must still go: leaving it would make
	// Kiro reject the entire request, which is the bug this guards.
	kept, want := RewriteServerTools([]anthropic.Tool{clientTool("Read"), webSearchServerTool()}, false)
	if want {
		t.Error("wantWebSearch = true, want false")
	}
	if len(kept) != 1 || kept[0].Name != "Read" {
		t.Fatalf("kept = %+v", kept)
	}
}

func TestRewriteServerTools_NoServerToolsReturnsInputUnchanged(t *testing.T) {
	in := []anthropic.Tool{clientTool("Read"), clientTool("Write")}
	kept, want := RewriteServerTools(in, true)
	if want {
		t.Error("wantWebSearch = true, want false")
	}
	if len(kept) != len(in) {
		t.Fatalf("kept %d, want %d", len(kept), len(in))
	}
}

func TestIsAnthropicServerTool(t *testing.T) {
	cases := []struct {
		tool anthropic.Tool
		want bool
	}{
		{anthropic.Tool{Type: "web_search_20250305", Name: "web_search"}, true},
		{anthropic.Tool{Type: "web_search_20260209", Name: "web_search"}, true},
		{anthropic.Tool{Type: "web_fetch_20250910", Name: "web_fetch"}, true},
		{anthropic.Tool{Type: "bash_20250124", Name: "bash"}, true},
		{anthropic.Tool{Type: "text_editor_20250728", Name: "str_replace"}, true},
		{clientTool("Read"), false},
		{anthropic.Tool{Type: "custom", Name: "x"}, false},
	}
	for _, c := range cases {
		if got := IsAnthropicServerTool(c.tool); got != c.want {
			t.Errorf("IsAnthropicServerTool(%q) = %v, want %v", c.tool.Type, got, c.want)
		}
	}
}

func TestWebSearchOptionsFrom_FindsDeferredDeclaration(t *testing.T) {
	// Claude Code defers WebSearch until the model asks for it, so a request
	// that is about to need a search carries it only in the deferred map.
	tsCtx := &toolsearch.Context{
		DeferredTools: map[string]anthropic.Tool{"WebSearch": webSearchServerTool()},
	}
	if WebSearchOptionsFrom(nil, tsCtx) == nil {
		t.Error("WebSearchOptionsFrom = nil for deferred declaration, want options")
	}
}

func TestWebSearchOptionsFrom_ActiveAndAbsent(t *testing.T) {
	active := &toolsearch.Context{ActiveTools: []anthropic.Tool{webSearchServerTool()}}
	if WebSearchOptionsFrom(nil, active) == nil {
		t.Error("WebSearchOptionsFrom = nil for active declaration")
	}
	if WebSearchOptionsFrom([]anthropic.Tool{clientTool("Read")}, nil) != nil {
		t.Error("WebSearchOptionsFrom != nil with no declaration")
	}
	if WebSearchOptionsFrom([]anthropic.Tool{webSearchServerTool()}, nil) == nil {
		t.Error("WebSearchOptionsFrom = nil for plain tool list")
	}
}

func TestWebSearchOptionsFrom_CarriesDeclarationOptions(t *testing.T) {
	decl := webSearchServerTool()
	decl.MaxUses = 4
	decl.AllowedDomains = []string{"go.dev", "pkg.go.dev"}
	opts := WebSearchOptionsFrom([]anthropic.Tool{decl}, nil)
	if opts == nil {
		t.Fatal("WebSearchOptionsFrom = nil")
	}
	if opts.MaxUses != 4 {
		t.Errorf("MaxUses = %d, want 4", opts.MaxUses)
	}
	if len(opts.AllowedDomains) != 2 || opts.AllowedDomains[0] != "go.dev" {
		t.Errorf("AllowedDomains = %v", opts.AllowedDomains)
	}
}

func TestBuildSystemAndTools_SwapsWebSearchForKiroTool(t *testing.T) {
	req := &anthropic.Request{
		Tools: []anthropic.Tool{clientTool("Read"), webSearchServerTool()},
	}
	_, entries := buildSystemAndTools(req, nil, NewToolNameMap(), true)

	var names []string
	for _, e := range entries {
		if e.ToolSpecification != nil {
			names = append(names, e.ToolSpecification.Name)
		}
	}
	if len(names) != 2 || names[0] != "Read" || names[1] != kiromcp.WebSearchToolName {
		t.Fatalf("tool names = %v, want [Read web_search]", names)
	}
	for _, e := range entries {
		if e.ToolSpecification == nil {
			continue
		}
		if e.ToolSpecification.InputSchema.JSON["type"] != "object" {
			t.Errorf("tool %q has no object schema: %v",
				e.ToolSpecification.Name, e.ToolSpecification.InputSchema.JSON)
		}
	}
}

func TestBuildSystemAndTools_DisabledDropsWebSearchEntirely(t *testing.T) {
	req := &anthropic.Request{Tools: []anthropic.Tool{clientTool("Read"), webSearchServerTool()}}
	_, entries := buildSystemAndTools(req, nil, NewToolNameMap(), false)
	if len(entries) != 1 || entries[0].ToolSpecification.Name != "Read" {
		t.Fatalf("entries = %+v, want only Read", entries)
	}
}

func TestEnsureObjectSchema_FillsMissingSchema(t *testing.T) {
	// A tool with no input_schema reaches Kiro as {} and takes the whole
	// request down with it; the guard turns it into a valid empty object.
	got := ensureObjectSchema(SanitizeJSONSchema(nil))
	if got["type"] != "object" {
		t.Errorf("type = %v, want object", got["type"])
	}
	if _, ok := got["properties"]; !ok {
		t.Errorf("properties missing: %v", got)
	}
}

func TestEnsureObjectSchema_PreservesExisting(t *testing.T) {
	in := map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string"}},
		"required":   []any{"q"},
	}
	got := ensureObjectSchema(in)
	props, _ := got["properties"].(map[string]any)
	if _, ok := props["q"]; !ok {
		t.Errorf("existing properties lost: %v", got)
	}
	if got["required"] == nil {
		t.Errorf("required lost: %v", got)
	}
}

func TestConvertTools_SchemalessToolGetsObjectSchema(t *testing.T) {
	entries := ConvertTools([]anthropic.Tool{{Name: "foo", Description: "does foo"}}, NewToolNameMap())
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].ToolSpecification.InputSchema.JSON["type"] != "object" {
		t.Errorf("schema = %v, want object type", entries[0].ToolSpecification.InputSchema.JSON)
	}
}

package kiromcp

import "github.com/d-kuro/kirocc/internal/kiroproto"

// WebSearchToolName is the tool name the Kiro-hosted MCP server exposes.
// Confirmed via tools/list, which currently returns this tool and no other.
const WebSearchToolName = "web_search"

// MaxQueryLength is the query cap the MCP server enforces. Exceeding it comes
// back as a ValidationException, so the orchestrator trims before calling.
const MaxQueryLength = 200

// webSearchDescription is condensed from the description tools/list returns.
// The original runs ~4 KB and would be resent on every request; the operational
// rules that actually change model behaviour (query cap, attribution, verbatim
// limit, output fields) are preserved verbatim in substance.
const webSearchDescription = `Search the web for information outside the model's training data or that cannot be inferred from the current codebase.

## When to Use
- The user asks for current or up-to-date information (pricing, versions, technical specs), or explicitly requests a web search.
- Verifying information that may have changed recently.

## When NOT to Use
- Basic concepts, historical facts, or well-established syntax and documentation.
- Topics that do not require current or evolving information.

For code-related tasks, search the repository first and use this tool only if the question is still unresolved and the library or data is likely new.

## Content Compliance
- ALWAYS attribute sources with inline links: [description](url)
- NEVER reproduce more than 30 consecutive words from any single source; paraphrase and summarize instead.
- You may paraphrase, summarize and reformat, but MUST NOT change the underlying substance.

## Usage
- Query MUST be 200 characters or fewer.
- You may rephrase the user's query, and may issue multiple focused queries.

## Output
Returns JSON with a results array of {title, url, snippet, publishedDate, id, domain}. Prioritize recent publishedDate values and prefer official documentation over blogs and news posts.`

// webSearchSchema mirrors the inputSchema tools/list returns for web_search.
func webSearchSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"query"},
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query to execute. Must be 200 characters or less.",
			},
		},
	}
}

// WebSearchToolEntry returns the Kiro tool entry that lets the model request a
// web search. The model only ever emits a tool_use for it; kirocc intercepts
// that call, runs it through CallTool, and never forwards it to the client.
func WebSearchToolEntry() kiroproto.ToolEntry {
	return kiroproto.ToolEntry{
		ToolSpecification: &kiroproto.ToolSpecification{
			Name:        WebSearchToolName,
			Description: webSearchDescription,
			InputSchema: kiroproto.InputSchema{JSON: webSearchSchema()},
		},
	}
}

// TrimQuery clamps a query to MaxQueryLength runes.
func TrimQuery(q string) string {
	r := []rune(q)
	if len(r) <= MaxQueryLength {
		return q
	}
	return string(r[:MaxQueryLength])
}

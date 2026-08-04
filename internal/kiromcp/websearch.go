package kiromcp

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"
	"unicode/utf8"

	"github.com/d-kuro/kirocc/internal/kiroproto"
)

// WebSearchToolName is the tool name the Kiro-hosted MCP server exposes.
// Confirmed via tools/list, which currently returns this tool and no other.
const WebSearchToolName = "web_search"

// MaxQueryLength is the query cap the MCP server enforces. Exceeding it comes
// back as a ValidationException, so the orchestrator trims before calling.
const MaxQueryLength = 200

// webSearchDescription is condensed from the description tools/list returns,
// adapted for kirocc's enrichment: results carry fetched page content, and one
// call may fan out several queries.
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
- Each query MUST be 200 characters or fewer.
- Prefer ONE call with several focused queries (query + additional_queries) over sequential calls: all queries in a call run in parallel, so covering different angles at once is much faster than searching, reading, then searching again.
- You may rephrase the user's question freely.

## Output
Returns JSON with a results array of {title, url, snippet, publishedDate, domain, content}. The content field, when present, holds the readable text of the page itself — ground your answer in it rather than in snippets. Prioritize recent publishedDate values and prefer official documentation over blogs and news posts.`

// webSearchSchema is the inputSchema offered to the model. It extends the
// upstream single-query schema with additional_queries so one call can fan out
// several searches in parallel.
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
			"additional_queries": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Up to 4 further queries covering other angles of the same question, executed in parallel with the main query.",
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

// SearchResult is one entry of the web_search results array, extended with the
// Content field kirocc fills from the fetched page.
type SearchResult struct {
	Title         string   `json:"title,omitempty"`
	URL           string   `json:"url,omitempty"`
	Snippet       string   `json:"snippet,omitempty"`
	PublishedDate FlexDate `json:"publishedDate,omitzero"`
	Domain        string   `json:"domain,omitempty"`
	Content       string   `json:"content,omitempty"`
}

// FlexDate decodes the publishedDate field, which the live server returns as
// epoch milliseconds or null (captured 2026-08; a string form is accepted too
// in case the shape drifts). It normalizes to a YYYY-MM-DD string, which is
// what both the model and the page_age field want.
type FlexDate string

func (d *FlexDate) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	switch dec.PeekKind() {
	case 'n':
		_, err := dec.ReadToken()
		return err
	case '"':
		var s string
		if err := json.UnmarshalDecode(dec, &s); err != nil {
			return err
		}
		*d = FlexDate(s)
		return nil
	default:
		var ms int64
		if err := json.UnmarshalDecode(dec, &ms); err != nil {
			return err
		}
		if ms > 0 {
			*d = FlexDate(time.UnixMilli(ms).UTC().Format("2006-01-02"))
		}
		return nil
	}
}

func (d FlexDate) MarshalJSONTo(enc *jsontext.Encoder) error {
	return json.MarshalEncode(enc, string(d))
}

// ParseSearchResults decodes the JSON text a web_search call returns. ok is
// false when the text is not the expected {"results": [...]} shape, in which
// case callers should pass the raw text through untouched.
func ParseSearchResults(text string) (results []SearchResult, ok bool) {
	var parsed struct {
		Results []SearchResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil, false
	}
	return parsed.Results, parsed.Results != nil
}

// MarshalSearchResults re-encodes results into the tool_result JSON the model
// reads. Falls back to an empty results array on the (unreachable in practice)
// marshal error.
func MarshalSearchResults(results []SearchResult) string {
	out, err := json.Marshal(map[string]any{"results": results})
	if err != nil {
		return `{"results":[]}`
	}
	return string(out)
}

// EncodeResultContent packs page text into the encrypted_content field of a
// web_search_result block. The field is opaque to clients — Claude Code stores
// and replays it verbatim, exactly as it does with Anthropic's encrypted
// payloads — which makes it a free round-trip carrier: search results survive
// into later turns without the client needing to understand them.
func EncodeResultContent(text string) string {
	if text == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(text))
}

// DecodeResultContent unpacks EncodeResultContent. ok is false for content
// kirocc did not produce (for example genuine Anthropic encrypted payloads,
// which are not valid UTF-8 text after base64 decoding).
func DecodeResultContent(s string) (text string, ok bool) {
	if s == "" {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || !utf8.Valid(raw) {
		return "", false
	}
	return string(raw), true
}

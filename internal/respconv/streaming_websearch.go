package respconv

import (
	"github.com/d-kuro/kirocc/internal/anthropic"
)

// WriteWebSearchResult writes a web_search_tool_result content block. content
// is either a slice of web_search_result maps or a single error map, matching
// the shapes Anthropic's own API streams.
func (s *SSEWriter) WriteWebSearchResult(toolUseID string, content any) {
	s.ensureStarted()
	s.fireVisibleOutput()
	s.writeBlock(
		map[string]any{
			"type":        anthropic.BlockTypeWebSearchToolResult,
			"tool_use_id": toolUseID,
			"content":     content,
		},
		nil,
	)
}

// Text returns the visible text accumulated by the current round's
// accumulator. The tool-search orchestrator uses it to carry the model's
// pre-search preamble into the synthetic history, so the next round does not
// repeat what the client has already been streamed.
func (s *SSEWriter) Text() string {
	return s.acc.TextBuf.String()
}

package reqconv

import (
	"fmt"
	"strings"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiromcp"
)

// textualizeWebSearchBlocks converts replayed web-search blocks into plain
// text. When kirocc runs a search visibly it emits server_tool_use +
// web_search_tool_result blocks; Claude Code stores them in the transcript and
// sends them back on every later request. Kiro has no notion of these blocks,
// so they are downgraded to assistant text here — which is precisely what
// makes past search results part of the model's memory in later turns.
//
// Runs unconditionally (not gated on the web-search flag): a conversation may
// carry blocks from a turn when the feature was on.
func textualizeWebSearchBlocks(msgs []anthropic.Message) []anthropic.Message {
	result := make([]anthropic.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Content.IsString() || !hasWebSearchBlocks(msg.Content.Blocks) {
			result = append(result, msg)
			continue
		}
		newBlocks := make([]anthropic.ContentBlock, 0, len(msg.Content.Blocks))
		for _, b := range msg.Content.Blocks {
			switch {
			case b.Type == anthropic.BlockTypeServerToolUse && b.Name == kiromcp.WebSearchToolName:
				query, _ := b.Input["query"].(string)
				newBlocks = append(newBlocks, anthropic.ContentBlock{
					Type: anthropic.BlockTypeText,
					Text: "[web_search: " + query + "]",
				})
			case b.Type == anthropic.BlockTypeWebSearchToolResult:
				newBlocks = append(newBlocks, anthropic.ContentBlock{
					Type: anthropic.BlockTypeText,
					Text: webSearchResultText(b),
				})
			default:
				newBlocks = append(newBlocks, b)
			}
		}
		result = append(result, anthropic.Message{
			Role:    msg.Role,
			Content: anthropic.MessageContent{Blocks: newBlocks},
		})
	}
	return result
}

func hasWebSearchBlocks(blocks []anthropic.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == anthropic.BlockTypeWebSearchToolResult ||
			(b.Type == anthropic.BlockTypeServerToolUse && b.Name == kiromcp.WebSearchToolName) {
			return true
		}
	}
	return false
}

// webSearchResultText renders a web_search_tool_result block as readable text:
// one entry per result with title/url/date, followed by the page content
// recovered from the encrypted_content carrier.
func webSearchResultText(b anthropic.ContentBlock) string {
	var sb strings.Builder
	sb.WriteString("[web_search results]")
	for _, inner := range b.Content.Blocks {
		switch inner.Type {
		case anthropic.BlockTypeWebSearchResult:
			sb.WriteString("\n- ")
			sb.WriteString(inner.Title)
			if inner.URL != "" {
				fmt.Fprintf(&sb, " (%s)", inner.URL)
			}
			if inner.PageAge != "" {
				fmt.Fprintf(&sb, " [%s]", inner.PageAge)
			}
			if content, ok := kiromcp.DecodeResultContent(inner.EncryptedContent); ok {
				sb.WriteString("\n")
				sb.WriteString(content)
			}
		case anthropic.BlockTypeWebSearchResultError:
			sb.WriteString("\n[web_search error: " + inner.ErrorCode + "]")
		}
	}
	return sb.String()
}

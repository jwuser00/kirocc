package reqconv

import (
	"strings"
	"testing"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiromcp"
)

func TestTextualizeWebSearchBlocks_ReplayBecomesAssistantText(t *testing.T) {
	// The shape Claude Code replays after a visible search: server_tool_use +
	// web_search_tool_result inside one assistant message, then the answer.
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Text: "latest go version?"}},
		{Role: "assistant", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			{
				Type:  anthropic.BlockTypeServerToolUse,
				ID:    "srvtoolu_1",
				Name:  kiromcp.WebSearchToolName,
				Input: map[string]any{"query": "latest go release"},
			},
			{
				Type:      anthropic.BlockTypeWebSearchToolResult,
				ToolUseID: "srvtoolu_1",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{
					Type:             anthropic.BlockTypeWebSearchResult,
					Title:            "Go 1.26.1 Released",
					URL:              "https://go.dev/blog",
					PageAge:          "2026-08-01",
					EncryptedContent: kiromcp.EncodeResultContent("Go 1.26.1 fixes the scheduler deadlock."),
				}}},
			},
			{Type: anthropic.BlockTypeText, Text: "The latest is Go 1.26.1."},
		}}},
	}

	out := textualizeWebSearchBlocks(msgs)
	blocks := out[1].Content.Blocks
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	for i, b := range blocks {
		if b.Type != anthropic.BlockTypeText {
			t.Errorf("blocks[%d].Type = %q, want text", i, b.Type)
		}
	}
	if !strings.Contains(blocks[0].Text, "latest go release") {
		t.Errorf("query text = %q", blocks[0].Text)
	}
	result := blocks[1].Text
	for _, want := range []string{"Go 1.26.1 Released", "https://go.dev/blog", "2026-08-01", "scheduler deadlock"} {
		if !strings.Contains(result, want) {
			t.Errorf("result text missing %q:\n%s", want, result)
		}
	}
}

func TestTextualizeWebSearchBlocks_ErrorResult(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "assistant", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{
			Type:      anthropic.BlockTypeWebSearchToolResult,
			ToolUseID: "srvtoolu_1",
			Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{
				Type:      anthropic.BlockTypeWebSearchResultError,
				ErrorCode: "unavailable",
			}}},
		}}}},
	}
	out := textualizeWebSearchBlocks(msgs)
	if got := out[0].Content.Blocks[0].Text; !strings.Contains(got, "unavailable") {
		t.Errorf("error text = %q", got)
	}
}

func TestTextualizeWebSearchBlocks_ForeignEncryptedContentSkipped(t *testing.T) {
	// Genuine Anthropic encrypted payloads are not our base64 text; the entry
	// must keep its title/url line and simply omit the content.
	msgs := []anthropic.Message{
		{Role: "assistant", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{
			Type: anthropic.BlockTypeWebSearchToolResult,
			Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{
				Type:             anthropic.BlockTypeWebSearchResult,
				Title:            "T",
				URL:              "https://x.test",
				EncryptedContent: "%%%not-base64%%%",
			}}},
		}}}},
	}
	out := textualizeWebSearchBlocks(msgs)
	got := out[0].Content.Blocks[0].Text
	if !strings.Contains(got, "https://x.test") {
		t.Errorf("title/url line missing: %q", got)
	}
	if strings.Contains(got, "%%%") {
		t.Errorf("undecodable content leaked: %q", got)
	}
}

func TestTextualizeWebSearchBlocks_OtherMessagesUntouched(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Text: "hi"}},
		{Role: "assistant", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			{Type: anthropic.BlockTypeToolUse, ID: "t1", Name: "Read", Input: map[string]any{"path": "a"}},
		}}},
	}
	out := textualizeWebSearchBlocks(msgs)
	if out[1].Content.Blocks[0].Type != anthropic.BlockTypeToolUse {
		t.Errorf("unrelated tool_use rewritten: %+v", out[1].Content.Blocks[0])
	}
}

func TestNormalize_ReplayedWebSearchSurvivesIntoHistoryText(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Text: "q"}},
		{Role: "assistant", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			{
				Type:  anthropic.BlockTypeServerToolUse,
				ID:    "srvtoolu_1",
				Name:  kiromcp.WebSearchToolName,
				Input: map[string]any{"query": "golang release"},
			},
			{
				Type:      anthropic.BlockTypeWebSearchToolResult,
				ToolUseID: "srvtoolu_1",
				Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{
					Type:             anthropic.BlockTypeWebSearchResult,
					Title:            "Release notes",
					EncryptedContent: kiromcp.EncodeResultContent("the full page content"),
				}}},
			},
		}}},
		{Role: "user", Content: anthropic.MessageContent{Text: "and now?"}},
	}
	normalized := Normalize(msgs, true)
	text := ExtractTextContent(normalized[1].Content)
	if !strings.Contains(text, "the full page content") {
		t.Errorf("history text lost replayed search content: %q", text)
	}
}

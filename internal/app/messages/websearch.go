package messages

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiromcp"
	"github.com/d-kuro/kirocc/internal/toolsearch"
	"github.com/google/uuid"
)

// maxWebSearchRounds bounds how many web searches one client request may
// trigger. Each round is a full extra Kiro round-trip, so this caps both
// latency and credit spend if a model gets stuck searching.
const maxWebSearchRounds = 3

// dropToolNames lists the tools kirocc executes itself and therefore hides
// from the client stream. The model calls them; Claude Code never learns they
// existed and only receives the final answer.
func (o *toolSearchOrchestrator) dropToolNames() []string {
	var names []string
	if o.tsCtx != nil {
		names = append(names, toolsearch.KiroToolSearchName)
	}
	if o.webSearch {
		names = append(names, kiromcp.WebSearchToolName)
	}
	return names
}

// isIntercepted reports whether a tool_use with this name is handled inside the
// orchestrator rather than forwarded to the client.
func (o *toolSearchOrchestrator) isIntercepted(name string) bool {
	switch name {
	case kiromcp.WebSearchToolName:
		return o.webSearch
	default:
		return o.tsCtx != nil && name == toolsearch.KiroToolSearchName
	}
}

// parseWebSearchInput extracts the query from a web_search tool_use payload.
func parseWebSearchInput(input string) (string, error) {
	var parsed struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		return "", fmt.Errorf("parse web_search input: %w", err)
	}
	if parsed.Query == "" {
		return "", fmt.Errorf("parse web_search input: empty query")
	}
	return kiromcp.TrimQuery(parsed.Query), nil
}

// executeWebSearch runs the query against the Kiro-hosted MCP server.
//
// A transport failure is not propagated to the caller as an error: the model
// asked for a search and is owed an answer either way, so the failure comes
// back as tool-error text it can reason about (retry, rephrase, or proceed
// without). Failing the whole conversation because a search failed would be a
// worse outcome than the one this feature exists to fix.
func (o *toolSearchOrchestrator) executeWebSearch(ctx context.Context, short string, round int, query string) (text string, isError bool) {
	res, err := o.service.mcp.CallTool(ctx, o.creds.AccessToken, o.creds.ProfileARN, o.creds.Region,
		kiromcp.WebSearchToolName, map[string]any{"query": query})
	if err != nil {
		slog.WarnContext(ctx, "web search failed",
			"trace_id", short, "round", round+1, "query", query, "err", err)
		return "web_search failed: " + err.Error(), true
	}

	slog.InfoContext(ctx, "web search executed",
		"trace_id", short, "round", round+1, "query", query,
		"result_bytes", len(res.Text), "tool_error", res.IsError)
	return res.Text, res.IsError
}

// appendWebSearchMessages records the search as a normal tool_use/tool_result
// exchange so the next Kiro round sees the results as conversation history.
//
// The assistant block uses tool_use rather than server_tool_use: from Kiro's
// point of view web_search is an ordinary client-side tool that kirocc happens
// to run, and the history has to match the tool it was offered.
func appendWebSearchMessages(msgs []anthropic.Message, toolUseID, query, resultText string, isError bool) []anthropic.Message {
	return append(msgs,
		anthropic.Message{
			Role: "assistant",
			Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{
				Type:  anthropic.BlockTypeToolUse,
				ID:    toolUseID,
				Name:  kiromcp.WebSearchToolName,
				Input: map[string]any{"query": query},
			}}},
		},
		anthropic.Message{
			Role: "user",
			Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{
				Type:      anthropic.BlockTypeToolResult,
				ToolUseID: toolUseID,
				Content:   anthropic.MessageContent{Text: resultText},
				IsError:   isError,
			}}},
		},
	)
}

// newWebSearchToolUseID mints an id for the synthetic tool_use block.
func newWebSearchToolUseID() string {
	return "toolu_" + uuid.New().String()[:24]
}

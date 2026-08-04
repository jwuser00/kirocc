package messages

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiromcp"
	"github.com/d-kuro/kirocc/internal/reqconv"
	"github.com/d-kuro/kirocc/internal/toolsearch"
	"github.com/google/uuid"
)

const (
	// maxWebSearchRounds bounds how many re-request cycles web search may
	// trigger. Each round is a full extra Kiro round-trip, so this caps both
	// latency and credit spend if a model gets stuck searching.
	maxWebSearchRounds = 3
	// maxWebSearchQueries bounds total searches per client request across all
	// rounds. A round may fan out several queries in parallel (they share one
	// Kiro round-trip), so the query budget is separate from the round budget.
	// The client's max_uses declaration lowers it further.
	maxWebSearchQueries = 10
	// maxQueriesPerCall clamps the fan-out of a single tool_use.
	maxQueriesPerCall = 5
)

// dropToolNames lists the tools kirocc executes itself and therefore hides
// from the client stream. The model calls them; the client never sees the raw
// tool_use — visible mode re-emits searches as server_tool_use blocks instead.
func (o *toolSearchOrchestrator) dropToolNames() []string {
	var names []string
	if o.tsCtx != nil {
		names = append(names, toolsearch.KiroToolSearchName)
	}
	if o.wsOpts != nil {
		names = append(names, kiromcp.WebSearchToolName)
	}
	return names
}

// isIntercepted reports whether a tool_use with this name is handled inside the
// orchestrator rather than forwarded to the client.
func (o *toolSearchOrchestrator) isIntercepted(name string) bool {
	switch name {
	case kiromcp.WebSearchToolName:
		return o.wsOpts != nil
	default:
		return o.tsCtx != nil && name == toolsearch.KiroToolSearchName
	}
}

// webSearchCall is one executed query and everything both consumers need: the
// raw text for the model-facing tool_result, and the parsed results for the
// client-facing web_search_tool_result block.
type webSearchCall struct {
	query   string
	srvID   string                 // server_tool_use id, set by the orchestrator in visible mode
	results []kiromcp.SearchResult // nil when the response was not parseable
	raw     string                 // tool_result text the model reads
	isError bool
}

// parseWebSearchQueries extracts the query fan-out from a web_search tool_use
// payload: the required query plus optional additional_queries, deduplicated,
// trimmed to the MCP server's length cap, and clamped to maxQueriesPerCall.
func parseWebSearchQueries(input string) ([]string, error) {
	var parsed struct {
		Query             string   `json:"query"`
		AdditionalQueries []string `json:"additional_queries"`
	}
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		return nil, fmt.Errorf("parse web_search input: %w", err)
	}
	seen := make(map[string]struct{})
	var queries []string
	for _, q := range append([]string{parsed.Query}, parsed.AdditionalQueries...) {
		q = kiromcp.TrimQuery(strings.TrimSpace(q))
		if q == "" {
			continue
		}
		if _, dup := seen[q]; dup {
			continue
		}
		seen[q] = struct{}{}
		queries = append(queries, q)
		if len(queries) == maxQueriesPerCall {
			break
		}
	}
	if len(queries) == 0 {
		return nil, fmt.Errorf("parse web_search input: empty query")
	}
	return queries, nil
}

// executeWebSearches runs all queries of one round concurrently. Queries share
// a single Kiro round-trip either way, so latency is the slowest query, not
// the sum.
func (o *toolSearchOrchestrator) executeWebSearches(ctx context.Context, short string, round int, queries []string) []webSearchCall {
	calls := make([]webSearchCall, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Go(func() {
			calls[i] = o.executeWebSearch(ctx, short, round, q)
		})
	}
	wg.Wait()
	return calls
}

// executeWebSearch runs one query against the Kiro-hosted MCP server and
// enriches the results with fetched page content.
//
// A transport failure is not propagated to the caller as an error: the model
// asked for a search and is owed an answer either way, so the failure comes
// back as tool-error text it can reason about (retry, rephrase, or proceed
// without). Failing the whole conversation because a search failed would be a
// worse outcome than the one this feature exists to fix.
func (o *toolSearchOrchestrator) executeWebSearch(ctx context.Context, short string, round int, query string) webSearchCall {
	call := webSearchCall{query: query}

	res, err := o.service.mcp.CallTool(ctx, o.creds.AccessToken, o.creds.ProfileARN, o.creds.Region,
		kiromcp.WebSearchToolName, map[string]any{"query": query})
	if err != nil {
		slog.WarnContext(ctx, "web search failed",
			"trace_id", short, "round", round+1, "query", query, "err", err)
		call.raw = "web_search failed: " + err.Error()
		call.isError = true
		return call
	}

	call.raw = res.Text
	call.isError = res.IsError
	if !res.IsError {
		if results, ok := kiromcp.ParseSearchResults(res.Text); ok {
			results = filterByDomains(results, o.wsOpts)
			fetched := o.service.enrichResults(ctx, results)
			call.results = results
			call.raw = kiromcp.MarshalSearchResults(results)
			slog.InfoContext(ctx, "web search executed",
				"trace_id", short, "round", round+1, "query", query,
				"results", len(results), "pages_fetched", fetched, "result_bytes", len(call.raw))
			return call
		}
	}
	slog.InfoContext(ctx, "web search executed",
		"trace_id", short, "round", round+1, "query", query,
		"result_bytes", len(call.raw), "tool_error", call.isError)
	return call
}

// enrichResults fetches the top result pages in parallel and attaches their
// readable text, returning how many pages were successfully fetched. Snippets
// tell the model that a page is relevant; content is what lets it answer from
// the page — this is the substance gap between the Kiro-hosted search and
// Anthropic's native one.
func (s *Service) enrichResults(ctx context.Context, results []kiromcp.SearchResult) (fetched int) {
	if s.webFetcher == nil || s.webFetchCount <= 0 {
		return 0
	}
	var urls []string
	var idx []int
	for i, r := range results {
		if r.URL == "" {
			continue
		}
		urls = append(urls, r.URL)
		idx = append(idx, i)
		if len(urls) == s.webFetchCount {
			break
		}
	}
	if len(urls) == 0 {
		return 0
	}
	for k, page := range s.webFetcher.FetchAll(ctx, urls, s.webFetchBytes) {
		if page.Err != nil {
			slog.DebugContext(ctx, "web fetch failed", "url", page.URL, "err", page.Err)
			continue
		}
		if page.Content == "" {
			continue
		}
		results[idx[k]].Content = page.Content
		fetched++
	}
	return fetched
}

// filterByDomains applies the client declaration's allowed/blocked domain
// lists to search results. Domains cover their subdomains, matching the
// Anthropic API's semantics.
func filterByDomains(results []kiromcp.SearchResult, opts *reqconv.WebSearchOptions) []kiromcp.SearchResult {
	if opts == nil || (len(opts.AllowedDomains) == 0 && len(opts.BlockedDomains) == 0) {
		return results
	}
	kept := make([]kiromcp.SearchResult, 0, len(results))
	for _, r := range results {
		host := resultHost(r)
		if len(opts.AllowedDomains) > 0 && !matchesAnyDomain(host, opts.AllowedDomains) {
			continue
		}
		if len(opts.BlockedDomains) > 0 && matchesAnyDomain(host, opts.BlockedDomains) {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

func resultHost(r kiromcp.SearchResult) string {
	if u, err := url.Parse(r.URL); err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	return strings.ToLower(r.Domain)
}

func matchesAnyDomain(host string, domains []string) bool {
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// webSearchResultContent builds the content of the client-facing
// web_search_tool_result block: web_search_result entries on success, the
// error object shape otherwise. Fetched page text travels in the
// encrypted_content carrier so it survives transcript replay into later turns.
func webSearchResultContent(call webSearchCall) any {
	if call.isError || call.results == nil {
		return map[string]any{
			"type":       anthropic.BlockTypeWebSearchResultError,
			"error_code": "unavailable",
		}
	}
	blocks := make([]map[string]any, 0, len(call.results))
	for _, r := range call.results {
		blocks = append(blocks, map[string]any{
			"type":              anthropic.BlockTypeWebSearchResult,
			"title":             r.Title,
			"url":               r.URL,
			"page_age":          string(r.PublishedDate),
			"encrypted_content": kiromcp.EncodeResultContent(resultCarrierText(r)),
		})
	}
	return blocks
}

// resultCarrierText is what a result contributes to later turns via the
// encrypted_content round-trip: the snippet plus the fetched page text.
func resultCarrierText(r kiromcp.SearchResult) string {
	switch {
	case r.Snippet != "" && r.Content != "":
		return r.Snippet + "\n\n" + r.Content
	case r.Content != "":
		return r.Content
	default:
		return r.Snippet
	}
}

// appendWebSearchMessages records the round's searches as one tool_use/
// tool_result exchange so the next Kiro round sees them as conversation
// history. The preamble — text the model emitted before deciding to search,
// already streamed to the client — leads the assistant message so the next
// round does not repeat it.
//
// The assistant blocks use tool_use rather than server_tool_use: from Kiro's
// point of view web_search is an ordinary client-side tool that kirocc happens
// to run, and the history has to match the tool it was offered.
func appendWebSearchMessages(msgs []anthropic.Message, preamble string, calls []webSearchCall) []anthropic.Message {
	var assistantBlocks []anthropic.ContentBlock
	if strings.TrimSpace(preamble) != "" {
		assistantBlocks = append(assistantBlocks, anthropic.ContentBlock{
			Type: anthropic.BlockTypeText,
			Text: preamble,
		})
	}
	userBlocks := make([]anthropic.ContentBlock, 0, len(calls))
	for _, call := range calls {
		id := "toolu_" + uuid.New().String()[:24]
		assistantBlocks = append(assistantBlocks, anthropic.ContentBlock{
			Type:  anthropic.BlockTypeToolUse,
			ID:    id,
			Name:  kiromcp.WebSearchToolName,
			Input: map[string]any{"query": call.query},
		})
		userBlocks = append(userBlocks, anthropic.ContentBlock{
			Type:      anthropic.BlockTypeToolResult,
			ToolUseID: id,
			Content:   anthropic.MessageContent{Text: call.raw},
			IsError:   call.isError,
		})
	}
	return append(msgs,
		anthropic.Message{
			Role:    "assistant",
			Content: anthropic.MessageContent{Blocks: assistantBlocks},
		},
		anthropic.Message{
			Role:    "user",
			Content: anthropic.MessageContent{Blocks: userBlocks},
		},
	)
}

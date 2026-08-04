package server

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/kiromcp"
	"github.com/d-kuro/kirocc/internal/webfetch"
)

// fakeMCP records tools/call invocations and replays scripted results.
// Queries within one round run concurrently, so access is mutex-guarded.
type fakeMCP struct {
	mu      sync.Mutex
	result  *kiromcp.Result
	err     error
	queries []string
	tokens  []string
}

func (f *fakeMCP) CallTool(_ context.Context, token, _, _, name string, args map[string]any) (*kiromcp.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	q, _ := args["query"].(string)
	f.queries = append(f.queries, name+":"+q)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeMCP) ListTools(context.Context, string, string, string) ([]kiromcp.Tool, error) {
	return []kiromcp.Tool{{Name: kiromcp.WebSearchToolName}}, nil
}

func (f *fakeMCP) sortedQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.queries)
	slices.Sort(out)
	return out
}

func newWebSearchServer(t *testing.T, client kiroclient.Client, mcp kiromcp.Client, opts ...ServerOption) *httptest.Server {
	t.Helper()
	mgr := &mockAuthManager{creds: &auth.Credentials{
		AccessToken: "test-token",
		ProfileARN:  "arn:test",
		Region:      "us-east-1",
	}}
	s := New(mgr, "", client, append([]ServerOption{WithMCPClient(mcp)}, opts...)...)
	return newTCP4TestServer(t, s.Handler())
}

// webSearchRequest is a Claude Code-shaped request: WebSearch declared as a
// server tool with no input_schema, which is what used to sink the request.
const webSearchRequest = `{
	"model": "claude-sonnet-4-6",
	"max_tokens": 1024,
	"stream": false,
	"messages": [{"role": "user", "content": "What is the latest Go release?"}],
	"tools": [
		{"type": "web_search_20250305", "name": "web_search"},
		{"name": "Read", "description": "Read a file", "input_schema": {"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}
	]
}`

func webSearchToolUseEvents() []any {
	return webSearchToolUseEventsInput(map[string]any{"query": "latest Go release"})
}

func webSearchToolUseEventsInput(input map[string]any) []any {
	return []any{"toolUseEvent", mustJSON(map[string]any{
		"name":      kiromcp.WebSearchToolName,
		"toolUseId": "tool_ws_1",
		"input":     input,
		"stop":      true,
	})}
}

func finalTextEvents(text string) []any {
	return []any{"assistantResponseEvent", mustJSON(map[string]string{"content": text})}
}

// contentBlocks decodes the JSON response and returns its content array.
func contentBlocks(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	raw, _ := result["content"].([]any)
	blocks := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			blocks = append(blocks, m)
		}
	}
	return blocks
}

func TestE2E_WebSearch_RawToolUseHiddenSearchVisible(t *testing.T) {
	mcp := &fakeMCP{result: &kiromcp.Result{Text: `{"results":[{"title":"Go 1.26.1","url":"https://go.dev","snippet":"released"}]}`}}
	client := &multiResponseClient{responses: [][]any{
		webSearchToolUseEvents(),
		finalTextEvents("Go 1.26.1 is the latest release."),
	}}

	srv := newWebSearchServer(t, client, mcp)
	defer srv.Close()

	resp := postMessages(t, srv.URL, webSearchRequest)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	// The search must have actually run, with the model's query.
	if got := mcp.sortedQueries(); len(got) != 1 || got[0] != kiromcp.WebSearchToolName+":latest Go release" {
		t.Fatalf("mcp queries = %v", got)
	}
	if len(mcp.tokens) != 1 || mcp.tokens[0] != "test-token" {
		t.Errorf("mcp called with tokens %v, want the Kiro bearer token", mcp.tokens)
	}

	// Two Kiro round-trips: the one that asked to search, and the one that answered.
	if client.callCount != 2 {
		t.Fatalf("upstream calls = %d, want 2", client.callCount)
	}

	blocks := contentBlocks(t, resp)
	var text strings.Builder
	var sawServerToolUse, sawResult bool
	for _, block := range blocks {
		switch block["type"] {
		case "tool_use":
			// The raw tool_use must never leak; the search surfaces only in
			// Anthropic's native server-tool shape.
			t.Errorf("web_search leaked to client as tool_use block: %v", block)
		case "server_tool_use":
			sawServerToolUse = true
			if block["name"] != kiromcp.WebSearchToolName {
				t.Errorf("server_tool_use name = %v", block["name"])
			}
		case "web_search_tool_result":
			sawResult = true
			results, _ := block["content"].([]any)
			if len(results) != 1 {
				t.Fatalf("web_search_tool_result content = %v", block["content"])
			}
			entry, _ := results[0].(map[string]any)
			if entry["type"] != "web_search_result" || entry["url"] != "https://go.dev" {
				t.Errorf("result entry = %v", entry)
			}
			// The snippet must round-trip through the encrypted_content carrier.
			enc, _ := entry["encrypted_content"].(string)
			if decoded, ok := kiromcp.DecodeResultContent(enc); !ok || !strings.Contains(decoded, "released") {
				t.Errorf("encrypted_content decoded = %q, %v", decoded, ok)
			}
		case "text":
			if s, ok := block["text"].(string); ok {
				text.WriteString(s)
			}
		}
	}
	if !sawServerToolUse || !sawResult {
		t.Errorf("visible search blocks missing: server_tool_use=%v result=%v", sawServerToolUse, sawResult)
	}
	if !strings.Contains(text.String(), "Go 1.26.1") {
		t.Errorf("final text = %q, want the answer derived from search results", text.String())
	}
}

func TestE2E_WebSearch_InvisibleModeHidesEverything(t *testing.T) {
	mcp := &fakeMCP{result: &kiromcp.Result{Text: `{"results":[{"title":"Go 1.26.1"}]}`}}
	client := &multiResponseClient{responses: [][]any{
		webSearchToolUseEvents(),
		finalTextEvents("done"),
	}}

	srv := newWebSearchServer(t, client, mcp, WithWebSearchVisible(false))
	defer srv.Close()

	resp := postMessages(t, srv.URL, webSearchRequest)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	for _, block := range contentBlocks(t, resp) {
		if block["type"] == "tool_use" || block["type"] == "server_tool_use" || block["type"] == "web_search_tool_result" {
			t.Errorf("search leaked in invisible mode as %v block: %v", block["type"], block)
		}
	}
}

func TestE2E_WebSearch_MultiQueryFansOutInOneRound(t *testing.T) {
	mcp := &fakeMCP{result: &kiromcp.Result{Text: `{"results":[{"title":"r"}]}`}}
	client := &multiResponseClient{responses: [][]any{
		webSearchToolUseEventsInput(map[string]any{
			"query":              "go 1.26 release notes",
			"additional_queries": []string{"go 1.26 changelog", "go 1.26 runtime changes"},
		}),
		finalTextEvents("summarized"),
	}}

	srv := newWebSearchServer(t, client, mcp)
	defer srv.Close()

	resp := postMessages(t, srv.URL, webSearchRequest)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	// All three queries execute, sharing a single extra Kiro round-trip.
	want := []string{
		"web_search:go 1.26 changelog",
		"web_search:go 1.26 release notes",
		"web_search:go 1.26 runtime changes",
	}
	if got := mcp.sortedQueries(); !slices.Equal(got, want) {
		t.Errorf("queries = %v, want %v", got, want)
	}
	if client.callCount != 2 {
		t.Errorf("upstream calls = %d, want 2 (fan-out must not add rounds)", client.callCount)
	}

	// One server_tool_use + result pair per query.
	var uses, results int
	for _, block := range contentBlocks(t, resp) {
		switch block["type"] {
		case "server_tool_use":
			uses++
		case "web_search_tool_result":
			results++
		}
	}
	if uses != 3 || results != 3 {
		t.Errorf("visible blocks: %d uses, %d results, want 3 each", uses, results)
	}
}

func TestE2E_WebSearch_FetchEnrichesResultsWithPageContent(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Go Blog</title></head><body><article><p>` +
			`Go 1.26.1 fixes a rare scheduler deadlock and improves build cache hits on macOS in real projects.` +
			`</p></article></body></html>`))
	}))
	defer page.Close()

	mcp := &fakeMCP{result: &kiromcp.Result{Text: `{"results":[{"title":"Go Blog","url":"` + page.URL + `","snippet":"fixes"}]}`}}
	client := &multiResponseClient{responses: [][]any{
		webSearchToolUseEvents(),
		finalTextEvents("answered from page"),
	}}

	fetcher := webfetch.New(webfetch.WithHTTPClient(&http.Client{Timeout: 5 * time.Second}))
	srv := newWebSearchServer(t, client, mcp, WithWebFetch(fetcher, 3, 4096))
	defer srv.Close()

	resp := postMessages(t, srv.URL, webSearchRequest)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	// The follow-up Kiro request must carry the fetched page text, not only
	// the snippet — that is the whole point of enrichment.
	raw, err := json.Marshal(client.payloads[1])
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if !strings.Contains(string(raw), "scheduler deadlock") {
		t.Errorf("second payload missing fetched page content: %s", raw)
	}

	// And the visible result block must carry it in the round-trip carrier.
	var carried bool
	for _, block := range contentBlocks(t, resp) {
		if block["type"] != "web_search_tool_result" {
			continue
		}
		results, _ := block["content"].([]any)
		for _, r := range results {
			entry, _ := r.(map[string]any)
			enc, _ := entry["encrypted_content"].(string)
			if decoded, ok := kiromcp.DecodeResultContent(enc); ok && strings.Contains(decoded, "scheduler deadlock") {
				carried = true
			}
		}
	}
	if !carried {
		t.Error("fetched content missing from encrypted_content carrier")
	}
}

func TestE2E_WebSearch_SecondRoundSeesResultsInHistory(t *testing.T) {
	mcp := &fakeMCP{result: &kiromcp.Result{Text: `{"results":[{"title":"Go 1.26.1"}]}`}}
	client := &multiResponseClient{responses: [][]any{
		webSearchToolUseEvents(),
		finalTextEvents("done"),
	}}

	srv := newWebSearchServer(t, client, mcp)
	defer srv.Close()

	resp := postMessages(t, srv.URL, webSearchRequest)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	if len(client.payloads) != 2 {
		t.Fatalf("payloads = %d, want 2", len(client.payloads))
	}
	// The follow-up request must carry the search output, otherwise the model
	// answers the second round with nothing to go on.
	raw, err := json.Marshal(client.payloads[1])
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if !strings.Contains(string(raw), "Go 1.26.1") {
		t.Errorf("second payload does not contain search results: %s", raw)
	}
}

func TestE2E_WebSearch_MaxUsesCapsQueries(t *testing.T) {
	mcp := &fakeMCP{result: &kiromcp.Result{Text: `{"results":[{"title":"r"}]}`}}
	// The model would happily search forever; max_uses=2 must stop it.
	client := &multiResponseClient{responses: [][]any{webSearchToolUseEvents()}}

	srv := newWebSearchServer(t, client, mcp)
	defer srv.Close()

	body := strings.Replace(webSearchRequest,
		`{"type": "web_search_20250305", "name": "web_search"}`,
		`{"type": "web_search_20250305", "name": "web_search", "max_uses": 2}`, 1)
	resp := postMessages(t, srv.URL, body)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	if got := len(mcp.sortedQueries()); got != 2 {
		t.Errorf("searches = %d, want 2 (client max_uses)", got)
	}
}

func TestE2E_WebSearch_BlockedDomainsFilterResults(t *testing.T) {
	mcp := &fakeMCP{result: &kiromcp.Result{Text: `{"results":[` +
		`{"title":"official","url":"https://go.dev/doc"},` +
		`{"title":"blogspam","url":"https://spam.example.com/post"}]}`}}
	client := &multiResponseClient{responses: [][]any{
		webSearchToolUseEvents(),
		finalTextEvents("done"),
	}}

	srv := newWebSearchServer(t, client, mcp)
	defer srv.Close()

	body := strings.Replace(webSearchRequest,
		`{"type": "web_search_20250305", "name": "web_search"}`,
		`{"type": "web_search_20250305", "name": "web_search", "blocked_domains": ["example.com"]}`, 1)
	resp := postMessages(t, srv.URL, body)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	raw, _ := json.Marshal(client.payloads[1])
	if !strings.Contains(string(raw), "go.dev") {
		t.Errorf("allowed result missing from follow-up payload: %s", raw)
	}
	if strings.Contains(string(raw), "spam.example.com") {
		t.Errorf("blocked domain leaked into follow-up payload: %s", raw)
	}
}

func TestE2E_WebSearch_ToolDeclaredWithSchema(t *testing.T) {
	mcp := &fakeMCP{result: &kiromcp.Result{Text: "{}"}}
	client := &multiResponseClient{responses: [][]any{finalTextEvents("hi")}}

	srv := newWebSearchServer(t, client, mcp)
	defer srv.Close()

	resp := postMessages(t, srv.URL, webSearchRequest)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	tools := client.payloads[0].ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	var names []string
	for _, entry := range tools {
		if entry.ToolSpecification == nil {
			continue
		}
		names = append(names, entry.ToolSpecification.Name)
		// Every tool Kiro receives must carry a schema; a bare {} makes it
		// reject the entire request.
		if entry.ToolSpecification.InputSchema.JSON["type"] != "object" {
			t.Errorf("tool %q sent without an object schema: %v",
				entry.ToolSpecification.Name, entry.ToolSpecification.InputSchema.JSON)
		}
	}
	if !slices.Contains(names, kiromcp.WebSearchToolName) {
		t.Errorf("tools = %v, want web_search present", names)
	}
}

func TestE2E_WebSearch_FailureBecomesToolErrorNotRequestFailure(t *testing.T) {
	mcp := &fakeMCP{err: context.DeadlineExceeded}
	client := &multiResponseClient{responses: [][]any{
		webSearchToolUseEvents(),
		finalTextEvents("I could not search, but here is what I know."),
	}}

	srv := newWebSearchServer(t, client, mcp)
	defer srv.Close()

	resp := postMessages(t, srv.URL, webSearchRequest)
	defer func() { _ = resp.Body.Close() }()

	// A failed search must not take the conversation down with it.
	requireStatus(t, resp, 200)
	if client.callCount != 2 {
		t.Fatalf("upstream calls = %d, want 2 (model gets a chance to respond)", client.callCount)
	}
	raw, _ := json.Marshal(client.payloads[1])
	if !strings.Contains(string(raw), "web_search failed") {
		t.Errorf("second payload should carry the tool error: %s", raw)
	}
}

func TestE2E_WebSearch_DisabledStripsToolAndSucceeds(t *testing.T) {
	client := &multiResponseClient{responses: [][]any{finalTextEvents("ok")}}

	// No MCP client => feature off.
	srv := newWebSearchServer(t, client, nil)
	defer srv.Close()

	resp := postMessages(t, srv.URL, webSearchRequest)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	tools := client.payloads[0].ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	for _, entry := range tools {
		if entry.ToolSpecification == nil {
			continue
		}
		if entry.ToolSpecification.Name == kiromcp.WebSearchToolName {
			t.Error("web_search offered while the feature is disabled")
		}
		if entry.ToolSpecification.InputSchema.JSON["type"] != "object" {
			t.Errorf("tool %q sent without an object schema", entry.ToolSpecification.Name)
		}
	}
}

func TestE2E_WebSearch_BudgetStopsRunawayLoop(t *testing.T) {
	mcp := &fakeMCP{result: &kiromcp.Result{Text: "{}"}}
	// Upstream always asks for another search; the budget has to break the loop.
	client := &multiResponseClient{responses: [][]any{webSearchToolUseEvents()}}

	srv := newWebSearchServer(t, client, mcp)
	defer srv.Close()

	resp := postMessages(t, srv.URL, webSearchRequest)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	// Exactly the round budget: fewer would mean interception never engaged,
	// more would mean the loop is unbounded.
	if got := len(mcp.sortedQueries()); got != 3 {
		t.Errorf("searches = %d, want exactly 3 (the web search round budget)", got)
	}
	// The loop must terminate rather than spin to the shared round cap.
	if client.callCount != 4 {
		t.Errorf("upstream calls = %d, want 4 (3 searches + the round that gives up)", client.callCount)
	}
}

func TestE2E_WebSearch_StreamingEmitsVisibleBlocksNotRawToolUse(t *testing.T) {
	mcp := &fakeMCP{result: &kiromcp.Result{Text: `{"results":[{"title":"Go 1.26.1"}]}`}}
	client := &multiResponseClient{responses: [][]any{
		webSearchToolUseEvents(),
		finalTextEvents("Go 1.26.1"),
	}}

	srv := newWebSearchServer(t, client, mcp)
	defer srv.Close()

	body := strings.Replace(webSearchRequest, `"stream": false`, `"stream": true`, 1)
	resp := postMessages(t, srv.URL, body)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	out := sb.String()
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Errorf("raw tool_use leaked into the SSE stream:\n%s", out)
	}
	if !strings.Contains(out, `"server_tool_use"`) || !strings.Contains(out, `"web_search_tool_result"`) {
		t.Errorf("stream missing visible search blocks:\n%s", out)
	}
	if !strings.Contains(out, "Go 1.26.1") {
		t.Errorf("stream missing final answer:\n%s", out)
	}
}

// compile-time assertion that the fake satisfies the client interface.
var _ kiromcp.Client = (*fakeMCP)(nil)

package server

import (
	"context"
	"encoding/json/v2"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/kiromcp"
)

// fakeMCP records tools/call invocations and replays scripted results.
type fakeMCP struct {
	result  *kiromcp.Result
	err     error
	queries []string
	tokens  []string
}

func (f *fakeMCP) CallTool(_ context.Context, token, _, _, name string, args map[string]any) (*kiromcp.Result, error) {
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

func newWebSearchServer(t *testing.T, client kiroclient.Client, mcp kiromcp.Client) *httptest.Server {
	t.Helper()
	mgr := &mockAuthManager{creds: &auth.Credentials{
		AccessToken: "test-token",
		ProfileARN:  "arn:test",
		Region:      "us-east-1",
	}}
	s := New(mgr, "", client, WithMCPClient(mcp))
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
	return []any{"toolUseEvent", mustJSON(map[string]any{
		"name":      kiromcp.WebSearchToolName,
		"toolUseId": "tool_ws_1",
		"input":     map[string]string{"query": "latest Go release"},
		"stop":      true,
	})}
}

func finalTextEvents(text string) []any {
	return []any{"assistantResponseEvent", mustJSON(map[string]string{"content": text})}
}

func TestE2E_WebSearch_InterceptedAndHiddenFromClient(t *testing.T) {
	mcp := &fakeMCP{result: &kiromcp.Result{Text: `{"results":[{"title":"Go 1.26.1","url":"https://go.dev"}]}`}}
	client := &multiResponseClient{responses: [][]any{
		webSearchToolUseEvents(),
		finalTextEvents("Go 1.26.1 is the latest release."),
	}}

	srv := newWebSearchServer(t, client, mcp)
	defer srv.Close()

	resp := postMessages(t, srv.URL, webSearchRequest)
	defer func() { _ = resp.Body.Close() }()
	requireStatus(t, resp, 200)

	var result map[string]any
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// The search must have actually run, with the model's query.
	if len(mcp.queries) != 1 || mcp.queries[0] != kiromcp.WebSearchToolName+":latest Go release" {
		t.Fatalf("mcp queries = %v", mcp.queries)
	}
	if len(mcp.tokens) != 1 || mcp.tokens[0] != "test-token" {
		t.Errorf("mcp called with tokens %v, want the Kiro bearer token", mcp.tokens)
	}

	// Two Kiro round-trips: the one that asked to search, and the one that answered.
	if client.callCount != 2 {
		t.Fatalf("upstream calls = %d, want 2", client.callCount)
	}

	// Claude Code must never see the web_search tool_use.
	content, _ := result["content"].([]any)
	var text strings.Builder
	for _, c := range content {
		block, _ := c.(map[string]any)
		if block["type"] == "tool_use" || block["type"] == "server_tool_use" {
			t.Errorf("web_search leaked to client as %v block: %v", block["type"], block)
		}
		if s, ok := block["text"].(string); ok {
			text.WriteString(s)
		}
	}
	if !strings.Contains(text.String(), "Go 1.26.1") {
		t.Errorf("final text = %q, want the answer derived from search results", text.String())
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
	if !slicesContains(names, kiromcp.WebSearchToolName) {
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

	// Exactly the budget: fewer would mean interception never engaged, more
	// would mean the loop is unbounded.
	if len(mcp.queries) != 3 {
		t.Errorf("searches = %d, want exactly 3 (the web search budget)", len(mcp.queries))
	}
	// The loop must terminate rather than spin to the shared round cap.
	if client.callCount != 4 {
		t.Errorf("upstream calls = %d, want 4 (3 searches + the round that gives up)", client.callCount)
	}
}

func TestE2E_WebSearch_StreamingHidesToolUse(t *testing.T) {
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
	if strings.Contains(out, kiromcp.WebSearchToolName) {
		t.Errorf("web_search leaked into the SSE stream:\n%s", out)
	}
	if !strings.Contains(out, "Go 1.26.1") {
		t.Errorf("stream missing final answer:\n%s", out)
	}
}

func slicesContains(s []string, v string) bool {
	return slices.Contains(s, v)
}

// compile-time assertion that the fake satisfies the client interface.
var _ kiromcp.Client = (*fakeMCP)(nil)

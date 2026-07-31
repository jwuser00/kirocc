package kiromcp

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// decodeRequest reads the JSON-RPC envelope the client sent.
func decodeRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return got
}

func TestCallTool_SendsJSONRPCEnvelopeAndHeaders(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = decodeRequest(t, r)

		if h := r.Header.Get("X-Amz-Target"); h != amzTarget {
			t.Errorf("X-Amz-Target = %q, want %q", h, amzTarget)
		}
		if h := r.Header.Get("Authorization"); h != "Bearer tok-123" {
			t.Errorf("Authorization = %q, want Bearer tok-123", h)
		}
		if h := r.Header.Get("Content-Type"); h != "application/x-amz-json-1.0" {
			t.Errorf("Content-Type = %q", h)
		}
		if h := r.Header.Get("X-Amzn-Kiro-Profile-Arn"); h != "arn:aws:codewhisperer:::profile/P1" {
			t.Errorf("profile arn header = %q", h)
		}

		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"{\"results\":[]}"}],"isError":false}}`)
	}))
	defer srv.Close()

	c := NewHTTPClient(WithBaseURL(srv.URL))
	res, err := c.CallTool(context.Background(), "tok-123", "arn:aws:codewhisperer:::profile/P1", "us-east-1",
		WebSearchToolName, map[string]any{"query": "go release"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text != `{"results":[]}` {
		t.Errorf("Text = %q", res.Text)
	}
	if res.IsError {
		t.Error("IsError = true, want false")
	}

	if got["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v", got["jsonrpc"])
	}
	if got["method"] != "tools/call" {
		t.Errorf("method = %v", got["method"])
	}
	// profileArn is a sibling of the JSON-RPC fields, not nested under params.
	if got["profileArn"] != "arn:aws:codewhisperer:::profile/P1" {
		t.Errorf("profileArn = %v", got["profileArn"])
	}
	params, _ := got["params"].(map[string]any)
	if params["name"] != WebSearchToolName {
		t.Errorf("params.name = %v", params["name"])
	}
	args, _ := params["arguments"].(map[string]any)
	if args["query"] != "go release" {
		t.Errorf("params.arguments.query = %v", args["query"])
	}
}

func TestCallTool_ToolErrorSurfacesAsResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"query too long"}],"isError":true}}`)
	}))
	defer srv.Close()

	res, err := NewHTTPClient(WithBaseURL(srv.URL)).
		CallTool(context.Background(), "t", "", "us-east-1", WebSearchToolName, map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("CallTool returned transport error for a tool-level failure: %v", err)
	}
	if !res.IsError || res.Text != "query too long" {
		t.Errorf("got %+v, want tool error with message", res)
	}
}

func TestCallTool_RPCErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"1","error":{"code":-32601,"message":"method not found"}}`)
	}))
	defer srv.Close()

	_, err := NewHTTPClient(WithBaseURL(srv.URL)).
		CallTool(context.Background(), "t", "", "us-east-1", WebSearchToolName, map[string]any{"query": "x"})
	if err == nil || !strings.Contains(err.Error(), "method not found") {
		t.Fatalf("err = %v, want rpc error", err)
	}
}

func TestCallTool_NoTextContentIsErrNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"1","result":{"content":[],"isError":false}}`)
	}))
	defer srv.Close()

	_, err := NewHTTPClient(WithBaseURL(srv.URL)).
		CallTool(context.Background(), "t", "", "us-east-1", WebSearchToolName, map[string]any{"query": "x"})
	if err == nil {
		t.Fatal("want ErrNoContent, got nil")
	}
}

func TestCallTool_RefreshesTokenOn403(t *testing.T) {
	var calls atomic.Int32
	var secondAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "expired")
			return
		}
		secondAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"ok"}]}}`)
	}))
	defer srv.Close()

	c := NewHTTPClient(
		WithBaseURL(srv.URL),
		WithTokenRefresher(func(context.Context) (string, error) { return "fresh", nil }),
	)
	res, err := c.CallTool(context.Background(), "stale", "", "us-east-1", WebSearchToolName, map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q", res.Text)
	}
	if secondAuth != "Bearer fresh" {
		t.Errorf("retry Authorization = %q, want Bearer fresh", secondAuth)
	}
}

func TestCallTool_ClientErrorDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "ValidationException")
	}))
	defer srv.Close()

	_, err := NewHTTPClient(WithBaseURL(srv.URL)).
		CallTool(context.Background(), "t", "", "us-east-1", WebSearchToolName, map[string]any{"query": "x"})
	if err == nil {
		t.Fatal("want error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1 (4xx must not retry)", n)
	}
}

func TestListTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := decodeRequest(t, r)
		if got["method"] != "tools/list" {
			t.Errorf("method = %v, want tools/list", got["method"])
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"1","result":{"tools":[{"name":"web_search","description":"d","inputSchema":{"type":"object"}}]}}`)
	}))
	defer srv.Close()

	tools, err := NewHTTPClient(WithBaseURL(srv.URL)).ListTools(context.Background(), "t", "", "us-east-1")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != WebSearchToolName {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestEndpointURL_DerivesFromRegion(t *testing.T) {
	c := NewHTTPClient()
	if got := c.endpointURL("eu-central-1"); got != "https://q.eu-central-1.amazonaws.com/" {
		t.Errorf("endpointURL = %q", got)
	}
}

func TestTrimQuery(t *testing.T) {
	if got := TrimQuery("short"); got != "short" {
		t.Errorf("TrimQuery(short) = %q", got)
	}
	long := strings.Repeat("가", MaxQueryLength+50)
	got := TrimQuery(long)
	if n := len([]rune(got)); n != MaxQueryLength {
		t.Errorf("TrimQuery length = %d runes, want %d", n, MaxQueryLength)
	}
}

func TestWebSearchToolEntry_HasUsableSchema(t *testing.T) {
	e := WebSearchToolEntry()
	if e.ToolSpecification == nil {
		t.Fatal("nil ToolSpecification")
	}
	if e.ToolSpecification.Name != WebSearchToolName {
		t.Errorf("Name = %q", e.ToolSpecification.Name)
	}
	// A schema-less tool is exactly what makes Kiro reject the whole request.
	schema := e.ToolSpecification.InputSchema.JSON
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Errorf("schema missing query property: %v", schema)
	}
}

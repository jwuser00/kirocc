package messages

import (
	"strings"
	"testing"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiromcp"
	"github.com/d-kuro/kirocc/internal/reqconv"
)

func TestParseWebSearchQueries(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{name: "single query", input: `{"query":"go release"}`, want: []string{"go release"}},
		{
			name:  "fan-out with additional queries",
			input: `{"query":"a","additional_queries":["b","c"]}`,
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "dedupe and drop empties",
			input: `{"query":"a","additional_queries":["a","","  ","b"]}`,
			want:  []string{"a", "b"},
		},
		{
			name:  "clamped to per-call cap",
			input: `{"query":"q1","additional_queries":["q2","q3","q4","q5","q6","q7"]}`,
			want:  []string{"q1", "q2", "q3", "q4", "q5"},
		},
		{name: "empty query", input: `{"query":""}`, wantErr: true},
		{name: "invalid json", input: `{`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseWebSearchQueries(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("queries = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("queries[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestParseWebSearchQueries_TrimsOverlongQuery(t *testing.T) {
	long := strings.Repeat("x", kiromcp.MaxQueryLength+50)
	got, err := parseWebSearchQueries(`{"query":"` + long + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0]) != kiromcp.MaxQueryLength {
		t.Errorf("query length = %d, want %d", len(got[0]), kiromcp.MaxQueryLength)
	}
}

func TestFilterByDomains(t *testing.T) {
	results := []kiromcp.SearchResult{
		{URL: "https://go.dev/doc"},
		{URL: "https://pkg.go.dev/net"},
		{URL: "https://spam.example.com/x"},
		{Domain: "example.com"}, // no URL: falls back to the domain field
	}
	allowed := filterByDomains(results, &reqconv.WebSearchOptions{AllowedDomains: []string{"go.dev"}})
	if len(allowed) != 2 {
		t.Errorf("allowed = %v, want the two go.dev results", allowed)
	}
	blocked := filterByDomains(results, &reqconv.WebSearchOptions{BlockedDomains: []string{"example.com"}})
	if len(blocked) != 2 {
		t.Errorf("blocked = %v, want example.com results dropped", blocked)
	}
	all := filterByDomains(results, nil)
	if len(all) != 4 {
		t.Errorf("nil opts filtered results: %v", all)
	}
}

func TestRoundBudget_WebSearchQueriesAndRounds(t *testing.T) {
	b := newRoundBudget(nil)
	if !b.allowWebSearchRound() {
		t.Fatal("first round refused")
	}
	if got := b.takeQueries(4); got != 4 {
		t.Fatalf("takeQueries(4) = %d", got)
	}
	if !b.allowWebSearchRound() {
		t.Fatal("second round refused")
	}
	// 6 remain of the 10-query budget.
	if got := b.takeQueries(8); got != 6 {
		t.Fatalf("takeQueries(8) = %d, want 6 (budget clamp)", got)
	}
	if b.allowWebSearchRound() {
		t.Error("round allowed with query budget exhausted")
	}
}

func TestRoundBudget_MaxUsesLowersQueryBudget(t *testing.T) {
	b := newRoundBudget(&reqconv.WebSearchOptions{MaxUses: 2})
	b.allowWebSearchRound()
	if got := b.takeQueries(5); got != 2 {
		t.Errorf("takeQueries(5) = %d, want 2 (max_uses)", got)
	}
}

func TestRoundBudget_ToolSearchIndependent(t *testing.T) {
	b := newRoundBudget(nil)
	for i := range maxToolSearchRounds {
		if !b.allowToolSearch() {
			t.Fatalf("tool search %d refused", i+1)
		}
	}
	if b.allowToolSearch() {
		t.Error("tool search allowed past its budget")
	}
	if !b.allowWebSearchRound() {
		t.Error("web search budget drained by tool search")
	}
}

func TestAppendWebSearchMessages_PreambleAndPairs(t *testing.T) {
	calls := []webSearchCall{
		{query: "q1", raw: `{"results":[]}`},
		{query: "q2", raw: "web_search failed: timeout", isError: true},
	}
	msgs := appendWebSearchMessages(nil, "Let me check the latest docs.", calls)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want assistant+user", len(msgs))
	}

	assistant := msgs[0]
	if assistant.Role != "assistant" || len(assistant.Content.Blocks) != 3 {
		t.Fatalf("assistant = %+v", assistant)
	}
	if b := assistant.Content.Blocks[0]; b.Type != anthropic.BlockTypeText || !strings.Contains(b.Text, "latest docs") {
		t.Errorf("preamble block = %+v", b)
	}
	for i, b := range assistant.Content.Blocks[1:] {
		if b.Type != anthropic.BlockTypeToolUse || b.Name != kiromcp.WebSearchToolName {
			t.Errorf("tool_use[%d] = %+v", i, b)
		}
	}

	user := msgs[1]
	if user.Role != "user" || len(user.Content.Blocks) != 2 {
		t.Fatalf("user = %+v", user)
	}
	for i, b := range user.Content.Blocks {
		if b.Type != anthropic.BlockTypeToolResult {
			t.Errorf("tool_result[%d] type = %q", i, b.Type)
		}
		if b.ToolUseID != assistant.Content.Blocks[i+1].ID {
			t.Errorf("tool_result[%d] id %q does not match tool_use %q", i, b.ToolUseID, assistant.Content.Blocks[i+1].ID)
		}
	}
	if !user.Content.Blocks[1].IsError {
		t.Error("failed search not marked is_error")
	}
}

func TestAppendWebSearchMessages_NoPreamble(t *testing.T) {
	msgs := appendWebSearchMessages(nil, "  \n", []webSearchCall{{query: "q", raw: "{}"}})
	if got := len(msgs[0].Content.Blocks); got != 1 {
		t.Errorf("assistant blocks = %d, want 1 (no preamble text block)", got)
	}
}

func TestWebSearchResultContent_SuccessAndError(t *testing.T) {
	success := webSearchCall{
		query: "q",
		results: []kiromcp.SearchResult{
			{Title: "T", URL: "https://go.dev", Snippet: "s", PublishedDate: "2026-08-01", Content: "page text"},
		},
	}
	blocks, ok := webSearchResultContent(success).([]map[string]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("content = %v", webSearchResultContent(success))
	}
	entry := blocks[0]
	if entry["type"] != anthropic.BlockTypeWebSearchResult || entry["url"] != "https://go.dev" || entry["page_age"] != "2026-08-01" {
		t.Errorf("entry = %v", entry)
	}
	decoded, ok := kiromcp.DecodeResultContent(entry["encrypted_content"].(string))
	if !ok || !strings.Contains(decoded, "page text") || !strings.Contains(decoded, "s") {
		t.Errorf("carrier = %q, %v", decoded, ok)
	}

	failure := webSearchCall{query: "q", isError: true, raw: "boom"}
	errContent, ok := webSearchResultContent(failure).(map[string]any)
	if !ok || errContent["type"] != anthropic.BlockTypeWebSearchResultError {
		t.Errorf("error content = %v", webSearchResultContent(failure))
	}
}

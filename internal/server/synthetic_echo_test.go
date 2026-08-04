package server

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/kiroproto"
)

// kirocc injects a synthetic placeholder into history to satisfy Kiro's role
// alternation. The model reads it as an example of an assistant turn and echoes
// it back, which reaches the user as a turn that says nothing. These tests pin
// the end-to-end behaviour: the echo is retried, and the client sees the retry's
// real answer rather than the placeholder.

func TestPostMessages_SyntheticEmptyEcho_RetriedStreaming(t *testing.T) {
	for _, echo := range []string{"(empty)", anthropic.SyntheticEmptyText} {
		t.Run(echo, func(t *testing.T) {
			var calls atomic.Int32
			client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
				text := echo
				if calls.Add(1) > 1 {
					text = "Here is the real answer."
				}
				p, _ := json.Marshal(map[string]string{"content": text})
				return &kiroclient.Response{
					StatusCode: 200,
					Body:       buildEventStream("assistantResponseEvent", p),
					Header:     http.Header{},
				}, nil
			}}

			srv := newTestServer(t, "", client)
			defer srv.Close()

			resp := postMessages(t, srv.URL, `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != 200 {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			body, _ := io.ReadAll(resp.Body)
			sse := string(body)

			if got := calls.Load(); got != 2 {
				t.Errorf("upstream calls = %d, want 2 (echo then retry)", got)
			}
			if !strings.Contains(sse, "Here is the real answer.") {
				t.Errorf("retry answer missing from stream:\n%s", sse)
			}
			// The discarded attempt must not reach the client at all.
			if strings.Contains(sse, `"text":"`+echo+`"`) {
				t.Errorf("placeholder echo leaked to the client:\n%s", sse)
			}
		})
	}
}

func TestPostMessages_SyntheticEmptyEcho_RetriedNonStreaming(t *testing.T) {
	var calls atomic.Int32
	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		text := "(empty)"
		if calls.Add(1) > 1 {
			text = "Real answer."
		}
		p, _ := json.Marshal(map[string]string{"content": text})
		return &kiroclient.Response{
			StatusCode: 200,
			Body:       buildEventStream("assistantResponseEvent", p),
			Header:     http.Header{},
		}, nil
	}}

	srv := newTestServer(t, "", client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var result map[string]any
	_ = json.UnmarshalRead(resp.Body, &result)

	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (echo then retry)", got)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content: %v", result)
	}
	block := content[0].(map[string]any)
	if block["text"] != "Real answer." {
		t.Errorf("text = %v, want the retry answer", block["text"])
	}
}

// A reply that merely mentions the placeholder is a real answer and must pass
// through untouched on the first attempt.
func TestPostMessages_PlaceholderMention_NotRetried(t *testing.T) {
	const answer = "(empty) is the placeholder the normalizer injects."
	var calls atomic.Int32
	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		calls.Add(1)
		p, _ := json.Marshal(map[string]string{"content": answer})
		return &kiroclient.Response{
			StatusCode: 200,
			Body:       buildEventStream("assistantResponseEvent", p),
			Header:     http.Header{},
		}, nil
	}}

	srv := newTestServer(t, "", client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (no retry for a real answer)", got)
	}
	if !strings.Contains(string(body), "is the placeholder the normalizer injects") {
		t.Errorf("real answer missing from stream:\n%s", body)
	}
}

// Both attempts echoing means there is no answer to deliver; the adapter reports
// an error rather than handing the client a turn that says nothing.
func TestPostMessages_SyntheticEmptyEcho_BothAttempts_Errors(t *testing.T) {
	var calls atomic.Int32
	client := &mockKiroClient{handler: func(ctx context.Context, token string, payload *kiroproto.Payload, region string) (*kiroclient.Response, error) {
		calls.Add(1)
		p, _ := json.Marshal(map[string]string{"content": "(empty)"})
		return &kiroclient.Response{
			StatusCode: 200,
			Body:       buildEventStream("assistantResponseEvent", p),
			Header:     http.Header{},
		}, nil
	}}

	srv := newTestServer(t, "", client)
	defer srv.Close()

	resp := postMessages(t, srv.URL, `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2", got)
	}
	if strings.Contains(string(body), `"text":"(empty)"`) {
		t.Errorf("placeholder echo leaked to the client:\n%s", body)
	}
	if resp.StatusCode == 200 && !strings.Contains(string(body), "error") {
		t.Errorf("expected an error after both attempts echoed; status=%d body=%s", resp.StatusCode, body)
	}
}

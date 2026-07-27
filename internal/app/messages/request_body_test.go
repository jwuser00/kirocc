package messages

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bodyOfSize builds a valid Anthropic request whose serialized size is at least
// n bytes, by padding the user message content.
func bodyOfSize(t *testing.T, n int) string {
	t.Helper()
	payload := map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 16,
		"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("x", n)},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestParseAndValidateRequest_BodySizeLimit(t *testing.T) {
	const limit = 4 << 10

	tests := []struct {
		name      string
		maxBody   int64
		padding   int
		wantError bool
	}{
		{"under the limit is accepted", limit, limit / 2, false},
		{"over the limit is rejected", limit, limit * 2, true},
		{"zero limit means unlimited", 0, limit * 4, false},
		{"negative limit means unlimited", -1, limit * 4, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := bodyOfSize(t, tt.padding)
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			w := httptest.NewRecorder()

			req, err := parseAndValidateRequest(context.Background(), w, r, tt.maxBody)
			if tt.wantError {
				if err == nil {
					t.Fatal("expected an error for a body over the limit")
				}
				if !strings.Contains(err.Error(), "too large") {
					t.Errorf("err = %v, want it to mention the body being too large", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.Model != "claude-sonnet-4-6" {
				t.Errorf("Model = %q, want the request to decode", req.Model)
			}
		})
	}
}

// The default has to clear what a real Claude Code turn sends: the whole
// transcript plus tool schemas and base64 images. A 4 MiB cap rejected live
// sessions, which surfaced to the user as an opaque 400.
func TestDefaultMaxBodySize_AcceptsLargeTranscript(t *testing.T) {
	const eightMiB = 8 << 20

	body := bodyOfSize(t, eightMiB)
	if int64(len(body)) <= 4<<20 {
		t.Fatalf("test body is %d bytes, expected it to exceed the old 4 MiB cap", len(body))
	}

	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()

	if _, err := parseAndValidateRequest(context.Background(), w, r, DefaultMaxBodySize); err != nil {
		t.Fatalf("default limit rejected a %s body: %v", fmt.Sprintf("%dMiB", len(body)>>20), err)
	}
}

func TestNew_DefaultsMaxBodySize(t *testing.T) {
	s := New(nil, nil)
	if s.maxBodySize != DefaultMaxBodySize {
		t.Errorf("maxBodySize = %d, want the default %d", s.maxBodySize, DefaultMaxBodySize)
	}

	s = New(nil, nil, WithMaxBodySize(123))
	if s.maxBodySize != 123 {
		t.Errorf("maxBodySize = %d, want the override 123", s.maxBodySize)
	}
}

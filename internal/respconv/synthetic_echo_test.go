package respconv

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiroproto"
)

// A response whose only visible text is the synthetic placeholder kirocc itself
// injected is indistinguishable from an empty one to the user, so it must be
// reported as an empty-visible end_turn and retried rather than delivered.
func TestSSEWriter_SyntheticEmptyEcho_IsEmptyVisible(t *testing.T) {
	for _, echo := range []string{anthropic.SyntheticEmptyText, "(empty)", "  (empty)\n"} {
		t.Run(echo, func(t *testing.T) {
			w := httptest.NewRecorder()
			sw := NewSSEWriter(context.Background(), w, "claude-opus-5", 200000, nil, 0, 0)
			var promoted bool
			sw.OnVisibleOutput = func() { promoted = true }

			sw.HandleEvent(kiroproto.Event{Type: "assistantResponseEvent", Content: echo})
			sw.Finish()

			if !sw.IsEmptyVisibleEndTurn() {
				t.Error("expected IsEmptyVisibleEndTurn() = true for a placeholder echo")
			}
			// Promotion must be withheld: once promoted the bytes are already on
			// the wire and the retry can no longer replace them.
			if promoted {
				t.Error("OnVisibleOutput fired; placeholder echo must not promote the gate")
			}
		})
	}
}

// A placeholder echo with real content attached is a genuine reply.
func TestSSEWriter_PlaceholderWithContent_IsNotEmptyVisible(t *testing.T) {
	w := httptest.NewRecorder()
	sw := NewSSEWriter(context.Background(), w, "claude-opus-5", 200000, nil, 0, 0)
	var promoted bool
	sw.OnVisibleOutput = func() { promoted = true }

	sw.HandleEvent(kiroproto.Event{
		Type:    "assistantResponseEvent",
		Content: "(empty) is what the normalizer injects between turns.",
	})
	sw.Finish()

	if sw.IsEmptyVisibleEndTurn() {
		t.Error("a reply that merely mentions the placeholder is not empty")
	}
	if !promoted {
		t.Error("expected the gate to be promoted for a real reply")
	}
	if !strings.Contains(w.Body.String(), "normalizer injects") {
		t.Errorf("real text missing from stream: %s", w.Body.String())
	}
}

// The decision to withhold has to be made on the first delta, before the full
// text is known. A stream that starts out looking like the placeholder must be
// held back, then released in full once it diverges.
func TestSSEWriter_PlaceholderPrefixThenDiverges_ReleasesFullText(t *testing.T) {
	w := httptest.NewRecorder()
	sw := NewSSEWriter(context.Background(), w, "claude-opus-5", 200000, nil, 0, 0)
	var promoted bool
	sw.OnVisibleOutput = func() { promoted = true }

	// Cumulative content, as Kiro sends it.
	sw.HandleEvent(kiroproto.Event{Type: "assistantResponseEvent", Content: "(emp"})
	if promoted {
		t.Fatal("gate promoted while the text could still complete the placeholder")
	}
	sw.HandleEvent(kiroproto.Event{Type: "assistantResponseEvent", Content: "(empty tables are fine)"})
	sw.Finish()

	if sw.IsEmptyVisibleEndTurn() {
		t.Error("diverged text is a real reply")
	}
	if !promoted {
		t.Error("expected promotion once the text diverged from the placeholder")
	}
	// Deltas are incremental, so the withheld prefix and the rest arrive as
	// separate events; both must reach the client for the text to be intact.
	body := w.Body.String()
	if !strings.Contains(body, `"text":"(emp"`) {
		t.Errorf("withheld prefix delta was lost, body: %s", body)
	}
	if !strings.Contains(body, "ty tables are fine)") {
		t.Errorf("remaining text delta was lost, body: %s", body)
	}
}

// Streaming and non-streaming must agree; Claude Code uses both.
func TestNonStreaming_SyntheticEmptyEcho_IsEmptyVisible(t *testing.T) {
	for _, echo := range []string{anthropic.SyntheticEmptyText, "(empty)"} {
		t.Run(echo, func(t *testing.T) {
			acc := NewNonStreamingAccumulator(200000, nil, 0, 0)
			acc.ProcessEvent(kiroproto.Event{Type: "assistantResponseEvent", Content: echo})
			if !acc.IsEmptyVisibleEndTurn() {
				t.Error("expected IsEmptyVisibleEndTurn() = true for a placeholder echo")
			}
		})
	}
}

func TestNonStreaming_PlaceholderWithContent_IsNotEmptyVisible(t *testing.T) {
	acc := NewNonStreamingAccumulator(200000, nil, 0, 0)
	acc.ProcessEvent(kiroproto.Event{
		Type:    "assistantResponseEvent",
		Content: "(empty) is the old placeholder value.",
	})
	if acc.IsEmptyVisibleEndTurn() {
		t.Error("a reply that merely mentions the placeholder is not empty")
	}
}

// A placeholder echo alongside a tool call is not an empty turn: the tool call
// is the visible output, and retrying would discard it.
func TestSSEWriter_PlaceholderEchoWithToolUse_IsNotEmptyVisible(t *testing.T) {
	w := httptest.NewRecorder()
	sw := NewSSEWriter(context.Background(), w, "claude-opus-5", 200000, nil, 0, 0)

	sw.HandleEvent(kiroproto.Event{Type: "assistantResponseEvent", Content: "(empty)"})
	sw.HandleEvent(kiroproto.Event{
		Type: "toolUseEvent", ToolStop: true,
		ToolUseID: "toolu_1", ToolName: "Bash", ToolInput: `{"command":"ls"}`,
	})
	sw.Finish()

	if sw.IsEmptyVisibleEndTurn() {
		t.Error("a turn carrying a tool call is not empty")
	}
}

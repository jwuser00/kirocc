package reqconv

import (
	"strings"
	"testing"

	"github.com/d-kuro/kirocc/internal/anthropic"
)

func testImageBlock(data string) anthropic.ContentBlock {
	return anthropic.ContentBlock{
		Type:   anthropic.BlockTypeImage,
		Source: &anthropic.ImageSource{Type: "base64", MediaType: "image/png", Data: data},
	}
}

// toolResultWithImage models what Claude Code sends after Read on an image file:
// the image block is nested inside the tool_result.
func toolResultWithImage(id, data string) anthropic.ContentBlock {
	return anthropic.ContentBlock{
		Type:      anthropic.BlockTypeToolResult,
		ToolUseID: id,
		Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			testImageBlock(data),
		}},
	}
}

func readToolUse(id, path string) anthropic.ContentBlock {
	return anthropic.ContentBlock{
		Type:  anthropic.BlockTypeToolUse,
		ID:    id,
		Name:  "Read",
		Input: map[string]any{"file_path": path},
	}
}

func buildWithImages(t *testing.T, msgs []anthropic.Message, maxHistoryImages int) []string {
	t.Helper()
	req := &anthropic.Request{
		Model:    "m",
		Tools:    []anthropic.Tool{{Name: "Read", InputSchema: map[string]any{"type": "object"}}},
		Messages: msgs,
	}
	p, _, err := BuildPayload(req, BuildOptions{
		ModelID:          "m",
		ConversationID:   "c",
		MaxHistoryImages: maxHistoryImages,
	})
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	got := make([]string, 0, len(p.ConversationState.CurrentMessage.UserInputMessage.Images))
	for _, im := range p.ConversationState.CurrentMessage.UserInputMessage.Images {
		got = append(got, im.Source.Bytes)
	}
	return got
}

func assertImages(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("images = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("images = %v, want %v", got, want)
		}
	}
}

// An image is only visible on the turn it arrives unless it is replayed: Kiro
// history entries have no images field. A follow-up question must still see it.
func TestHistoryImageReplay_SurvivesFollowUpTurn(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			{Type: anthropic.BlockTypeText, Text: "what is this?"},
			testImageBlock("PASTED"),
		}}},
		{Role: "assistant", Content: anthropic.MessageContent{Text: "A chart."}},
		{Role: "user", Content: anthropic.MessageContent{Text: "what color is the bar?"}},
	}
	assertImages(t, buildWithImages(t, msgs, DefaultMaxHistoryImages), []string{"PASTED"})
	// Replay off reproduces the old behaviour: the image is gone.
	assertImages(t, buildWithImages(t, msgs, 0), nil)
}

// Paste and path arrive on different turns (the paste is already in history by
// the time the Read result lands), so replay is what makes both visible at once.
func TestHistoryImageReplay_MixedPasteAndPath(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			{Type: anthropic.BlockTypeText, Text: "compare this with /b.png"},
			testImageBlock("PASTED"),
		}}},
		{Role: "assistant", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			readToolUse("t1", "/b.png"),
		}}},
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			toolResultWithImage("t1", "FROMPATH"),
		}}},
	}
	// History images come first, so the order is oldest to newest.
	assertImages(t, buildWithImages(t, msgs, DefaultMaxHistoryImages), []string{"PASTED", "FROMPATH"})
	assertImages(t, buildWithImages(t, msgs, 0), []string{"FROMPATH"})
}

// Two Reads in one turn land as two tool_results on the same message, so both
// are current-message images and need no replay.
func TestHistoryImageReplay_TwoPathsSameTurn(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Text: "compare /a.png and /b.png"}},
		{Role: "assistant", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			readToolUse("t1", "/a.png"),
			readToolUse("t2", "/b.png"),
		}}},
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			toolResultWithImage("t1", "IMG_A"),
			toolResultWithImage("t2", "IMG_B"),
		}}},
	}
	assertImages(t, buildWithImages(t, msgs, 0), []string{"IMG_A", "IMG_B"})
}

// Past the cap, the oldest images are dropped and the newest are kept.
func TestHistoryImageReplay_CapKeepsNewest(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			testImageBlock("OLD1"), testImageBlock("OLD2"), testImageBlock("OLD3"),
		}}},
		{Role: "assistant", Content: anthropic.MessageContent{Text: "ok"}},
		{Role: "user", Content: anthropic.MessageContent{Text: "and now?"}},
	}
	assertImages(t, buildWithImages(t, msgs, 2), []string{"OLD2", "OLD3"})
	assertImages(t, buildWithImages(t, msgs, -1), []string{"OLD1", "OLD2", "OLD3"})
}

// The cap bounds replayed history images only; the current turn's own images are
// always attached, so a capped session still sees what just arrived.
func TestHistoryImageReplay_CapDoesNotDropCurrentTurn(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			testImageBlock("OLD1"), testImageBlock("OLD2"),
		}}},
		{Role: "assistant", Content: anthropic.MessageContent{Text: "ok"}},
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			{Type: anthropic.BlockTypeText, Text: "and this one"},
			testImageBlock("NEW"),
		}}},
	}
	assertImages(t, buildWithImages(t, msgs, 1), []string{"OLD2", "NEW"})
}

func TestCollectHistoryImages_Disabled(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{testImageBlock("A")}}},
	}
	if got := collectHistoryImages(msgs, 0); got != nil {
		t.Fatalf("got %v, want nil when replay is disabled", got)
	}
}

// The backend rejects a single image over 5MB with an unattributed 502, and
// replay would repeat that on every later turn, so oversized images are dropped
// before they reach the wire.
func TestImageSizeLimit_OversizedNotCarried(t *testing.T) {
	oversized := testImageBlock(strings.Repeat("A", maxImageBytes+1))
	atLimit := testImageBlock(strings.Repeat("B", maxImageBytes))

	if _, ok := imageFromBlock(oversized); ok {
		t.Fatal("oversized image was carried, want dropped")
	}
	if _, ok := imageFromBlock(atLimit); !ok {
		t.Fatal("image exactly at the limit was dropped, want carried")
	}
}

// A dropped image must not be described as attached: the model cannot verify the
// claim, and "attached" would make it answer as if it could see the image.
func TestImageSizeLimit_PlaceholderTellsTheTruth(t *testing.T) {
	big := anthropic.ContentBlock{
		Type:      anthropic.BlockTypeToolResult,
		ToolUseID: "t1",
		Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			testImageBlock(strings.Repeat("A", maxImageBytes+1)),
		}},
	}
	toolResults, images := scanCurrentMessage(anthropic.MessageContent{
		Blocks: []anthropic.ContentBlock{big},
	})
	if len(images) != 0 {
		t.Fatalf("got %d images, want 0", len(images))
	}
	stdout, ok := toolResults[0].Content[0].JSON["stdout"].(string)
	if !ok {
		t.Fatalf("stdout is not a string: %v", toolResults[0].Content[0].JSON)
	}
	if stdout != imageTooLargePlaceholder {
		t.Fatalf("stdout = %q, want %q", stdout, imageTooLargePlaceholder)
	}
}

// One oversized image must not take the whole request down with it: the others
// still travel.
func TestImageSizeLimit_OthersStillCarried(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
			{Type: anthropic.BlockTypeText, Text: "three images"},
			testImageBlock("SMALL1"),
			testImageBlock(strings.Repeat("A", maxImageBytes+1)),
			testImageBlock("SMALL2"),
		}}},
	}
	assertImages(t, buildWithImages(t, msgs, DefaultMaxHistoryImages), []string{"SMALL1", "SMALL2"})
}

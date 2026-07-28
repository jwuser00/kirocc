package reqconv

import (
	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiroproto"
)

// scanCurrentMessage walks message content once and extracts tool_results and
// images. Replaces the former pattern of calling ExtractToolResults and
// ExtractImages separately, which scanned the block list twice.
//
// Images nested inside a tool_result (what Claude Code sends after reading an
// image file) are lifted into the message-level images array: Kiro tool results
// carry text or JSON only, so an inline image block there would be dropped.
func scanCurrentMessage(content anthropic.MessageContent) (toolResults []kiroproto.ToolResult, images []kiroproto.Image) {
	if content.IsString() {
		return nil, nil
	}
	for _, b := range content.Blocks {
		switch {
		case b.IsToolResult():
			for _, cb := range nestedImageBlocks(b) {
				if img, ok := imageFromBlock(cb); ok {
					images = append(images, img)
				}
			}
			toolResults = append(toolResults, toolResultFromBlock(b, imageAttachedPlaceholder))
		case b.Type == anthropic.BlockTypeImage:
			if img, ok := imageFromBlock(b); ok {
				images = append(images, img)
			}
		}
	}
	return toolResults, images
}

// toolResultFromBlock converts an Anthropic tool_result block to the Kiro wire
// form. imagePlaceholder stands in for nested image blocks, which have no
// representation in a Kiro tool result.
func toolResultFromBlock(b anthropic.ContentBlock, imagePlaceholder string) kiroproto.ToolResult {
	status := kiroproto.ToolResultStatusSuccess
	// v3 captures show kiro-cli uses exit_status/stdout/stderr format.
	exitStatus := "0"
	if b.IsError {
		status = kiroproto.ToolResultStatusError
		exitStatus = "1"
	}
	text := extractToolResultContentText(b, imagePlaceholder)
	if text == "" {
		text = "(empty result)"
	}
	return kiroproto.ToolResult{
		ToolUseID: b.ToolUseID,
		Status:    status,
		Content: []kiroproto.ToolResultContent{{JSON: map[string]any{
			"exit_status": exitStatus,
			"stdout":      text,
			"stderr":      "",
		}}},
	}
}

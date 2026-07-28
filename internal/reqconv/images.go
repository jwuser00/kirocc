package reqconv

import (
	"log/slog"
	"strings"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiroproto"
)

const (
	// imageAttachedPlaceholder stands in for an image that was lifted out of a
	// tool_result into the message-level images array. Kiro tool results carry
	// text/JSON only, so the bytes cannot stay inline.
	imageAttachedPlaceholder = "[image attached to this message]"

	// imageEarlierPlaceholder stands in for an image from an earlier turn. Kiro
	// history entries have no images field, so the bytes travel on the current
	// message via replay (or are dropped once the replay cap is hit).
	imageEarlierPlaceholder = "[image provided earlier in this conversation]"
)

// DefaultMaxHistoryImages is how many images from earlier turns are replayed on
// the current message by default.
//
// Kiro's history entries have no images field, so an image is only visible on
// the turn it arrives. Without replay, any follow-up question that needs to look
// at the image again ("what does that label say?") is answered from the previous
// reply alone — the model cannot tell that the image is gone, so it guesses.
// Replaying costs a re-upload of every carried image on every turn, hence a cap.
const DefaultMaxHistoryImages = 10

// ExtractImages extracts image blocks from message content and converts to Kiro
// format. Images nested inside tool_result blocks (what a Read of an image file
// returns) are collected too, since Kiro has no place for them inside a tool
// result. URL-based images are skipped with a warning log.
func ExtractImages(content anthropic.MessageContent) []kiroproto.Image {
	if content.IsString() {
		return nil
	}
	var images []kiroproto.Image
	for _, b := range content.Blocks {
		switch {
		case b.Type == anthropic.BlockTypeImage:
			if img, ok := imageFromBlock(b); ok {
				images = append(images, img)
			}
		case b.IsToolResult():
			for _, cb := range nestedImageBlocks(b) {
				if img, ok := imageFromBlock(cb); ok {
					images = append(images, img)
				}
			}
		}
	}
	return images
}

// imageFromBlock converts a single Anthropic image block to the Kiro wire form.
// Reports false for blocks Kiro cannot carry (missing or non-base64 source).
func imageFromBlock(b anthropic.ContentBlock) (kiroproto.Image, bool) {
	if b.Source == nil {
		return kiroproto.Image{}, false
	}
	if b.Source.Type != "base64" {
		slog.Warn("skipping non-base64 image source type", "type", b.Source.Type)
		return kiroproto.Image{}, false
	}
	format := b.Source.MediaType
	if idx := strings.LastIndex(format, "/"); idx >= 0 {
		format = format[idx+1:]
	}
	return kiroproto.Image{
		Format: format,
		Source: kiroproto.ImageSource{Bytes: b.Source.Data},
	}, true
}

// nestedImageBlocks returns the image blocks inside a tool_result block's
// content. Callers that textualize a tool_result re-emit these at the top level
// so the image bytes are not lost along with the block.
func nestedImageBlocks(b anthropic.ContentBlock) []anthropic.ContentBlock {
	if b.Content.IsString() {
		return nil
	}
	var out []anthropic.ContentBlock
	for _, cb := range b.Content.Blocks {
		if cb.Type == anthropic.BlockTypeImage && cb.Source != nil {
			out = append(out, cb)
		}
	}
	return out
}

// collectHistoryImages returns the images from earlier-turn messages, oldest
// first, keeping at most the newest max. A max of zero disables replay; a
// negative max keeps everything.
//
// Kiro drops images that fall out of the current message (history entries have
// no images field), so these are re-sent on the current message to keep them
// visible for the rest of the session.
func collectHistoryImages(msgs []anthropic.Message, max int) []kiroproto.Image {
	if max == 0 || len(msgs) == 0 {
		return nil
	}
	var images []kiroproto.Image
	for _, msg := range msgs {
		images = append(images, ExtractImages(msg.Content)...)
	}
	if max > 0 && len(images) > max {
		dropped := len(images) - max
		slog.Warn("history image replay cap reached; dropping oldest images",
			"cap", max, "dropped", dropped, "total", len(images))
		images = images[dropped:]
	}
	return images
}

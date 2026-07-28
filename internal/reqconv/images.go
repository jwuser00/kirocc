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

	// imageTooLargePlaceholder stands in for an image the backend would reject
	// on size. Saying "attached" here would be a lie the model cannot check.
	imageTooLargePlaceholder = "[image omitted: over the 5MB per-image size limit]"
)

// imageBlockPlaceholder returns the text that stands in for an image block whose
// bytes cannot appear inline. Oversized images are never carried, so they get
// their own wording rather than the caller's default.
func imageBlockPlaceholder(b anthropic.ContentBlock, dflt string) string {
	if b.Source != nil && len(b.Source.Data) > maxImageBytes {
		return imageTooLargePlaceholder
	}
	return dflt
}

// DefaultMaxHistoryImages is how many images from earlier turns are replayed on
// the current message by default.
//
// Kiro's history entries have no images field, so an image is only visible on
// the turn it arrives. Without replay, any follow-up question that needs to look
// at the image again ("what does that label say?") is answered from the previous
// reply alone — the model cannot tell that the image is gone, so it guesses.
// Replaying costs a re-upload of every carried image on every turn, hence a cap.
const DefaultMaxHistoryImages = 10

// maxImageBytes is the largest single image the Kiro backend accepts, measured
// on the base64 payload.
//
// Probed against the live backend: one 4.85 MiB image succeeds, one 5.24 MiB
// image fails, and four images totalling 12.40 MiB succeed. The limit is
// therefore per-image rather than per-request, and sits at 5 MB — the figure the
// Anthropic API documents. Rejection is a bare "upstream API error" 502 naming
// neither the image nor its size, and replay resends history images every turn,
// so without this check one oversized image would fail every later turn too.
const maxImageBytes = 5 * 1000 * 1000

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
// Reports false for blocks Kiro cannot carry (missing source, non-base64 source,
// or a payload over the backend's per-image size limit).
func imageFromBlock(b anthropic.ContentBlock) (kiroproto.Image, bool) {
	if b.Source == nil {
		return kiroproto.Image{}, false
	}
	if b.Source.Type != "base64" {
		slog.Warn("skipping non-base64 image source type", "type", b.Source.Type)
		return kiroproto.Image{}, false
	}
	if len(b.Source.Data) > maxImageBytes {
		// Sending it anyway costs a 502 that names no cause, and replay would
		// repeat that failure on every later turn in the session.
		slog.Warn("skipping image over the backend per-image size limit",
			"bytes", len(b.Source.Data), "limit", maxImageBytes, "media_type", b.Source.MediaType)
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

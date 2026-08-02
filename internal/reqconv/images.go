package reqconv

import (
	"fmt"
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

// DefaultHistoryImageTurns is how many earlier user turns still contribute
// replayed images to the current message.
//
// Kiro's history entries have no images field, so an image is only visible on
// the turn it arrives. Without replay, any follow-up question that needs to look
// at the image again ("what does that label say?") is answered from the previous
// reply alone — the model cannot tell that the image is gone, so it guesses.
//
// The window is counted in turns rather than images so that everything attached
// to one turn expires together: five images sent at once stay usable as a set for
// as long as a single image would, which is what makes a "compare these" request
// survive into the next question. historyImageNote keeps a replayed image from
// being read as part of the current question, so the window is purely about cost
// — and that cost is unavoidable: replay attaches to the current message, which
// changes every turn, so the bytes are never prompt-cached and every carried
// image is billed again on every turn.
//
// Two means the current turn plus the two user turns before it. Past the window
// the imageEarlierPlaceholder text remains in history, so the model still knows
// an image was there and can ask for it again rather than guessing. Set 0 for the
// upstream behaviour of dropping earlier-turn images outright.
const DefaultHistoryImageTurns = 2

// historyImageNote explains why images from earlier turns are attached to the
// current message.
//
// Replay is the only way to keep them visible, but it also strips their
// provenance: an image from ten turns ago arrives in exactly the same place as
// one the user just sent, so the model has no reason not to treat an unrelated
// screenshot as part of the current question. kiroproto.Image carries bytes and
// format and nothing else, so the note has to travel in the message text.
//
// historyImages are prepended to the current turn's own images, which is what
// makes "the first N" accurate.
func historyImageNote(n int) string {
	if n == 1 {
		return "[Note: the first image attached to this message is from an earlier turn " +
			"of this conversation, not newly sent with this message.]"
	}
	return fmt.Sprintf("[Note: the first %d images attached to this message are from "+
		"earlier turns of this conversation, not newly sent with this message.]", n)
}

// appendHistoryImageNote adds the replay note to a current-message content
// string. A tool-result-only turn has empty content, and the note becomes the
// whole of it: the provenance is worth more than matching kiro-cli's empty-string
// shape on those turns.
func appendHistoryImageNote(content string, n int) string {
	note := historyImageNote(n)
	if content == "" {
		return note
	}
	return content + "\n\n" + note
}

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

// isUserTurnStart reports whether msg is a turn the user actually typed, as
// opposed to a tool_result continuation.
//
// Tool results arrive as user-role messages too, so counting raw user messages
// would let a handful of Read calls consume the whole window before the user has
// said anything else. Anything attached during those continuations still belongs
// to the turn that started them.
func isUserTurnStart(msg anthropic.Message) bool {
	if msg.Role != "user" {
		return false
	}
	if msg.Content.IsString() {
		return true
	}
	for _, b := range msg.Content.Blocks {
		switch b.Type {
		case anthropic.BlockTypeToolResult, anthropic.BlockTypeToolSearchToolResult:
			continue
		default:
			return true
		}
	}
	return false
}

// historyImageWindowStart returns the index in msgs where the replay window
// begins: the start of the turns'th most recent user turn. A history with fewer
// user turns than that keeps everything.
func historyImageWindowStart(msgs []anthropic.Message, turns int) int {
	seen := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if !isUserTurnStart(msgs[i]) {
			continue
		}
		seen++
		if seen > turns {
			return i + 1
		}
	}
	return 0
}

// collectHistoryImages returns the images from earlier-turn messages, oldest
// first, keeping those from the most recent turns user turns. Zero disables
// replay; a negative value keeps every earlier-turn image.
//
// Kiro drops images that fall out of the current message (history entries have
// no images field), so these are re-sent on the current message to stay visible.
// The window is per turn rather than per image so that a set sent together
// expires together — five images attached at once remain usable as a set for the
// same span a lone image would.
func collectHistoryImages(msgs []anthropic.Message, turns int) []kiroproto.Image {
	if turns == 0 || len(msgs) == 0 {
		return nil
	}

	start := 0
	if turns > 0 {
		start = historyImageWindowStart(msgs, turns)
	}

	var images []kiroproto.Image
	for _, msg := range msgs[start:] {
		images = append(images, ExtractImages(msg.Content)...)
	}

	if start > 0 {
		var older int
		for _, msg := range msgs[:start] {
			older += len(ExtractImages(msg.Content))
		}
		if older > 0 {
			slog.Debug("history images outside the replay window; not resent",
				"turns", turns, "dropped", older, "replayed", len(images))
		}
	}
	return images
}

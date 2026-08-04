package anthropic

import "strings"

// SyntheticEmptyText is the placeholder kirocc injects for a message it has to
// invent: a leading user turn when history starts with the assistant, or a
// filler turn between two same-role messages that Kiro's strict alternation
// would otherwise reject.
//
// The wording matters more than it looks. The placeholder lands in history on
// nearly every multi-turn request, so whatever it says becomes an example of
// what an assistant turn from this conversation looks like. The previous value,
// "(empty)", read as a plausible short reply, and the model imitated it —
// answering a real question with the literal text "(empty)" and nothing else.
// Observed against claude-opus-5 on 2026-08-03/04.
//
// This value is a bracketed annotation instead: it reads as metadata about a
// turn rather than as a turn, which is the same convention the image
// placeholders in reqconv already use.
const SyntheticEmptyText = "[no content for this turn]"

// legacySyntheticEmptyText is the placeholder used before SyntheticEmptyText
// replaced it. Sessions that ran against the old value carry it in their
// history, and Claude Code replays that history verbatim, so the echo guard has
// to keep recognizing it or those conversations stay poisoned.
const legacySyntheticEmptyText = "(empty)"

// syntheticEmptyTexts are every placeholder an assistant turn may echo back.
var syntheticEmptyTexts = [...]string{SyntheticEmptyText, legacySyntheticEmptyText}

// IsSyntheticEmptyEcho reports whether text is nothing but a synthetic-empty
// placeholder echoed back by the model.
//
// A response like this is indistinguishable from an empty one to the user, so
// callers treat it the same way they treat a thinking-only response: discard and
// retry rather than deliver. Surrounding whitespace is ignored; a placeholder
// with real content attached is a genuine reply and is left alone.
func IsSyntheticEmptyEcho(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	for _, p := range syntheticEmptyTexts {
		if trimmed == p {
			return true
		}
	}
	return false
}

// MayBeSyntheticEmptyEcho reports whether text is a prefix of some synthetic
// placeholder, and so could still turn into a full echo as more deltas arrive.
//
// Streaming needs this: the decision to withhold output has to be made on the
// first delta, before the full text is known. A stream that starts with "[no" is
// held back until it either completes the placeholder (discard and retry) or
// diverges from it (release immediately). Text that already contains a complete
// placeholder plus more content is a real reply, so it diverges.
func MayBeSyntheticEmptyEcho(text string) bool {
	if text == "" {
		return false
	}
	if IsSyntheticEmptyEcho(text) {
		return true // already a complete echo, trailing whitespace and all.
	}
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if trimmed == "" {
		return true // whitespace so far reveals nothing either way.
	}
	for _, p := range syntheticEmptyTexts {
		if strings.HasPrefix(p, trimmed) {
			return true
		}
	}
	return false
}

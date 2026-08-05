package reqconv

import (
	"slices"
	"strings"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/toolsearch"
)

// Anthropic declares tools its own API executes server-side with a versioned
// `type` and no `input_schema` — the schema lives on Anthropic's side, so the
// client never sends one. Kiro has no such concept: it rejects the entire
// request when any tool arrives without a schema, which is why a Claude Code
// turn that reaches for WebSearch used to fail with "upstream API error"
// instead of merely losing the tool.
//
// Every server tool is therefore either translated into a Kiro-side equivalent
// or dropped before conversion.
var anthropicServerToolPrefixes = []string{
	"web_search_",
	"web_fetch_",
	"code_execution_",
	"bash_",
	"text_editor_",
	"computer_",
}

// IsAnthropicServerTool reports whether t is one of Anthropic's server-executed
// tool declarations.
func IsAnthropicServerTool(t anthropic.Tool) bool {
	if t.Type == "" {
		return false
	}
	for _, p := range anthropicServerToolPrefixes {
		if strings.HasPrefix(t.Type, p) {
			return true
		}
	}
	return false
}

// isWebSearchServerTool reports whether t is Anthropic's WebSearch declaration
// (web_search_20250305, web_search_20260209, ...).
func isWebSearchServerTool(t anthropic.Tool) bool {
	return t.Type != "" && strings.HasPrefix(t.Type, "web_search_")
}

// WebSearchOptions carries the parts of Anthropic's WebSearch declaration that
// kirocc honors itself: max_uses caps the per-request query budget, and the
// domain lists post-filter search results (at most one list is set — the
// Anthropic API rejects both together, so kirocc doesn't re-validate).
type WebSearchOptions struct {
	MaxUses        int
	AllowedDomains []string
	BlockedDomains []string
}

// WebSearchOptionsFrom returns the options of the client's WebSearch
// declaration, or nil when none was offered.
//
// Under tool search the declaration usually starts out deferred — Claude Code
// only promotes WebSearch once the model asks for it — so the deferred map has
// to be consulted too. Missing that would leave the orchestrator unarmed on the
// very requests where a search is about to happen.
func WebSearchOptionsFrom(tools []anthropic.Tool, tsCtx *toolsearch.Context) *WebSearchOptions {
	if i := slices.IndexFunc(tools, isWebSearchServerTool); i >= 0 {
		return optionsOf(tools[i])
	}
	if tsCtx == nil {
		return nil
	}
	if i := slices.IndexFunc(tsCtx.ActiveTools, isWebSearchServerTool); i >= 0 {
		return optionsOf(tsCtx.ActiveTools[i])
	}
	for _, t := range tsCtx.DeferredTools {
		if isWebSearchServerTool(t) {
			return optionsOf(t)
		}
	}
	return nil
}

func optionsOf(t anthropic.Tool) *WebSearchOptions {
	return &WebSearchOptions{
		MaxUses:        t.MaxUses,
		AllowedDomains: t.AllowedDomains,
		BlockedDomains: t.BlockedDomains,
	}
}

// RewriteServerTools strips Anthropic server-tool declarations from tools and
// reports whether a WebSearch declaration was among them.
//
// When webSearchEnabled is true a WebSearch declaration makes wantWebSearch
// true, and the caller is expected to append kiromcp.WebSearchToolEntry() so
// the model can request a search that kirocc executes on its behalf. When it
// is false the declaration is simply dropped: the capability disappears, but
// the request still succeeds.
//
// The returned slice aliases nothing from tools; callers may mutate it freely.
func RewriteServerTools(tools []anthropic.Tool, webSearchEnabled bool) (kept []anthropic.Tool, wantWebSearch bool) {
	// Fast path: no server tools at all, which is the common case for clients
	// that are not Claude Code.
	var found bool
	if slices.ContainsFunc(tools, IsAnthropicServerTool) {
		found = true
	}
	if !found {
		return tools, false
	}

	kept = make([]anthropic.Tool, 0, len(tools))
	for _, t := range tools {
		if !IsAnthropicServerTool(t) {
			kept = append(kept, t)
			continue
		}
		if webSearchEnabled && isWebSearchServerTool(t) {
			wantWebSearch = true
		}
	}
	return kept, wantWebSearch
}

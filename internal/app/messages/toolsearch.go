package messages

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/httpx"
	"github.com/d-kuro/kirocc/internal/kiromcp"
	"github.com/d-kuro/kirocc/internal/kiroproto"
	"github.com/d-kuro/kirocc/internal/logging"
	"github.com/d-kuro/kirocc/internal/reqconv"
	"github.com/d-kuro/kirocc/internal/respconv"
	"github.com/d-kuro/kirocc/internal/toolsearch"
	"github.com/google/uuid"
)

const maxToolSearchRounds = 3

// maxOrchestratorRounds bounds the shared round loop. ToolSearch and web_search
// keep independent budgets (a response may legitimately use both), so the loop
// has to allow for the worst case of each being exhausted in turn.
const maxOrchestratorRounds = maxToolSearchRounds + maxWebSearchRounds

// interception is a tool_use the orchestrator handles instead of forwarding.
type interception struct {
	name  string
	input string
}

// budget tracks per-tool round usage so one tool cannot starve the other.
type roundBudget struct {
	toolSearch int
	webSearch  int
}

// allow reports whether another round of the named tool is permitted, counting
// it when it is. Exhausting a budget stops interception for that tool only.
func (b *roundBudget) allow(name string) bool {
	if name == kiromcp.WebSearchToolName {
		if b.webSearch >= maxWebSearchRounds {
			return false
		}
		b.webSearch++
		return true
	}
	if b.toolSearch >= maxToolSearchRounds {
		return false
	}
	b.toolSearch++
	return true
}

// roundTotals accumulates per-round usage across tool-search rounds and folds
// in the current (partial) round when a final summary is needed.
type roundTotals struct {
	inputTokens  int
	outputTokens int
	credits      float64
	hasCredits   bool
}

// addCompleted accumulates a round whose accumulator is about to be reset.
func (t *roundTotals) addCompleted(in, out int, credits float64, hasCredits bool) {
	t.inputTokens += in
	t.outputTokens += out
	if hasCredits {
		t.credits += credits
		t.hasCredits = true
	}
}

// withCurrent returns the totals folded together with the current round's stats.
func (t roundTotals) withCurrent(in, out int, credits float64, hasCredits bool) (totalIn, totalOut int, totalCredits float64, totalHasCredits bool) {
	totalIn = t.inputTokens + in
	totalOut = t.outputTokens + out
	totalCredits = t.credits + credits
	totalHasCredits = t.hasCredits || hasCredits
	return
}

// creditsWith returns cumulative credits including the current round; the bool
// is true if any meteringEvent was observed across all rounds so far.
func (t roundTotals) creditsWith(credits float64, hasCredits bool) (float64, bool) {
	return t.credits + credits, t.hasCredits || hasCredits
}

// toolSearchOrchestrator manages the inner loop for tools kirocc resolves
// itself: ToolSearch (deferred tool loading) and the Kiro-hosted web_search.
// Either may be active independently — tsCtx is nil when the client does not
// use deferred tools, and webSearch is false when the feature is off — but
// both share one round loop because a single response can call both.
type toolSearchOrchestrator struct {
	service           *Service
	tsCtx             *toolsearch.Context
	webSearch         bool
	req               *anthropic.Request
	creds             *auth.Credentials
	buildOpts         reqconv.BuildOptions
	contextWindowSize int
	responseModel     string
}

// run dispatches to the streaming or non-streaming implementation based on
// req.Stream. Returns retryReasonEmptyVisibleEndTurn when the upstream produced
// nothing the user would see and the call site should retry.
func (o *toolSearchOrchestrator) run(ctx context.Context, w http.ResponseWriter) string {
	if o.req.Stream {
		return o.handleStreaming(ctx, w)
	}
	return o.handleNonStreaming(ctx, w)
}

func (o *toolSearchOrchestrator) handleStreaming(ctx context.Context, w http.ResponseWriter) string {
	_, short := logging.TraceIDs(ctx)

	gw := NewGateWriter(w)
	sw := respconv.NewSSEWriter(ctx, gw, o.responseModel, o.contextWindowSize, o.req.StopSequences, o.req.MaxTokens, 0)
	sw.OnVisibleOutput = func() { gw.Promote() }

	msgs := slices.Clone(o.req.Messages)

	var totals roundTotals
	var budget roundBudget

	for round := range maxOrchestratorRounds {
		payload, nameMap, err := o.buildPayload(msgs)
		if err != nil {
			slog.WarnContext(ctx, "tool search payload build error", "trace_id", short, "err", err)
			writeStreamingOrJSONError(gw, sw, w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
			return ""
		}
		sw.SetToolNameMap(nameMap.ReverseMap())

		apiResp, err := o.service.client.GenerateAssistantResponse(ctx, o.creds.AccessToken, payload, o.creds.Region)
		if err != nil {
			logUpstreamError(ctx, short, err, "round", round+1)
			writeStreamingOrJSONError(gw, sw, w, http.StatusBadGateway, errTypeAPI, "upstream API error")
			return ""
		}

		if round > 0 {
			// Accumulate usage from previous round before resetting.
			in, out := sw.Usage()
			credits, hasCredits := sw.Credits()
			totals.addCompleted(in, out, credits, hasCredits)
			sw.ResetAccumulator(o.contextWindowSize, o.req.StopSequences, o.req.MaxTokens, 0)
		}
		sw.SetDropToolNames(o.dropToolNames()...)

		var caught *interception
		var streamErr, localStop bool

		err = kiroproto.ParseStream(ctx, apiResp.Body, func(e kiroproto.Event) bool {
			if streamErr || localStop {
				return true
			}
			// After the intercepted frame is observed, keep parsing so subsequent
			// meteringEvent / contextUsageEvent frames flow into the accumulator.
			// Upstream errors in the tail must still abort the round.
			if caught != nil {
				if e.Type == kiroproto.EventException || e.Type == kiroproto.EventInvalidState {
					streamErr = true
					return true
				}
				sw.RecordTail(e)
				return false
			}
			if sw.WriteErr() {
				streamErr = true
				return true
			}
			if e.Type == kiroproto.EventToolUse && e.ToolStop && o.isIntercepted(e.ToolName) {
				caught = &interception{name: e.ToolName, input: e.ToolInput}
				return false
			}
			shouldStop := sw.HandleEvent(e)
			if sw.WriteErr() {
				streamErr = true
				return true
			}
			if !shouldStop {
				return false
			}
			if sw.LocalStop() {
				localStop = true
				return true
			}
			streamErr = true
			return true
		})
		_ = apiResp.Body.Close()

		// A parse error or upstream error frame in the tail must abort the
		// orchestrator even when the tool-search frame was already observed.
		if err != nil || streamErr {
			if err != nil {
				slog.ErrorContext(ctx, "stream error", "trace_id", short, "round", round+1, "err", err)
			}
			// If the stream is already promoted, sw.HandleEvent has already
			// emitted an SSE error event for invalidStateEvent/exception;
			// emitting another would produce duplicate/conflicting frames.
			if sw.Started() && gw.IsPromoted() {
				return ""
			}
			writeStreamingOrJSONError(gw, sw, w, http.StatusBadGateway, errTypeAPI, "upstream stream error")
			return ""
		}

		if caught == nil || !budget.allow(caught.name) {
			if caught != nil {
				slog.WarnContext(ctx, "orchestrator round budget exhausted",
					"trace_id", short, "tool", caught.name)
			}
			// streamErr was already handled above; only success/localStop reach here.
			if !localStop {
				sw.Finish()
			}
			// Detect a response the user would see as empty and signal retry.
			if !localStop && sw.IsEmptyVisibleEndTurn() && !gw.IsPromoted() {
				gw.Discard()
				slog.WarnContext(ctx, "empty visible end_turn detected in tool search",
					"trace_id", short, "cause", sw.EmptyVisibleCause())
				if credits, ok := totals.creditsWith(sw.Credits()); ok {
					logAbortedAttemptCredits(ctx, short, credits, retryReasonEmptyVisibleEndTurn)
				}
				return retryReasonEmptyVisibleEndTurn
			}
			in, out := sw.Usage()
			credits, hasCredits := sw.Credits()
			totalIn, totalOut, totalCredits, totalHasCredits := totals.withCurrent(in, out, credits, hasCredits)
			logResponseStats(ctx, short, totalIn, totalOut, sw.HasContextUsage(), sw.ContextUsagePercentage(), o.contextWindowSize, totalCredits, totalHasCredits)
			return ""
		}

		if caught.name == kiromcp.WebSearchToolName {
			// Web search is invisible to the client: no SSE blocks are emitted,
			// so Claude Code sees only the answer the results produced.
			query, parseErr := parseWebSearchInput(caught.input)
			if parseErr != nil {
				slog.WarnContext(ctx, "web search input parse error", "trace_id", short, "err", parseErr)
				writeStreamingOrJSONError(gw, sw, w, http.StatusBadRequest, errTypeInvalidRequest, parseErr.Error())
				return ""
			}
			resultText, isErr := o.executeWebSearch(ctx, short, round, query)
			msgs = appendWebSearchMessages(msgs, newWebSearchToolUseID(), query, resultText, isErr)
			continue
		}

		// ToolSearch detected — execute search and emit SSE blocks.
		query, maxResults, parseErr := parseToolSearchInput(caught.input)
		if parseErr != nil {
			slog.WarnContext(ctx, "tool search input parse error", "trace_id", short, "err", parseErr)
			writeStreamingOrJSONError(gw, sw, w, http.StatusBadRequest, errTypeInvalidRequest, parseErr.Error())
			return ""
		}
		srvToolUseID := "srvtoolu_" + uuid.New().String()[:24]
		searchInput := buildSearchInput(query, maxResults)

		inputBytes, _ := json.Marshal(searchInput)
		sw.WriteServerToolUse(srvToolUseID, o.tsCtx.SearchToolName, string(inputBytes))

		results, searchErr := o.executeSearch(ctx, short, round, query, maxResults)
		if searchErr != nil {
			sw.WriteToolSearchError(srvToolUseID, toolsearch.ErrorCode(searchErr))
		} else {
			sw.WriteToolSearchResult(srvToolUseID, results)
		}

		msgs = o.appendSearchMessages(msgs, srvToolUseID, searchInput, results, searchErr, nameMap)
	}

	// Max rounds reached without normal completion.
	slog.WarnContext(ctx, "tool search max rounds reached", "trace_id", short, "max_rounds", maxToolSearchRounds)
	sw.Finish()
	in, out := sw.Usage()
	credits, hasCredits := sw.Credits()
	totalIn, totalOut, totalCredits, totalHasCredits := totals.withCurrent(in, out, credits, hasCredits)
	logResponseStats(ctx, short, totalIn, totalOut, sw.HasContextUsage(), sw.ContextUsagePercentage(), o.contextWindowSize, totalCredits, totalHasCredits)
	return ""
}

func (o *toolSearchOrchestrator) handleNonStreaming(ctx context.Context, w http.ResponseWriter) string {
	_, short := logging.TraceIDs(ctx)

	msgs := slices.Clone(o.req.Messages)

	var orderedBlocks []any
	var totals roundTotals
	var lastStopReason string
	var lastStopSequence any

	var normalExit bool
	var budget roundBudget

	for round := range maxOrchestratorRounds {
		payload, nameMap, err := o.buildPayload(msgs)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
			return ""
		}

		apiResp, err := o.service.client.GenerateAssistantResponse(ctx, o.creds.AccessToken, payload, o.creds.Region)
		if err != nil {
			logUpstreamError(ctx, short, err, "round", round+1)
			httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream API error")
			return ""
		}

		acc := respconv.NewNonStreamingAccumulator(o.contextWindowSize, o.req.StopSequences, o.req.MaxTokens, 0)
		acc.SetDropToolNames(o.dropToolNames()...)
		acc.SetToolNameMap(nameMap.ReverseMap())

		var hasError bool
		var caught *interception
		err = kiroproto.ParseStream(ctx, apiResp.Body, func(e kiroproto.Event) bool {
			d := acc.ProcessEvent(e)
			if d.IsError {
				hasError = true
				return true
			}
			// Detect filtered tool_use (ToolSearch, web_search) via EventDelta.
			if d.ToolStop && o.isIntercepted(d.ToolName) {
				caught = &interception{name: d.ToolName, input: d.ToolInput}
			}
			return false
		})
		_ = apiResp.Body.Close()

		if (err != nil || hasError) && caught == nil {
			httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream error")
			return ""
		}

		resp, stats := acc.BuildResponse(o.responseModel)
		totals.addCompleted(stats.InputTokens, stats.OutputTokens, stats.Credits, stats.HasCredits)
		lastStopReason, _ = resp["stop_reason"].(string)
		lastStopSequence = resp["stop_sequence"]

		// Extract content blocks (ToolSearch won't appear here since it's filtered).
		content, _ := resp["content"].([]any)
		orderedBlocks = append(orderedBlocks, content...)

		if caught == nil || !budget.allow(caught.name) {
			if caught != nil {
				slog.WarnContext(ctx, "orchestrator round budget exhausted",
					"trace_id", short, "tool", caught.name)
			}
			// Detect a response the user would see as empty and signal retry.
			if acc.IsEmptyVisibleEndTurn() {
				slog.WarnContext(ctx, "empty visible end_turn detected in tool search",
					"trace_id", short, "cause", acc.EmptyVisibleCause())
				if totals.hasCredits {
					logAbortedAttemptCredits(ctx, short, totals.credits, retryReasonEmptyVisibleEndTurn)
				}
				return retryReasonEmptyVisibleEndTurn
			}
			normalExit = true
			break
		}

		if caught.name == kiromcp.WebSearchToolName {
			// No blocks are added to orderedBlocks: the search stays invisible
			// to the client, exactly as in the streaming path.
			query, parseErr := parseWebSearchInput(caught.input)
			if parseErr != nil {
				slog.WarnContext(ctx, "web search input parse error", "trace_id", short, "err", parseErr)
				httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, parseErr.Error())
				return ""
			}
			resultText, isErr := o.executeWebSearch(ctx, short, round, query)
			msgs = appendWebSearchMessages(msgs, newWebSearchToolUseID(), query, resultText, isErr)
			continue
		}

		// Execute search.
		query, maxResults, parseErr := parseToolSearchInput(caught.input)
		if parseErr != nil {
			slog.WarnContext(ctx, "tool search input parse error", "trace_id", short, "err", parseErr)
			httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, parseErr.Error())
			return ""
		}

		srvToolUseID := "srvtoolu_" + uuid.New().String()[:24]
		results, searchErr := o.executeSearch(ctx, short, round, query, maxResults)

		// Add server_tool_use block.
		searchInput := buildSearchInput(query, maxResults)
		orderedBlocks = append(orderedBlocks, map[string]any{
			"type":  anthropic.BlockTypeServerToolUse,
			"id":    srvToolUseID,
			"name":  o.tsCtx.SearchToolName,
			"input": searchInput,
		})

		// Add tool_search_tool_result block.
		if searchErr != nil {
			orderedBlocks = append(orderedBlocks, map[string]any{
				"type":        anthropic.BlockTypeToolSearchToolResult,
				"tool_use_id": srvToolUseID,
				"content": map[string]any{
					"type":       anthropic.BlockTypeToolSearchResultError,
					"error_code": toolsearch.ErrorCode(searchErr),
				},
			})
		} else {
			orderedBlocks = append(orderedBlocks, map[string]any{
				"type":        anthropic.BlockTypeToolSearchToolResult,
				"tool_use_id": srvToolUseID,
				"content": map[string]any{
					"type":            anthropic.BlockTypeToolSearchSearchResult,
					"tool_references": toolsearch.ToolRefMaps(results),
				},
			})
		}

		msgs = o.appendSearchMessages(msgs, srvToolUseID, searchInput, results, searchErr, nameMap)
	}

	// Max rounds reached without normal completion.
	if !normalExit {
		slog.WarnContext(ctx, "tool search max rounds reached", "trace_id", short, "max_rounds", maxToolSearchRounds)
	}

	// Build final response.
	finalResp := map[string]any{
		"id":            "msg_" + uuid.New().String()[:24],
		"type":          "message",
		"role":          "assistant",
		"content":       orderedBlocks,
		"model":         o.responseModel,
		"stop_reason":   lastStopReason,
		"stop_sequence": lastStopSequence,
		"usage": map[string]any{
			"input_tokens":                totals.inputTokens,
			"output_tokens":               totals.outputTokens,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 0,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, finalResp); err != nil {
		slog.ErrorContext(ctx, "write non-streaming response failed", "err", err)
	}
	_, _ = w.Write([]byte("\n"))

	logResponseStats(ctx, short, totals.inputTokens, totals.outputTokens, false, 0, o.contextWindowSize, totals.credits, totals.hasCredits)
	return ""
}

// executeSearch runs the tool search, promotes results, and logs.
func (o *toolSearchOrchestrator) executeSearch(ctx context.Context, short string, round int, query string, maxResults int) ([]string, error) {
	results, err := toolsearch.Search(query, o.tsCtx.DeferredTools, o.tsCtx.SearchType, maxResults)
	if err == nil {
		o.tsCtx.PromoteTools(results)
	}
	slog.InfoContext(ctx, "tool search executed",
		"trace_id", short, "round", round+1, "query", query, "results", results,
	)
	return results, err
}

// appendSearchMessages appends the server_tool_use + tool_result messages to the conversation.
// On error, the tool_result contains the error message instead of tool references.
// Tool names in the result text are shortened via nameMap so Kiro sees consistent names.
func (o *toolSearchOrchestrator) appendSearchMessages(msgs []anthropic.Message, srvToolUseID string, searchInput map[string]any, results []string, searchErr error, nameMap *reqconv.ToolNameMap) []anthropic.Message {
	var resultContent anthropic.MessageContent
	var isError bool
	if searchErr != nil {
		isError = true
		resultContent = anthropic.MessageContent{Text: "tool search error: " + toolsearch.ErrorCode(searchErr)}
	} else {
		// Shorten tool names in the result text so Kiro sees names matching the tool schema.
		shortened := make([]string, len(results))
		for i, name := range results {
			shortened[i] = nameMap.Shorten(name)
		}
		resultContent = anthropic.MessageContent{Text: "Found tools: " + strings.Join(shortened, ", ")}
	}
	return append(msgs,
		anthropic.Message{
			Role: "assistant",
			Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
				{Type: anthropic.BlockTypeServerToolUse, ID: srvToolUseID, Name: toolsearch.KiroToolSearchName, Input: searchInput},
			}},
		},
		anthropic.Message{
			Role: "user",
			Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
				{Type: anthropic.BlockTypeToolResult, ToolUseID: srvToolUseID, Content: resultContent, IsError: isError},
			}},
		},
	)
}

// buildSearchInput constructs the input map for a ToolSearch tool_use.
func buildSearchInput(query string, maxResults int) map[string]any {
	input := map[string]any{"query": query}
	if maxResults > 0 {
		input["max_results"] = maxResults
	}
	return input
}

func (o *toolSearchOrchestrator) buildPayload(msgs []anthropic.Message) (*kiroproto.Payload, *reqconv.ToolNameMap, error) {
	tmpReq := *o.req
	tmpReq.Messages = msgs
	return reqconv.BuildPayload(&tmpReq, o.buildOpts)
}

// writeStreamingOrJSONError writes an error via SSE if the stream is promoted, otherwise via JSON.
func writeStreamingOrJSONError(gw *GateWriter, sw *respconv.SSEWriter, w http.ResponseWriter, status int, errType, message string) {
	if sw.Started() && gw.IsPromoted() {
		sw.WriteError(errType, message)
		return
	}
	if !gw.IsPromoted() {
		gw.Discard()
	}
	httpx.WriteError(w, status, errType, message)
}

// parseToolSearchInput extracts query and max_results from the ToolSearch tool input JSON.
// Returns an error if the input is not valid JSON.
func parseToolSearchInput(input string) (query string, maxResults int, err error) {
	var parsed struct {
		Query      string  `json:"query"`
		MaxResults float64 `json:"max_results"`
	}
	if uerr := json.Unmarshal([]byte(input), &parsed); uerr != nil {
		return "", 0, fmt.Errorf("parse tool_search input: %w", uerr)
	}
	return parsed.Query, int(parsed.MaxResults), nil
}

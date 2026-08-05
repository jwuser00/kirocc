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
	"github.com/d-kuro/kirocc/internal/models"
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

// roundBudget tracks per-tool usage so one tool cannot starve the other.
// Web search is double-budgeted: rounds (extra Kiro round-trips) and queries
// (MCP searches), because one round may fan out several parallel queries.
type roundBudget struct {
	toolSearch       int
	webSearchRounds  int
	webSearchQueries int
	maxQueries       int
}

// newRoundBudget derives the budget, honoring the client's max_uses.
func newRoundBudget(wsOpts *reqconv.WebSearchOptions) roundBudget {
	b := roundBudget{maxQueries: maxWebSearchQueries}
	if wsOpts != nil && wsOpts.MaxUses > 0 && wsOpts.MaxUses < b.maxQueries {
		b.maxQueries = wsOpts.MaxUses
	}
	return b
}

// allowToolSearch reports whether another tool-search execution is permitted,
// counting it when it is.
func (b *roundBudget) allowToolSearch() bool {
	if b.toolSearch >= maxToolSearchRounds {
		return false
	}
	b.toolSearch++
	return true
}

// allowWebSearchRound reports whether another web-search round is permitted,
// counting it when it is.
func (b *roundBudget) allowWebSearchRound() bool {
	if b.webSearchRounds >= maxWebSearchRounds || b.webSearchQueries >= b.maxQueries {
		return false
	}
	b.webSearchRounds++
	return true
}

// takeQueries reserves up to n queries from the per-request budget and returns
// how many were granted.
func (b *roundBudget) takeQueries(n int) int {
	if remaining := b.maxQueries - b.webSearchQueries; n > remaining {
		n = remaining
	}
	b.webSearchQueries += n
	return n
}

// roundPlan is the work one round's interceptions produce after budgeting.
type roundPlan struct {
	webQueries   []string
	toolSearches []interception
}

// planRound turns the round's intercepted tool_uses into budgeted work. A nil
// plan with nil error means nothing is allowed to run and the round's output
// is final. A parse error aborts the request: the model produced an
// undecodable tool_use and re-prompting with it would loop.
func (o *toolSearchOrchestrator) planRound(caught []interception, budget *roundBudget) (*roundPlan, error) {
	var plan roundPlan
	var webInputs []string
	for _, c := range caught {
		if c.name == kiromcp.WebSearchToolName {
			webInputs = append(webInputs, c.input)
		} else if budget.allowToolSearch() {
			plan.toolSearches = append(plan.toolSearches, c)
		}
	}
	if len(webInputs) > 0 && budget.allowWebSearchRound() {
		seen := make(map[string]struct{})
		var queries []string
		for _, input := range webInputs {
			qs, err := parseWebSearchQueries(input)
			if err != nil {
				return nil, err
			}
			for _, q := range qs {
				if _, dup := seen[q]; dup {
					continue
				}
				seen[q] = struct{}{}
				queries = append(queries, q)
			}
		}
		plan.webQueries = queries[:budget.takeQueries(len(queries))]
	}
	if len(plan.webQueries) == 0 && len(plan.toolSearches) == 0 {
		return nil, nil
	}
	return &plan, nil
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
	service *Service
	tsCtx   *toolsearch.Context
	// wsOpts is the client's WebSearch declaration; nil when web search is off
	// (feature disabled or no declaration in the request).
	wsOpts            *reqconv.WebSearchOptions
	req               *anthropic.Request
	creds             *auth.Credentials
	buildOpts         reqconv.BuildOptions
	contextWindowSize int
	responseModel     string
}

// run dispatches to the streaming or non-streaming implementation based on
// req.Stream. Returns retryReasonEmptyVisibleEndTurn when the upstream produced
// nothing the user would see and the call site should retry.
func (o *toolSearchOrchestrator) run(ctx context.Context, w http.ResponseWriter, session *streamSession) string {
	if o.req.Stream {
		return o.handleStreaming(ctx, session)
	}
	return o.handleNonStreaming(ctx, w)
}

func (o *toolSearchOrchestrator) handleStreaming(ctx context.Context, session *streamSession) string {
	_, short := logging.TraceIDs(ctx)

	sw := respconv.NewSSEWriter(ctx, session, o.responseModel, o.contextWindowSize, o.req.StopSequences, o.req.MaxTokens, 0)
	sw.OnVisibleOutput = session.Promote
	sw.SetDrainOnStop(models.IsReasoningModel(o.buildOpts.ModelID))

	msgs := slices.Clone(o.req.Messages)

	var totals roundTotals
	budget := newRoundBudget(o.wsOpts)

	for round := range maxOrchestratorRounds {
		payload, nameMap, err := o.buildPayload(msgs)
		if err != nil {
			slog.WarnContext(ctx, "tool search payload build error", "trace_id", short, "err", err)
			final := newStreamFinalError(http.StatusBadRequest, errTypeInvalidRequest, err.Error())
			_ = session.WriteFinalError(final, func() error {
				return sw.WriteError(errTypeInvalidRequest, err.Error())
			})
			return ""
		}
		sw.SetToolNameMap(nameMap.ReverseMap())

		apiResp, err := o.service.client.GenerateAssistantResponse(ctx, o.creds.AccessToken, payload, o.creds.Region)
		if err != nil {
			logUpstreamError(ctx, short, err, "round", round+1)
			final := newStreamFinalError(http.StatusBadGateway, errTypeAPI, "upstream API error")
			_ = session.WriteFinalError(final, func() error {
				return sw.WriteError(errTypeAPI, "upstream API error")
			})
			return ""
		}
		session.Start()

		if round > 0 {
			// Accumulate usage from previous round before resetting.
			in, out := sw.Usage()
			credits, hasCredits := sw.Credits()
			totals.addCompleted(in, out, credits, hasCredits)
			sw.ResetAccumulator(o.contextWindowSize, o.req.StopSequences, o.req.MaxTokens, 0)
		}
		sw.SetDropToolNames(o.dropToolNames()...)

		var caught []interception
		var streamErr, localStop bool
		var invalidReason string
		var isException bool
		var upstreamMessage string

		err = kiroproto.ParseStream(ctx, apiResp.Body, func(e kiroproto.Event) bool {
			if streamErr || localStop {
				return true
			}
			// After the first intercepted frame, keep parsing: further
			// intercepted tool_uses in the same response are collected (one
			// answer may fan out several searches), and meteringEvent /
			// contextUsageEvent frames still flow into the accumulator.
			// Upstream errors in the tail must still abort the round.
			if len(caught) > 0 {
				if e.Type == kiroproto.EventException || e.Type == kiroproto.EventInvalidState {
					isException = e.Type == kiroproto.EventException
					invalidReason = e.InvalidStateReason
					upstreamMessage = e.ErrorText()
					streamErr = true
					return true
				}
				if e.Type == kiroproto.EventToolUse && e.ToolStop && o.isIntercepted(e.ToolName) {
					caught = append(caught, interception{name: e.ToolName, input: e.ToolInput})
					return false
				}
				sw.RecordTail(e)
				return false
			}
			if sw.WriteErr() != nil || session.Err() != nil {
				streamErr = true
				return true
			}
			if e.Type == kiroproto.EventToolUse && e.ToolStop && o.isIntercepted(e.ToolName) {
				caught = append(caught, interception{name: e.ToolName, input: e.ToolInput})
				return false
			}
			if e.Type == kiroproto.EventException || e.Type == kiroproto.EventInvalidState {
				isException = e.Type == kiroproto.EventException
				invalidReason = e.InvalidStateReason
				upstreamMessage = e.ErrorText()
			}
			shouldStop := sw.HandleEvent(e)
			if sw.WriteErr() != nil || session.Err() != nil {
				streamErr = true
				return true
			}
			if !shouldStop {
				return false
			}
			// Upstream error frames terminate as errors even when a local
			// stop is already latched (e.g. an exception arriving mid-drain
			// after a GPT tool-use max_tokens stop).
			if e.Type == kiroproto.EventException || e.Type == kiroproto.EventInvalidState {
				streamErr = true
				return true
			}
			if sw.LocalStop() {
				localStop = true
				return true
			}
			streamErr = true
			return true
		})
		_ = apiResp.Body.Close()

		if sw.WriteErr() != nil || session.Err() != nil || ctx.Err() != nil {
			return ""
		}

		// A parse error or upstream error frame in the tail must abort the
		// orchestrator even when the tool-search frame was already observed.
		if streamErr {
			classification := classifyUpstreamError(isException, invalidReason, upstreamMessage)
			_ = session.WriteFinalError(classification.final, func() error {
				return sw.WriteError(classification.final.sseType, classification.final.sseMessage)
			})
			return ""
		}
		if err != nil {
			slog.ErrorContext(ctx, "stream error", "trace_id", short, "round", round+1, "err", err)
			final := newStreamFinalError(http.StatusBadGateway, errTypeStreamError, "upstream stream error")
			_ = session.WriteFinalError(final, func() error {
				return sw.WriteError(errTypeStreamError, "upstream stream error")
			})
			return ""
		}

		plan, planErr := o.planRound(caught, &budget)
		if planErr != nil {
			slog.WarnContext(ctx, "web search input parse error", "trace_id", short, "err", planErr)
			final := newStreamFinalError(http.StatusBadRequest, errTypeInvalidRequest, planErr.Error())
			_ = session.WriteFinalError(final, func() error {
				return sw.WriteError(errTypeInvalidRequest, planErr.Error())
			})
			return ""
		}
		if plan == nil {
			if len(caught) > 0 {
				slog.WarnContext(ctx, "orchestrator round budget exhausted",
					"trace_id", short, "tool", caught[0].name)
			}
			// streamErr was already handled above; only success/localStop reach here.
			if !localStop {
				if err := sw.Finish(); err != nil {
					return ""
				}
			}
			// Detect a response the user would see as empty and signal retry.
			if !localStop && sw.IsEmptyVisibleEndTurn() && !session.IsPromoted() {
				session.Discard()
				slog.WarnContext(ctx, "empty visible end_turn detected in tool search",
					"trace_id", short, "cause", sw.EmptyVisibleCause())
				if credits, ok := totals.creditsWith(sw.Credits()); ok {
					logAbortedAttemptCredits(ctx, short, credits, retryReasonEmptyVisibleEndTurn)
				}
				return retryReasonEmptyVisibleEndTurn
			}
			if !session.IsPromoted() {
				if err := session.Promote(); err != nil {
					return ""
				}
			}
			in, out := sw.Usage()
			credits, hasCredits := sw.Credits()
			totalIn, totalOut, totalCredits, totalHasCredits := totals.withCurrent(in, out, credits, hasCredits)
			logResponseStats(ctx, short, totalIn, totalOut, sw.HasContextUsage(), sw.ContextUsagePercentage(), o.contextWindowSize, totalCredits, totalHasCredits)
			return ""
		}

		if len(plan.webQueries) > 0 {
			// The preamble must be read before the searches run: it is this
			// round's already-streamed text, carried into the history so the
			// next round does not repeat it.
			preamble := sw.Text()
			// Emit the server_tool_use blocks before running the searches so
			// the client shows them while the search+fetch is in flight.
			srvIDs := make([]string, len(plan.webQueries))
			for i, q := range plan.webQueries {
				srvIDs[i] = "srvtoolu_" + uuid.New().String()[:24]
				if o.service.webSearchVisible {
					inputBytes, _ := json.Marshal(map[string]any{"query": q})
					sw.WriteServerToolUse(srvIDs[i], kiromcp.WebSearchToolName, string(inputBytes))
				}
			}
			calls := o.executeWebSearches(ctx, short, round, plan.webQueries)
			for i := range calls {
				calls[i].srvID = srvIDs[i]
				if o.service.webSearchVisible {
					sw.WriteWebSearchResult(calls[i].srvID, webSearchResultContent(calls[i]))
				}
			}
			msgs = appendWebSearchMessages(msgs, preamble, calls)
		}
		if sw.WriteErr() != nil || session.Err() != nil {
			return ""
		}

		// The round's redacted reasoning blobs (GPT 5.6) belong to the round, not
		// to each search, so only the first replayed assistant turn carries them.
		redacted := sw.RedactedContents()
		for _, ts := range plan.toolSearches {
			// ToolSearch detected — execute search and emit SSE blocks.
			query, maxResults, parseErr := parseToolSearchInput(ts.input)
			if parseErr != nil {
				slog.WarnContext(ctx, "tool search input parse error", "trace_id", short, "err", parseErr)
				final := newStreamFinalError(http.StatusBadRequest, errTypeInvalidRequest, parseErr.Error())
				_ = session.WriteFinalError(final, func() error {
					return sw.WriteError(errTypeInvalidRequest, parseErr.Error())
				})
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
			if sw.WriteErr() != nil || session.Err() != nil {
				return ""
			}

			msgs = o.appendSearchMessages(msgs, srvToolUseID, searchInput, results, searchErr, nameMap, redacted)
			redacted = nil
		}
	}

	// Max rounds reached without normal completion.
	slog.WarnContext(ctx, "tool search max rounds reached", "trace_id", short, "max_rounds", maxToolSearchRounds)
	if err := sw.Finish(); err != nil {
		return ""
	}
	if !session.IsPromoted() {
		if err := session.Promote(); err != nil {
			return ""
		}
	}
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
	budget := newRoundBudget(o.wsOpts)

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
		var caught []interception
		err = kiroproto.ParseStream(ctx, apiResp.Body, func(e kiroproto.Event) bool {
			d := acc.ProcessEvent(e)
			if d.IsError {
				hasError = true
				return true
			}
			// Collect filtered tool_uses (ToolSearch, web_search) via EventDelta;
			// one response may fan out several searches.
			if d.ToolStop && o.isIntercepted(d.ToolName) {
				caught = append(caught, interception{name: d.ToolName, input: d.ToolInput})
			}
			return false
		})
		_ = apiResp.Body.Close()

		if (err != nil || hasError) && len(caught) == 0 {
			httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream error")
			return ""
		}

		resp, stats := acc.BuildResponse(o.responseModel)
		totals.addCompleted(stats.InputTokens, stats.OutputTokens, stats.Credits, stats.HasCredits)
		lastStopReason, _ = resp["stop_reason"].(string)
		lastStopSequence = resp["stop_sequence"]

		// Extract content blocks (intercepted tools won't appear here since
		// they're filtered).
		content, _ := resp["content"].([]any)
		orderedBlocks = append(orderedBlocks, content...)

		plan, planErr := o.planRound(caught, &budget)
		if planErr != nil {
			slog.WarnContext(ctx, "web search input parse error", "trace_id", short, "err", planErr)
			httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, planErr.Error())
			return ""
		}
		if plan == nil {
			if len(caught) > 0 {
				slog.WarnContext(ctx, "orchestrator round budget exhausted",
					"trace_id", short, "tool", caught[0].name)
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

		if len(plan.webQueries) > 0 {
			calls := o.executeWebSearches(ctx, short, round, plan.webQueries)
			if o.service.webSearchVisible {
				for i := range calls {
					calls[i].srvID = "srvtoolu_" + uuid.New().String()[:24]
					call := calls[i]
					orderedBlocks = append(orderedBlocks,
						map[string]any{
							"type":  anthropic.BlockTypeServerToolUse,
							"id":    call.srvID,
							"name":  kiromcp.WebSearchToolName,
							"input": map[string]any{"query": call.query},
						},
						map[string]any{
							"type":        anthropic.BlockTypeWebSearchToolResult,
							"tool_use_id": call.srvID,
							"content":     webSearchResultContent(call),
						},
					)
				}
			}
			msgs = appendWebSearchMessages(msgs, textOfContentBlocks(content), calls)
		}

		// The round's redacted reasoning blobs (GPT 5.6) belong to the round,
		// not to each search, so only the first replayed assistant turn
		// carries them.
		redacted := acc.RedactedContents()
		for _, ts := range plan.toolSearches {
			// Execute search.
			query, maxResults, parseErr := parseToolSearchInput(ts.input)
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

			msgs = o.appendSearchMessages(msgs, srvToolUseID, searchInput, results, searchErr, nameMap, redacted)
			redacted = nil
		}
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
// redacted carries the round's redacted reasoning blobs (GPT 5.6); they are
// replayed as redacted_thinking blocks so buildHistory can attach the blob to
// the in-flight tool round in the next request.
func (o *toolSearchOrchestrator) appendSearchMessages(msgs []anthropic.Message, srvToolUseID string, searchInput map[string]any, results []string, searchErr error, nameMap *reqconv.ToolNameMap, redacted []string) []anthropic.Message {
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
	assistantBlocks := make([]anthropic.ContentBlock, 0, len(redacted)+1)
	for _, data := range redacted {
		assistantBlocks = append(assistantBlocks, anthropic.ContentBlock{Type: anthropic.BlockTypeRedactedThinking, Data: data})
	}
	assistantBlocks = append(assistantBlocks, anthropic.ContentBlock{Type: anthropic.BlockTypeServerToolUse, ID: srvToolUseID, Name: toolsearch.KiroToolSearchName, Input: searchInput})
	return append(msgs,
		anthropic.Message{
			Role:    "assistant",
			Content: anthropic.MessageContent{Blocks: assistantBlocks},
		},
		anthropic.Message{
			Role: "user",
			Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{
				{Type: anthropic.BlockTypeToolResult, ToolUseID: srvToolUseID, Content: resultContent, IsError: isError},
			}},
		},
	)
}

// textOfContentBlocks joins the text of block maps produced by BuildResponse.
// The non-streaming path uses it to recover the round's preamble text for the
// synthetic web-search history.
func textOfContentBlocks(content []any) string {
	var sb strings.Builder
	for _, c := range content {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != anthropic.BlockTypeText {
			continue
		}
		if t, ok := m["text"].(string); ok {
			sb.WriteString(t)
		}
	}
	return sb.String()
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

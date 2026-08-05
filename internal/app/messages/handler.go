package messages

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/httpx"
	"github.com/d-kuro/kirocc/internal/logging"
	"github.com/d-kuro/kirocc/internal/models"
	"github.com/d-kuro/kirocc/internal/reqconv"
	"github.com/d-kuro/kirocc/internal/toolsearch"
)

const headerCCSessionID = "X-Claude-Code-Session-Id"

func (s *Service) HandleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	traceID, short := logging.TraceIDs(ctx)

	req, err := parseAndValidateRequest(ctx, w, r, s.maxBodySize)
	if err != nil {
		slog.WarnContext(ctx, "invalid request", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
		return
	}

	ccSessionID := r.Header.Get(headerCCSessionID)
	if ccSessionID == "" {
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, "missing "+headerCCSessionID+" header")
		return
	}
	ctx = logging.WithSessionID(ctx, ccSessionID)
	r = r.WithContext(ctx)

	slog.DebugContext(ctx, "client request headers",
		"trace_id", traceID,
		"session_id", ccSessionID,
		"headers", logging.SafeHeaders{H: r.Header},
	)

	creds, err := s.auth.GetToken(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "auth error", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusUnauthorized, ErrTypeAuthentication, "authentication failed")
		return
	}

	kiroModel, thinking, contextWindowSize, anthropicModel := models.Resolve(req.Model, anthropic.HasContext1MBeta(r.Header))
	if req.IsThinkingEnabled() {
		thinking = true
	}

	s.logRequest(ctx, short, ccSessionID, kiroModel, contextWindowSize, req, thinking)

	effort := resolveEffort(ctx, kiroModel, req, thinking)

	// Tool search and Kiro-hosted web search both need the orchestrator's round
	// loop, so either one short-circuits into it. The orchestrator has its own
	// retry handling.
	tsCtx := toolsearch.NewContext(req.Tools)
	var wsOpts *reqconv.WebSearchOptions
	if s.webSearchEnabled() {
		wsOpts = reqconv.WebSearchOptionsFrom(req.Tools, tsCtx)
	}
	if tsCtx != nil || wsOpts != nil {
		if tsCtx != nil {
			refs := reqconv.ExtractToolReferences(req.Messages)
			tsCtx.PromoteTools(refs)
			slog.InfoContext(ctx, "tool search enabled",
				"trace_id", short,
				"search_type", tsCtx.SearchType,
				"deferred_tools", len(tsCtx.DeferredTools),
				"active_tools", len(tsCtx.ActiveTools),
			)
		}
		if wsOpts != nil {
			slog.InfoContext(ctx, "kiro web search enabled", "trace_id", short,
				"max_uses", wsOpts.MaxUses,
				"allowed_domains", len(wsOpts.AllowedDomains),
				"blocked_domains", len(wsOpts.BlockedDomains))
		}
		s.runToolSearch(ctx, w, req, creds, tsCtx, wsOpts, kiroModel, anthropicModel, contextWindowSize, thinking, effort, ccSessionID, short)
		return
	}

	payload, nameMap, err := reqconv.BuildPayload(req, reqconv.BuildOptions{
		ProfileARN:        creds.ProfileARN,
		ModelID:           kiroModel,
		ConversationID:    ccSessionID,
		Effort:            effort,
		HistoryImageTurns: s.historyImageTurns,
	})
	if err != nil {
		slog.WarnContext(ctx, "payload build error", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
		return
	}

	s.executeWithRetry(ctx, w, &invocation{
		req:               req,
		payload:           payload,
		creds:             creds,
		model:             kiroModel,
		responseModel:     anthropicModel,
		contextWindowSize: contextWindowSize,
		thinking:          thinking,
		toolNameMap:       nameMap.ReverseMap(),
	})
}

// logRequest emits the "--> POST /v1/messages" info log summarizing the call.
func (s *Service) logRequest(ctx context.Context, short, ccSessionID, kiroModel string, contextWindowSize int, req *anthropic.Request, thinking bool) {
	var thinkingLog any = false
	if thinking {
		if effort := req.Effort(); effort != "" {
			thinkingLog = effort
		} else {
			thinkingLog = "enabled"
		}
	}
	slog.InfoContext(ctx, "--> POST /v1/messages",
		"trace_id", short,
		"session_id", logging.ShortID(ccSessionID),
		"model", kiroModel,
		"thinking", thinkingLog,
		"stream", req.Stream,
		"context_window", formatContextWindow(contextWindowSize),
	)
}

// runToolSearch wires up the orchestrator and retries once on empty-visible end_turn.
func (s *Service) runToolSearch(ctx context.Context, w http.ResponseWriter, req *anthropic.Request, creds *auth.Credentials, tsCtx *toolsearch.Context, wsOpts *reqconv.WebSearchOptions, kiroModel, responseModel string, contextWindowSize int, thinking bool, effort string, ccSessionID, short string) {
	orch := &toolSearchOrchestrator{
		service: s,
		tsCtx:   tsCtx,
		wsOpts:  wsOpts,
		req:     req,
		creds:   creds,
		buildOpts: reqconv.BuildOptions{
			ProfileARN:        creds.ProfileARN,
			ModelID:           kiroModel,
			ConversationID:    ccSessionID,
			Effort:            effort,
			HistoryImageTurns: s.historyImageTurns,
			ToolSearchCtx:     tsCtx,
			WebSearch:         wsOpts != nil,
		},
		contextWindowSize: contextWindowSize,
		responseModel:     responseModel,
	}
	if req.Stream {
		session := newStreamSession(ctx, w, s.keepAliveInterval)
		defer session.Stop()
		s.runToolSearchWithRetry(session.Context(), session, session, orch, short)
		return
	}
	s.runToolSearchWithRetry(ctx, w, nil, orch, short)
}

func (s *Service) runToolSearchWithRetry(ctx context.Context, w http.ResponseWriter, session *streamSession, orch *toolSearchOrchestrator, short string) {
	reason := orch.run(ctx, w, session)
	if reason != retryReasonEmptyVisibleEndTurn {
		return
	}
	slog.WarnContext(ctx, "retrying tool search after empty visible end_turn", "trace_id", short)
	if r2 := orch.run(ctx, w, session); r2 == retryReasonEmptyVisibleEndTurn {
		slog.ErrorContext(ctx, "tool search retry also returned empty visible end_turn", "trace_id", short)
		if session != nil {
			_ = session.WriteFinalError(newStreamFinalError(http.StatusBadGateway, errTypeAPI, "upstream returned empty response"), nil)
		} else {
			httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream returned empty response")
		}
	}
}

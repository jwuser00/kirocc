package reqconv

import (
	"github.com/d-kuro/kirocc/internal/anthropic"
	"github.com/d-kuro/kirocc/internal/kiromcp"
	"github.com/d-kuro/kirocc/internal/kiroproto"
	"github.com/d-kuro/kirocc/internal/toolsearch"
	"github.com/google/uuid"
)

// BuildOptions controls how an Anthropic request is mapped to a Kiro payload.
type BuildOptions struct {
	ProfileARN     string
	ModelID        string
	ConversationID string
	// Effort is the resolved reasoning effort level (low/medium/high/xhigh/max).
	// Empty means the model does not support effort or none was requested, in
	// which case additionalModelRequestFields is omitted entirely.
	Effort string
	// HistoryImageTurns is how many earlier user turns still contribute replayed
	// images to the current message. Kiro history entries cannot carry images, so
	// without replay an image is only visible on the turn it arrives. Counted in
	// turns rather than images so a set attached together expires together. Zero
	// disables replay; negative means unlimited.
	HistoryImageTurns int
	ToolSearchCtx     *toolsearch.Context
	// WebSearch enables translating Anthropic's WebSearch server tool into the
	// Kiro-hosted web_search tool. Anthropic server tools are stripped either
	// way; this only controls whether WebSearch gets a working replacement.
	WebSearch bool
}

// BuildPayload converts an Anthropic request into a Kiro API payload.
func BuildPayload(req *anthropic.Request, options BuildOptions) (*kiroproto.Payload, *ToolNameMap, error) {
	nameMap := NewToolNameMap()

	// 1. Build system prompt and convert tools.
	systemPrompt, toolEntries := buildSystemAndTools(req, options.ToolSearchCtx, nameMap, options.WebSearch)

	// envState is derived from the system prompt's <env> block (no host
	// fallback) and only ever attached to the current message.
	envState := ParseEnvState(systemPrompt)

	// 2. Normalize and split messages.
	hasTools := len(req.Tools) > 0
	if options.ToolSearchCtx != nil {
		hasTools = true
	}
	msgs := Normalize(req.Messages, hasTools)
	historyMsgs, lastMsg := splitMessages(msgs)

	// 3. Build history and place system prompt.
	history := buildHistory(historyMsgs, nameMap)
	history, lastContent := placeSystemPrompt(systemPrompt, history, ExtractTextContent(lastMsg.Content))

	// 4. Build currentMessage.
	// Extract tool_use IDs from the preceding assistant message for reordering tool results.
	var precedingToolUseIDs []string
	if len(historyMsgs) > 0 {
		precedingToolUseIDs = extractToolUseIDs(historyMsgs[len(historyMsgs)-1])
	}
	userInputMessage := buildCurrentMessage(lastMsg, lastContent, options.ModelID, toolEntries, envState, precedingToolUseIDs,
		collectHistoryImages(historyMsgs, options.HistoryImageTurns))

	convState := kiroproto.ConversationState{
		ConversationID:  options.ConversationID,
		ChatTriggerType: kiroproto.ChatTriggerTypeManual,
		AgentTaskType:   kiroproto.AgentTaskTypeVibe,
		CurrentMessage:  kiroproto.CurrentMessage{UserInputMessage: userInputMessage},
	}
	if len(history) > 0 {
		convState.History = history
	}
	payload := &kiroproto.Payload{ConversationState: convState}
	if options.ProfileARN != "" {
		payload.ProfileARN = options.ProfileARN
	}
	if options.Effort != "" {
		payload.AdditionalModelRequestFields = &kiroproto.AdditionalModelRequestFields{
			OutputConfig: &kiroproto.OutputConfig{Effort: options.Effort},
		}
	}
	return payload, nameMap, nil
}

// buildSystemAndTools extracts the system prompt and converts tools.
func buildSystemAndTools(req *anthropic.Request, tsCtx *toolsearch.Context, nameMap *ToolNameMap, webSearch bool) (string, []kiroproto.ToolEntry) {
	systemPrompt := ExtractSystemPrompt(req.System)

	// Anthropic's server tools carry no input_schema and would make Kiro reject
	// the whole request, so they are stripped here regardless of mode. A
	// WebSearch declaration is swapped for the Kiro-hosted equivalent below.
	var (
		tools         []anthropic.Tool
		wantWebSearch bool
		toolSearch    = tsCtx != nil
	)
	if toolSearch {
		tools, wantWebSearch = RewriteServerTools(tsCtx.ActiveTools, webSearch)
	} else {
		tools, wantWebSearch = RewriteServerTools(req.Tools, webSearch)
	}

	var toolEntries []kiroproto.ToolEntry
	if toolSearch || len(tools) > 0 || wantWebSearch {
		toolEntries = ConvertTools(tools, nameMap)
		toolEntries = ApplyToolCachePoints(tools, toolEntries)
	}
	if toolSearch {
		toolEntries = append(toolEntries, toolsearch.KiroToolSearchEntry())
	}
	if wantWebSearch {
		toolEntries = append(toolEntries, kiromcp.WebSearchToolEntry())
	}
	return systemPrompt, toolEntries
}

// splitMessages splits normalized messages into history messages and the last message.
// If the last message is from the assistant, all messages go to history and a
// synthetic "Continue" user message is returned.
func splitMessages(msgs []anthropic.Message) (history []anthropic.Message, last anthropic.Message) {
	if len(msgs) == 0 {
		return nil, anthropic.Message{}
	}
	if msgs[len(msgs)-1].Role == "assistant" {
		return msgs, anthropic.Message{
			Role:    "user",
			Content: anthropic.MessageContent{Text: syntheticContinue},
		}
	}
	return msgs[:len(msgs)-1], msgs[len(msgs)-1]
}

// syntheticAck is the synthetic assistant acknowledgment that kiro-cli always
// inserts after the system prompt in history. v2 captures confirm this is present
// in every request.
const syntheticAck = "I will fully incorporate this information when generating my responses, and explicitly acknowledge relevant parts of the summary when answering questions."

// syntheticAckMessageID is a deterministic UUID for the synthetic ack, computed once since the input is constant.
var syntheticAckMessageID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("synthetic-ack:"+syntheticAck)).String()

// placeSystemPrompt inserts the system prompt as a dedicated history entry pair
// (user message + synthetic assistant ack), matching the v2 kiro-cli structure.
// v2 captures show this pair is present in every request, even the first one.
// Returns a new history slice (original is not mutated) and the updated lastContent.
func placeSystemPrompt(systemPrompt string, history []kiroproto.HistoryEntry, lastContent string) ([]kiroproto.HistoryEntry, string) {
	if systemPrompt == "" {
		return history, lastContent
	}
	// Always build the system prompt pair: user message + synthetic assistant ack.
	systemPair := []kiroproto.HistoryEntry{
		{UserInputMessage: &kiroproto.HistoryUserInputMessage{
			Content: systemPrompt,
			Origin:  kiroproto.OriginKiroCLI,
		}},
		{AssistantResponseMessage: &kiroproto.AssistantResponseMessage{
			MessageID: syntheticAckMessageID,
			Content:   syntheticAck,
		}},
	}
	newHistory := make([]kiroproto.HistoryEntry, 0, len(systemPair)+len(history))
	newHistory = append(newHistory, systemPair...)
	newHistory = append(newHistory, history...)
	return newHistory, lastContent
}

// buildCurrentMessage constructs the Kiro UserInputMessage from the last Anthropic message.
// historyImages are images from earlier turns, replayed here because Kiro
// history entries cannot carry them; they precede this turn's own images so the
// ordering still runs oldest to newest.
func buildCurrentMessage(lastMsg anthropic.Message, lastContent, modelID string, toolEntries []kiroproto.ToolEntry, envState *kiroproto.EnvState, precedingToolUseIDs []string, historyImages []kiroproto.Image) kiroproto.UserInputMessage {
	msg := kiroproto.UserInputMessage{
		Content: lastContent,
		ModelID: modelID,
		Origin:  kiroproto.OriginKiroCLI,
	}

	// Single-pass scan of lastMsg.Content to extract both tool_results and images.
	toolResults, images := scanCurrentMessage(lastMsg.Content)
	toolResults = ReorderToolResults(toolResults, precedingToolUseIDs)
	if envState != nil || len(toolEntries) > 0 || len(toolResults) > 0 {
		// Field order matches the wire format: envState before tools.
		ctx := &kiroproto.UserInputMessageContext{}
		if envState != nil {
			ctx.EnvState = envState
		}
		if len(toolEntries) > 0 {
			ctx.Tools = toolEntries
		}
		if len(toolResults) > 0 {
			ctx.ToolResults = toolResults
		}
		msg.UserInputMessageContext = ctx
	}

	// Match the observed kiro-cli continuation shape:
	// tool-result-only turns keep empty currentMessage.content instead of "Continue".
	if msg.Content == "" && len(toolResults) == 0 {
		msg.Content = syntheticContinue
	}

	if len(historyImages) > 0 || len(images) > 0 {
		msg.Images = append(historyImages, images...)
	}
	// Replayed images arrive in the same place as ones the user just sent, so
	// say which are which. Without this the model reads a screenshot from ten
	// turns ago as part of the current question.
	if len(historyImages) > 0 {
		msg.Content = appendHistoryImageNote(msg.Content, len(historyImages))
	}

	return msg
}

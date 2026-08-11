package proxy

import (
	"kiro-go/config"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractOpenAIMessageTextStructured(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "alpha"},
		map[string]interface{}{"type": "input_text", "text": "beta"},
	}

	if got := extractOpenAIMessageText(content); got != "alphabeta" {
		t.Fatalf("expected concatenated structured text, got %q", got)
	}

	nested := map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "nested"}},
	}
	if got := extractOpenAIMessageText(nested); got != "nested" {
		t.Fatalf("expected nested content extraction, got %q", got)
	}
}

func TestOpenAIToKiroPreservesStructuredAssistantAndToolContent(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{
				Role: "system",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "system-a"},
					map[string]interface{}{"type": "text", "text": "system-b"},
				},
			},
			{Role: "user", Content: "first-question"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "assistant-structured"},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "tool-result-structured"},
				},
			},
		},
	}

	payload := OpenAIToKiro(req, false)

	// History starts with a priming pair.
	if len(payload.ConversationState.History) != 4 {
		t.Fatalf("expected 4 history items (2 priming + 2 conversation), got %d", len(payload.ConversationState.History))
	}

	// history[0]: priming user
	primingUser := payload.ConversationState.History[0].UserInputMessage
	if primingUser == nil {
		t.Fatalf("expected history[0] to be priming user message")
	}
	if !strings.Contains(primingUser.Content, "system-a") || !strings.Contains(primingUser.Content, "system-b") {
		t.Fatalf("expected priming user message to contain system prompt, got %q", primingUser.Content)
	}
	if strings.Contains(primingUser.Content, "first-question") {
		t.Fatalf("expected system prompt priming not to contain user question, got %q", primingUser.Content)
	}

	// history[1]: priming assistant
	primingAssistant := payload.ConversationState.History[1].AssistantResponseMessage
	if primingAssistant == nil {
		t.Fatalf("expected history[1] to be priming assistant message")
	}
	if primingAssistant.Content != "I will follow these instructions." {
		t.Fatalf("expected priming assistant ack, got %q", primingAssistant.Content)
	}

	// history[2]: first user turn
	firstConvUser := payload.ConversationState.History[2].UserInputMessage
	if firstConvUser == nil {
		t.Fatalf("expected history[2] to be first conversation user message")
	}
	if !strings.Contains(firstConvUser.Content, "first-question") {
		t.Fatalf("expected history[2] to contain first-question, got %q", firstConvUser.Content)
	}

	// history[3]: assistant reply
	historyAssistant := payload.ConversationState.History[3].AssistantResponseMessage
	if historyAssistant == nil {
		t.Fatalf("expected history[3] to be assistant message")
	}
	if historyAssistant.Content != "assistant-structured" {
		t.Fatalf("expected assistant structured content to be preserved, got %q", historyAssistant.Content)
	}

	// The tool result answers call_1, but the last history assistant has no
	// matching structured tool call (it is text-only), so the tool result is an
	// orphan. Kiro's upstream rejects structured tool results that do not answer
	// the immediately preceding assistant tool call, so it must be narrated into
	// the current message text rather than kept structured.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "tool-result-structured") {
		t.Fatalf("expected tool-result continuation content, got %q", cur.Content)
	}
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) != 0 {
		t.Fatalf("expected orphan tool result to be flattened into text, not kept structured")
	}
}

func TestOpenAIToKiroAssistantMapContentInHistory(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: map[string]interface{}{"type": "text", "text": "assistant-map"}},
			{Role: "user", Content: "u2"},
		},
	}

	payload := OpenAIToKiro(req, false)

	if len(payload.ConversationState.History) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(payload.ConversationState.History))
	}
	assistant := payload.ConversationState.History[1].AssistantResponseMessage
	if assistant == nil {
		t.Fatalf("expected second history entry to be assistant")
	}
	if assistant.Content != "assistant-map" {
		t.Fatalf("expected assistant map content preserved, got %q", assistant.Content)
	}
}

func TestOpenAIToKiroAssistantToolCallsDoNotInjectPlaceholder(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find weather"},
			{
				Role:    "assistant",
				Content: nil,
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: "{}"},
				}},
			},
			{Role: "user", Content: "continue"},
		},
	}

	payload := OpenAIToKiro(req, false)

	// The mid-history assistant turn carried ONLY a tool call (no text) and is
	// not the active tool turn, so its structured toolUses are cleared. That
	// leaves it hollow, and a hollow assistant turn is dropped entirely rather
	// than backfilled with a "." placeholder (which the model would imitate).
	// No surviving turn may contain tool-invocation text or structured toolUses.
	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil {
			continue
		}
		if len(a.ToolUses) != 0 {
			t.Fatalf("history[%d] retains structured toolUses", i)
		}
		if strings.Contains(a.Content, "get_weather") || strings.Contains(a.Content, "[Called tool") {
			t.Fatalf("history[%d] assistant contains tool-invocation text: %q", i, a.Content)
		}
		if strings.TrimSpace(a.Content) == "." || strings.TrimSpace(a.Content) == "" {
			t.Fatalf("history[%d] is a hollow assistant turn that should have been dropped", i)
		}
	}
}

func TestOpenAIConversationIDStableFromAnchor(t *testing.T) {
	baseMessages := []OpenAIMessage{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "Build calculator"},
		{Role: "assistant", Content: "Sure"},
		{Role: "user", Content: "Continue"},
	}

	reqA := &OpenAIRequest{Model: "claude-sonnet-4.5", Messages: baseMessages}
	reqB := &OpenAIRequest{Model: "claude-sonnet-4.5", Messages: append(baseMessages, OpenAIMessage{Role: "assistant", Content: "Next step"})}

	payloadA := OpenAIToKiro(reqA, false)
	payloadB := OpenAIToKiro(reqB, false)

	if payloadA.ConversationState.ConversationID == "" || payloadB.ConversationState.ConversationID == "" {
		t.Fatalf("expected non-empty conversation IDs")
	}
	if payloadA.ConversationState.ConversationID != payloadB.ConversationState.ConversationID {
		t.Fatalf("expected stable conversation ID across turns, got %q vs %q", payloadA.ConversationState.ConversationID, payloadB.ConversationState.ConversationID)
	}
}

func TestClaudeConversationIDStableFromAnchor(t *testing.T) {
	reqA := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
		},
	}
	reqB := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "next"},
		},
	}

	payloadA := ClaudeToKiro(reqA, false)
	payloadB := ClaudeToKiro(reqB, false)

	if payloadA.ConversationState.ConversationID == "" || payloadB.ConversationState.ConversationID == "" {
		t.Fatalf("expected non-empty conversation IDs")
	}
	if payloadA.ConversationState.ConversationID != payloadB.ConversationState.ConversationID {
		t.Fatalf("expected stable conversation ID across turns, got %q vs %q", payloadA.ConversationState.ConversationID, payloadB.ConversationState.ConversationID)
	}
}

func TestOpenAIConversationIDRandomForSyntheticAnchor(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "assistant", Content: "prefill"},
		},
	}

	payloadA := OpenAIToKiro(req, false)
	payloadB := OpenAIToKiro(req, false)

	if payloadA.ConversationState.ConversationID == payloadB.ConversationState.ConversationID {
		t.Fatalf("expected synthetic anchor to generate non-deterministic conversation IDs")
	}
}

func TestClaudeToKiroDropsLeadingAssistantHistory(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: "prefill"},
			{Role: "user", Content: "real user message"},
		},
	}

	payload := ClaudeToKiro(req, false)

	if len(payload.ConversationState.History) != 0 {
		t.Fatalf("expected leading assistant-only history to be dropped, got %d entries", len(payload.ConversationState.History))
	}

	if strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "Begin conversation") {
		t.Fatalf("unexpected synthetic Begin conversation injection in current content: %q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
}

func TestKiroToClaudeResponseCanEmitEmptyThinkingBlock(t *testing.T) {
	resp := KiroToClaudeResponse("final answer", "", true, nil, 10, 20, "claude-sonnet-4.6")

	if len(resp.Content) != 2 {
		t.Fatalf("expected empty thinking block plus text block, got %d blocks", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" {
		t.Fatalf("expected first block to be thinking, got %#v", resp.Content[0])
	}
	if resp.Content[0].Thinking != "" {
		t.Fatalf("expected omitted thinking block to have empty content, got %#v", resp.Content[0].Thinking)
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "final answer" {
		t.Fatalf("expected text block to be preserved, got %#v", resp.Content[1])
	}
}

func TestToolResultsContinuationIncludesInstructionPrefix(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find data"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "fetch", Arguments: "{}"},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "result-1"},
		},
	}

	payload := OpenAIToKiro(req, false)
	content := payload.ConversationState.CurrentMessage.UserInputMessage.Content

	if !strings.Contains(content, toolResultsContinuationPrefix) {
		t.Fatalf("expected tool continuation prefix, got %q", content)
	}
	if !strings.Contains(content, "result-1") {
		t.Fatalf("expected tool result text in continuation content, got %q", content)
	}
}

func TestEnsureObjectSchemaRemovesKiroRejectedFieldsRecursively(t *testing.T) {
	input := map[string]interface{}{
		"type":                 "object",
		"required":             []interface{}{},
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":                 "string",
				"required":             nil,
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
			"options": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"force": map[string]interface{}{"type": "boolean"},
				},
			},
		},
		"anyOf": []interface{}{
			map[string]interface{}{
				"type":                 "object",
				"required":             []interface{}{},
				"additionalProperties": false,
			},
		},
	}

	got := ensureObjectSchema(input).(map[string]interface{})
	if schemaContainsKey(got, "additionalProperties") {
		t.Fatalf("expected additionalProperties to be removed recursively, got %#v", got)
	}
	if schemaContainsKey(got, "required") {
		t.Fatalf("expected empty/nil required fields to be removed recursively, got %#v", got)
	}
	if _, stillPresent := input["additionalProperties"]; !stillPresent {
		t.Fatalf("expected sanitizer not to mutate caller schema")
	}
}

func TestConvertOpenAIToolsSanitizesSchemaAndDescription(t *testing.T) {
	var tool OpenAITool
	tool.Type = "function"
	tool.Function.Name = "read_file"
	tool.Function.Parameters = map[string]interface{}{
		"type":                 "object",
		"required":             []string{},
		"additionalProperties": false,
	}

	tools := convertOpenAITools([]OpenAITool{tool})
	if len(tools) != 1 {
		t.Fatalf("expected one converted tool, got %d", len(tools))
	}
	if strings.TrimSpace(tools[0].ToolSpecification.Description) == "" {
		t.Fatalf("expected fallback tool description")
	}
	schema := tools[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if schemaContainsKey(schema, "additionalProperties") {
		t.Fatalf("expected OpenAI tool schema to be sanitized, got %#v", schema)
	}
	if schemaContainsKey(schema, "required") {
		t.Fatalf("expected empty required field to be removed, got %#v", schema)
	}
}

func schemaContainsKey(value interface{}, key string) bool {
	switch v := value.(type) {
	case map[string]interface{}:
		if _, ok := v[key]; ok {
			return true
		}
		for _, child := range v {
			if schemaContainsKey(child, key) {
				return true
			}
		}
	case []interface{}:
		for _, child := range v {
			if schemaContainsKey(child, key) {
				return true
			}
		}
	}
	return false
}

func TestParseModelAndThinking(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantModel    string
		wantThinking bool
	}{
		// Format normalization: dash → dot for new versions without code changes.
		{"new opus dash form", "claude-opus-4-8", "claude-opus-4.8", false},
		{"new opus dot form", "claude-opus-4.8", "claude-opus-4.8", false},
		{"existing opus dash form", "claude-opus-4-7", "claude-opus-4.7", false},
		{"existing opus dot form", "claude-opus-4.7", "claude-opus-4.7", false},
		{"sonnet dash form", "claude-sonnet-4-6", "claude-sonnet-4.6", false},
		{"sonnet dot form", "claude-sonnet-4.6", "claude-sonnet-4.6", false},
		{"haiku dash form", "claude-haiku-4-5", "claude-haiku-4.5", false},
		{"haiku dot form", "claude-haiku-4.5", "claude-haiku-4.5", false},
		{"future major bump", "claude-sonnet-5-0", "claude-sonnet-5.0", false},

		// Bare family name passes through (no minor to normalize).
		{"bare sonnet 4", "claude-sonnet-4", "claude-sonnet-4", false},

		// Dated snapshot must hit the alias before the regex rewrites it.
		{"dated sonnet snapshot", "claude-sonnet-4-20250514", "claude-sonnet-4", false},

		// Cross-family legacy IDs.
		{"claude 3.5 sonnet", "claude-3-5-sonnet", "claude-sonnet-4.5", false},
		{"claude 3 opus", "claude-3-opus", "claude-sonnet-4.5", false},
		{"claude 3 sonnet", "claude-3-sonnet", "claude-sonnet-4", false},
		{"claude 3 haiku", "claude-3-haiku", "claude-haiku-4.5", false},

		// Non-Anthropic fallbacks.
		{"gpt-4-turbo", "gpt-4-turbo", "claude-sonnet-4.5", false},
		{"gpt-4o", "gpt-4o", "claude-sonnet-4.5", false},
		{"gpt-4", "gpt-4", "claude-sonnet-4.5", false},
		{"gpt-3.5-turbo", "gpt-3.5-turbo", "claude-sonnet-4.5", false},

		// Thinking suffix is stripped before mapping.
		{"thinking suffix on dash form", "claude-opus-4-8-thinking", "claude-opus-4.8", true},
		{"thinking suffix on dot form", "claude-sonnet-4.5-thinking", "claude-sonnet-4.5", true},
		{"thinking suffix on legacy alias", "claude-3-5-sonnet-thinking", "claude-sonnet-4.5", true},

		// Unknown models pass through unchanged.
		{"unknown model", "some-other-model", "some-other-model", false},
		{"misspelled claude family", "claude-opux-4-8", "claude-opux-4-8", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotThinking := ParseModelAndThinking(tc.input, "-thinking")
			if gotModel != tc.wantModel {
				t.Errorf("model: got %q, want %q", gotModel, tc.wantModel)
			}
			if gotThinking != tc.wantThinking {
				t.Errorf("thinking: got %v, want %v", gotThinking, tc.wantThinking)
			}
		})
	}
}

func TestParseModelAndThinkingDoesNotRewriteDatedSnapshotMinor(t *testing.T) {
	// Guards the \b boundary in claudeVersionPattern: without it, the regex would
	// rewrite "claude-sonnet-4-20250514" to "claude-sonnet-4.20250514" before the
	// alias table could redirect it.
	got, _ := ParseModelAndThinking("claude-sonnet-4-20250514", "-thinking")
	if got != "claude-sonnet-4" {
		t.Fatalf("dated snapshot must alias to claude-sonnet-4, got %q", got)
	}
	if strings.Contains(got, ".") {
		t.Fatalf("dated snapshot must not be rewritten with a dot, got %q", got)
	}
}

func TestClaudeToolResultImageAttachedToCurrentMessage(t *testing.T) {
	const imgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "read this image"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "tool_use", "id": "tool_1", "name": "read", "input": map[string]interface{}{"path": "a.png"}},
				},
			},
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tool_1",
						"content": []interface{}{
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       imgData,
								},
							},
						},
					},
				},
			},
		},
	}

	payload := ClaudeToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected tool_result image attached to current message, got %d images", len(cur.Images))
	}
	if cur.Images[0].Format != "png" || cur.Images[0].Source.Bytes != imgData {
		t.Fatalf("unexpected image payload: %+v", cur.Images[0])
	}
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected one tool result preserved")
	}
	if strings.TrimSpace(cur.UserInputMessageContext.ToolResults[0].Content[0].Text) == "" {
		t.Fatalf("expected non-empty placeholder text for image-only tool result")
	}
}

func TestClaudeToolResultMixedTextAndImage(t *testing.T) {
	const imgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "take a screenshot"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "tool_use", "id": "tool_2", "name": "screenshot", "input": map[string]interface{}{}},
				},
			},
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tool_2",
						"content": []interface{}{
							map[string]interface{}{"type": "text", "text": "here is the screenshot"},
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       imgData,
								},
							},
						},
					},
				},
			},
		},
	}

	payload := ClaudeToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected one image extracted, got %d", len(cur.Images))
	}
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected one tool result")
	}
	gotText := cur.UserInputMessageContext.ToolResults[0].Content[0].Text
	if gotText != "here is the screenshot" {
		t.Fatalf("expected original tool text preserved, got %q", gotText)
	}
}

func TestOpenAIToolResultImageAttachedToCurrentMessage(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "look at the file"},
			{
				Role:       "tool",
				ToolCallID: "call_img",
				Content: []interface{}{
					map[string]interface{}{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": dataURL},
					},
				},
			},
		},
	}

	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected tool image attached to current message, got %d", len(cur.Images))
	}
	if cur.Images[0].Format != "png" {
		t.Fatalf("expected png format, got %q", cur.Images[0].Format)
	}
}

func TestOpenAIToolResultImageCarriedWhenFollowedByUser(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "look at the file"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID:   "call_img",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: "read", Arguments: `{"path":"a.png"}`},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_img",
				Content: []interface{}{
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": dataURL}},
				},
			},
			{Role: "user", Content: "what do you see?"},
		},
	}

	payload := OpenAIToKiro(req, false)

	var toolHistImages int
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil {
			toolHistImages += len(h.UserInputMessage.Images)
		}
	}

	if toolHistImages != 1 {
		t.Fatalf("expected tool image carried on the flushed tool-result history entry, got %d", toolHistImages)
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 0 {
		t.Fatalf("tool image should not leak into a later user message, got %d on current", len(cur.Images))
	}
}

func TestResolveClaudeConversationID_Deterministic(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4",
		System: "You are a helpful assistant.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Hello, what is 1+1?"},
			{Role: "assistant", Content: "2"},
			{Role: "user", Content: "And 2+2?"},
		},
		MaxTokens: 100,
	}
	id1 := ResolveClaudeConversationID(nil, req)
	if id1 == "" {
		t.Fatal("want non-empty convID for real anchor")
	}
	// 第二次请求：历史更长但首条 user message 不变
	req2 := &ClaudeRequest{
		Model: "claude-sonnet-4",
		System: "You are a helpful assistant.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Hello, what is 1+1?"},
			{Role: "assistant", Content: "2"},
			{Role: "user", Content: "And 2+2?"},
			{Role: "assistant", Content: "4"},
			{Role: "user", Content: "And 3+3?"},
		},
		MaxTokens: 100,
	}
	id2 := ResolveClaudeConversationID(nil, req2)
	if id1 != id2 {
		t.Fatalf("same anchor+system+model should produce same convID: %q vs %q", id1, id2)
	}
}

func TestResolveClaudeConversationID_SyntheticAnchorEmpty(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "."},
		},
		MaxTokens: 100,
	}
	if id := ResolveClaudeConversationID(nil, req); id != "" {
		t.Fatalf("synthetic anchor should yield empty convID, got %q", id)
	}
}

func TestResolveClaudeConversationID_NoAnchorEmpty(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4",
		Messages: []ClaudeMessage{{Role: "assistant", Content: "hi"}},
		MaxTokens: 100,
	}
	if id := ResolveClaudeConversationID(nil, req); id != "" {
		t.Fatalf("no user anchor should yield empty convID, got %q", id)
	}
}

func TestResolveOpenAIConversationID_Deterministic(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-4",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Hello, what is 1+1?"},
			{Role: "assistant", Content: "2"},
		},
	}
	id1 := ResolveOpenAIConversationID(nil, req)
	if !strings.Contains(id1, "-") {
		t.Fatalf("want uuid-like convID, got %q", id1)
	}

	// 历史更长但首条 user message 不变，应返回相同 ID
	req2 := &OpenAIRequest{
		Model: "gpt-4",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Hello, what is 1+1?"},
			{Role: "assistant", Content: "2"},
			{Role: "user", Content: "And 2+2?"},
		},
	}
	id2 := ResolveOpenAIConversationID(nil, req2)
	if id1 != id2 {
		t.Fatalf("same anchor+system+model should produce same convID: %q vs %q", id1, id2)
	}
}

func TestResolveOpenAIConversationID_SyntheticAnchorEmpty(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-4",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "."},
		},
	}
	if id := ResolveOpenAIConversationID(nil, req); id != "" {
		t.Fatalf("synthetic anchor should yield empty convID, got %q", id)
	}
}

func TestResolveOpenAIConversationID_NoAnchorEmpty(t *testing.T) {
	req := &OpenAIRequest{
		Model:    "gpt-4",
		Messages: []OpenAIMessage{{Role: "assistant", Content: "hi"}},
	}
	if id := ResolveOpenAIConversationID(nil, req); id != "" {
		t.Fatalf("no user anchor should yield empty convID, got %q", id)
	}
}

func TestResolveResponsesConversationID_Deterministic(t *testing.T) {
	// 模拟 Responses 场景：instructions 作为 system，input 含多轮消息。
	messages := []OpenAIMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello, what is 1+1?"},
		{Role: "assistant", Content: "2"},
	}
	id1 := ResolveResponsesConversationID(nil, "claude-sonnet-4", messages, nil)
	if !strings.Contains(id1, "-") {
		t.Fatalf("want uuid-like convID, got %q", id1)
	}

	// 同一对话链（previous_response_id 展开后历史更长但首条 user 不变）应返回相同 ID
	continued := []OpenAIMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello, what is 1+1?"},
		{Role: "assistant", Content: "2"},
		{Role: "user", Content: "And 2+2?"},
	}
	id2 := ResolveResponsesConversationID(nil, "claude-sonnet-4", continued, nil)
	if id1 != id2 {
		t.Fatalf("same anchor+system+model should produce same convID: %q vs %q", id1, id2)
	}
}

func TestResolveResponsesConversationID_SyntheticAnchorEmpty(t *testing.T) {
	messages := []OpenAIMessage{
		{Role: "user", Content: "."},
	}
	if id := ResolveResponsesConversationID(nil, "claude-sonnet-4", messages, nil); id != "" {
		t.Fatalf("synthetic anchor should yield empty convID, got %q", id)
	}
}

func TestResolveResponsesConversationID_NoAnchorEmpty(t *testing.T) {
	messages := []OpenAIMessage{{Role: "assistant", Content: "hi"}}
	if id := ResolveResponsesConversationID(nil, "claude-sonnet-4", messages, nil); id != "" {
		t.Fatalf("no user anchor should yield empty convID, got %q", id)
	}
}

func TestResolveClaudeConversationID_SystemBlocks(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4",
		System: []interface{}{
			map[string]interface{}{"type": "text", "text": "You are helpful."},
			map[string]interface{}{"type": "text", "text": "Be concise."},
		},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Hello, what is 1+1?"},
		},
		MaxTokens: 100,
	}
	id1 := ResolveClaudeConversationID(nil, req)
	if id1 == "" {
		t.Fatal("want non-empty convID for real anchor with system blocks")
	}

	// 相同 system blocks 应产生相同 ID
	req2 := &ClaudeRequest{
		Model: "claude-sonnet-4",
		System: []interface{}{
			map[string]interface{}{"type": "text", "text": "You are helpful."},
			map[string]interface{}{"type": "text", "text": "Be concise."},
		},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Hello, what is 1+1?"},
			{Role: "assistant", Content: "2"},
			{Role: "user", Content: "And 2+2?"},
		},
		MaxTokens: 100,
	}
	id2 := ResolveClaudeConversationID(nil, req2)
	if id1 != id2 {
		t.Fatalf("same anchor+system blocks+model should produce same convID: %q vs %q", id1, id2)
	}
}

func TestResolveConversationID_ExplicitQueryOverridesAnchor(t *testing.T) {
	r, _ := http.NewRequest("POST", "/v1/messages?conversation_id=my-session-42", nil)
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	id1 := ResolveClaudeConversationID(r, req)
	if !strings.HasPrefix(id1, "sess_") {
		t.Fatalf("explicit query should produce sess_ prefix, got %q", id1)
	}
	// 同一显式 ID 下内容/模型不同也应稳定
	id2 := ResolveClaudeConversationID(r, &ClaudeRequest{
		Model:    "claude-opus-4",
		Messages: []ClaudeMessage{{Role: "user", Content: "different first msg"}},
	})
	if id1 != id2 {
		t.Fatalf("explicit session ID should be stable across content, got %q vs %q", id1, id2)
	}
}

func TestResolveConversationID_ExplicitHeader(t *testing.T) {
	r, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("X-Kiro-Conversation-Id", "sess-header-1")
	req := &OpenAIRequest{
		Model:    "gpt-4",
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}
	id := ResolveOpenAIConversationID(r, req)
	if !strings.HasPrefix(id, "sess_") {
		t.Fatalf("explicit header should produce sess_ prefix, got %q", id)
	}
}

func TestResolveConversationID_ContentAnchorPrefix(t *testing.T) {
	req := &OpenAIRequest{
		Model:    "gpt-4",
		Messages: []OpenAIMessage{{Role: "user", Content: "hello"}},
	}
	id := ResolveOpenAIConversationID(nil, req)
	if !strings.HasPrefix(id, "anch_") {
		t.Fatalf("content anchor should produce anch_ prefix, got %q", id)
	}
}

func TestResolveResponsesConversationID_MetadataOverridesChain(t *testing.T) {
	req := &ResponsesRequest{
		Model:    "claude-sonnet-4.5",
		Metadata: map[string]string{"conversation_id": "meta-session-9"},
	}
	id := ResolveResponsesConversationID(nil, "claude-sonnet-4.5", nil, req)
	if !strings.HasPrefix(id, "sess_") {
		t.Fatalf("metadata conversation_id should produce sess_ prefix, got %q", id)
	}
}

func TestResolveResponsesConversationID_ChainRoot(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	// 根 response
	root := &ResponsesObject{ID: "resp_root", Object: "response", CreatedAt: time.Now().Unix(), Status: "completed", Model: "claude-sonnet-4.5"}
	if err := saveResponse(root); err != nil {
		t.Fatalf("save root: %v", err)
	}
	// 中间 response，previous 指向 root；saveResponse 应归一 RootResponseID=resp_root
	mid := &ResponsesObject{ID: "resp_mid", Object: "response", CreatedAt: time.Now().Unix(), Status: "completed", Model: "claude-sonnet-4.5", PreviousResponseID: "resp_root"}
	if err := saveResponse(mid); err != nil {
		t.Fatalf("save mid: %v", err)
	}

	// 新请求 previous_response_id=resp_mid → 链根应为 resp_root
	req := &ResponsesRequest{Model: "claude-sonnet-4.5", PreviousResponseID: "resp_mid"}
	id := ResolveResponsesConversationID(nil, "claude-sonnet-4.5", nil, req)
	want := buildRootConversationID("resp_root")
	if id != want {
		t.Fatalf("chain root expected %q, got %q", want, id)
	}
}

func TestResolveClaudeConversationID_ClaudeCodeSessionHeader(t *testing.T) {
	r, _ := http.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("x-claude-code-session-id", "uuid-session-abc-123")
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4",
		System:   "You are helpful",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	id1 := ResolveClaudeConversationID(r, req)
	if !strings.HasPrefix(id1, "sess_") {
		t.Fatalf("claude-code session id should produce sess_ prefix, got %q", id1)
	}
	// 会话内不同轮次（system 动态变化、消息不同）也应以 session id 稳定
	id2 := ResolveClaudeConversationID(r, &ClaudeRequest{
		Model:    "claude-sonnet-4",
		System:   "You are helpful\n(cwd: /tmp) dynamic",
		Messages: []ClaudeMessage{{Role: "user", Content: "another turn"}},
	})
	if id1 != id2 {
		t.Fatalf("x-claude-code-session-id should be stable across turns, got %q vs %q", id1, id2)
	}
}

func TestResolveClaudeConversationID_SystemAnchorUsesFirstBlockOnly(t *testing.T) {
	// 模拟 Claude Code：首块 attribution 稳定，后续块含动态内容（cwd/git/日期/提醒）
	base := &ClaudeRequest{
		Model: "claude-sonnet-4",
		System: []interface{}{
			map[string]interface{}{"type": "text", "text": "stable attribution block"},
			map[string]interface{}{"type": "text", "text": "cwd: /repo, git: main, date: 2026-08-11"},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "build a calculator"}},
	}
	turn2 := &ClaudeRequest{
		Model: "claude-sonnet-4",
		System: []interface{}{
			map[string]interface{}{"type": "text", "text": "stable attribution block"},
			map[string]interface{}{"type": "text", "text": "cwd: /repo, git: main, date: 2026-08-11, reminder: check tests"},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "build a calculator"}},
	}
	id1 := ResolveClaudeConversationID(nil, base)
	id2 := ResolveClaudeConversationID(nil, turn2)
	if id1 == "" || id1 != id2 {
		t.Fatalf("anchor should be stable when only trailing dynamic system blocks change: %q vs %q", id1, id2)
	}
}


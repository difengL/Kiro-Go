package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

const defaultResponsesModel = "claude-sonnet-4.5"

func (h *Handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultResponsesModel
	}

	storedInputCopy := append(json.RawMessage(nil), req.Input...)

	storeResponse := true
	if req.Store != nil {
		storeResponse = *req.Store
	}

	var historyMessages []OpenAIMessage
	if req.PreviousResponseID != "" {
		prev, loadErr := loadResponse(req.PreviousResponseID)
		if loadErr != nil {
			h.sendOpenAIError(w, 404, "invalid_request_error",
				fmt.Sprintf("previous_response_id not found: %v", loadErr))
			return
		}
		historyMessages = expandPreviousResponseHistory(prev)
	}

	inputMessages, err := parseResponsesInput(req.Input)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}

	finalMessages := make([]OpenAIMessage, 0, len(historyMessages)+len(inputMessages)+1)
	finalMessages = append(finalMessages, historyMessages...)
	if strings.TrimSpace(req.Instructions) != "" {
		// New instructions on this turn always take effect, even when
		// continuing from previous_response_id. Place them after the
		// expanded history so they apply to the current and future turns,
		// while ancestor instructions (re-emitted by expandPreviousResponseHistory)
		// stay in scope for the historical exchanges they shaped.
		finalMessages = append(finalMessages, OpenAIMessage{
			Role:    "system",
			Content: req.Instructions,
		})
	}
	finalMessages = append(finalMessages, inputMessages...)

	if len(finalMessages) == 0 {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one message")
		return
	}

	hasUser := false
	for _, m := range finalMessages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one user message")
		return
	}

	openaiReq := &OpenAIRequest{
		Model:    req.Model,
		Messages: finalMessages,
		Stream:   req.Stream,
		Tools:    req.Tools,
	}
	if req.Temperature != nil {
		openaiReq.Temperature = *req.Temperature
	}
	if req.MaxOutputTokens != nil {
		openaiReq.MaxTokens = *req.MaxOutputTokens
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	openaiReq.Model = actualModel

	// 单次 BPE 编码同时产出总 token 与逐消息 token，供缓存 profile 复用。
	estimatedInputTokens, perMsgTokens := estimateOpenAIRequestInputTokensDetailed(openaiReq)
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	apiKeyID := apiKeyIDFromContext(r.Context())
	respID := generateResponseID()

	// 会话亲和：三级解析 convID——显式会话 ID（header/query/metadata conversation_id）
	// > previous_response_id 链根 > 内容锚点（基于组装后的完整消息列表）。
	convID, convSource := ResolveResponsesConversationIDWithSource(r, actualModel, finalMessages, &req)
	logger.Infof("[Responses] request model=%s stream=%t source=%s conv=%s previous_response_id_present=%t messages=%d",
		actualModel, req.Stream, convSource, responsesConversationLogID(convID), req.PreviousResponseID != "", len(finalMessages))

	// GPT 缓存命中模拟：与 Chat 路径共用同一个 OpenAIRequest 构建器。
	gptProfile := h.promptCache.BuildOpenAIProfile(openaiReq, estimatedInputTokens, perMsgTokens)

	if req.Stream {
		h.handleResponsesStream(w, kiroPayload, actualModel, thinking, estimatedInputTokens, gptProfile,
			apiKeyID, convID, convSource, respID, &req, storedInputCopy, storeResponse)
		return
	}

	h.handleResponsesNonStream(w, kiroPayload, actualModel, thinking, estimatedInputTokens, gptProfile,
		apiKeyID, convID, convSource, respID, &req, storedInputCopy, storeResponse)
}

func responsesConversationLogID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "<empty>"
	}
	if len(id) > 32 {
		return id[:32] + "..."
	}
	return id
}

func responsesAccountLogID(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.ID
}

func (h *Handler) handleResponsesNonStream(
	w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, gptProfile *promptCacheProfile, apiKeyID, convID, convSource, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	excluded := make(map[string]bool)
	var lastErr error
	reqStart := time.Now()

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account, affinityReason := h.pool.SelectForConversationWithReason(convID, model, excluded)
		logger.Infof("[Responses] route model=%s source=%s conv=%s reason=%s account=%s attempt=%d",
			model, convSource, responsesConversationLogID(convID), affinityReason, responsesAccountLogID(account), attempt+1)
		if account == nil {
			break
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.failAccount(convID, account, err)
			continue
		}

		gptUsage := h.promptCache.Compute(account.ID, gptProfile)

		var content, reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamUsage KiroTokenUsage

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(h.effectiveContextWindow(model)) / 100.0)
			},
			OnTokenUsage: func(usage KiroTokenUsage) { upstreamUsage = usage },
		}

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.failAccount(convID, account, err)
			continue
		}

		finalContent, _ := extractThinkingFromContent(content)
		if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)
		logger.Infof("[Responses] complete model=%s conv=%s account=%s input=%d output=%d upstream_input=%d uncached=%d cache_read=%d cache_write=%d cache_creation=%d cache_fields_present=%t",
			model, responsesConversationLogID(convID), account.ID, inputTokens, outputTokens, upstreamUsage.InputTokens,
			upstreamUsage.UncachedInputTokens, upstreamUsage.CacheReadInputTokens, upstreamUsage.CacheWriteInputTokens,
			upstreamUsage.CacheCreationInputTokens, upstreamUsage.CacheFieldsPresent)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("responses", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())
		h.promptCache.Update(account.ID, gptProfile)

		respObj := buildResponsesObject(respID, model, finalContent, toolUses, inputTokens, outputTokens, req, gptUsage.CacheReadInputTokens)
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions
		h.rememberResponsesAccount(convID, respObj, account.ID)

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}
	h.recordFailureWithDetails("responses", model, "", lastErr)
	h.sendOpenAIError(w, 500, "server_error", lastErr.Error())
}

// rememberResponsesAccount binds both the key used for this request and the
// stable Responses chain-root key. The first turn normally has only a content
// anchor; subsequent turns with previous_response_id use root_<id>, so both
// keys must point at the account selected for the first successful turn.
func (h *Handler) rememberResponsesAccount(convID string, resp *ResponsesObject, accountID string) {
	if resp == nil {
		return
	}

	rootID := resp.RootResponseID
	if rootID == "" {
		rootID = resp.ID
		if resp.PreviousResponseID != "" {
			if root := resolveChainRoot(resp.PreviousResponseID); root != "" {
				rootID = root
			}
		}
		resp.RootResponseID = rootID
	}

	h.pool.Remember(convID, accountID)
	if rootID != "" {
		h.pool.Remember(buildRootConversationID(rootID), accountID)
	}
}

func buildResponsesObject(
	id, model, content string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest, cachedTokens int,
) *ResponsesObject {
	output := make([]ResponseOutputItem, 0, 1+len(toolUses))

	if strings.TrimSpace(content) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: content,
			}},
		})
	}

	for _, tu := range toolUses {
		args, _ := json.Marshal(tu.Input)
		output = append(output, ResponseOutputItem{
			ID:        generateOutputItemID("fc"),
			Type:      "function_call",
			Status:    "completed",
			CallID:    tu.ToolUseID,
			Name:      tu.Name,
			Arguments: string(args),
		})
	}

	if len(output) == 0 {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "",
			}},
		})
	}

	resp := &ResponsesObject{
		ID:                 id,
		Object:             "response",
		CreatedAt:          time.Now().Unix(),
		Status:             "completed",
		Model:              model,
		Output:             output,
		Usage: ResponsesUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}

	// 官方语义：input_tokens 报全量输入，缓存命中单列。
	if cachedTokens > 0 {
		resp.Usage.InputTokensDetails = &ResponsesInputTokensDetails{CachedTokens: cachedTokens}
	}
	return resp
}

func (h *Handler) handleResponsesStream(
	w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, gptProfile *promptCacheProfile, apiKeyID, convID, convSource, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	send := func(eventName string, payload interface{}) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data))
		flusher.Flush()
	}

	createdAt := time.Now().Unix()
	initial := &ResponsesObject{
		ID:                 respID,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              model,
		Output:             []ResponseOutputItem{},
		Usage:              ResponsesUsage{},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
	send("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": initial,
	})

	excluded := make(map[string]bool)
	var lastErr error
	responseStarted := false
	reqStart := time.Now()

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account, affinityReason := h.pool.SelectForConversationWithReason(convID, model, excluded)
		logger.Infof("[Responses] route model=%s source=%s conv=%s reason=%s account=%s attempt=%d",
			model, convSource, responsesConversationLogID(convID), affinityReason, responsesAccountLogID(account), attempt+1)
		if account == nil {
			break
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.failAccount(convID, account, err)
			continue
		}

		send("response.in_progress", map[string]interface{}{
			"type":     "response.in_progress",
			"response": initial,
		})

		gptUsage := h.promptCache.Compute(account.ID, gptProfile)

		var (
			fullText        strings.Builder
			reasoningText   strings.Builder
			toolUses        []KiroToolUse
			inputTokens     int
			outputTokens    int
			credits         float64
			realInputTokens int
			upstreamUsage   KiroTokenUsage
		)

		messageItemID := generateOutputItemID("msg")
		messageStarted := false
		outputIndex := 0
		contentIndex := 0

		ensureMessageStarted := func() {
			if messageStarted {
				return
			}
			messageStarted = true
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      messageItemID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []map[string]interface{}{},
				},
			})
			send("response.content_part.added", map[string]interface{}{
				"type":          "response.content_part.added",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": "",
				},
			})
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					reasoningText.WriteString(text)
					return
				}
				fullText.WriteString(text)
				ensureMessageStarted()
				send("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       messageItemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"delta":         text,
				})
				responseStarted = true
			},
			OnToolUse: func(tu KiroToolUse) {
				if messageStarted {
					send("response.content_part.done", map[string]interface{}{
						"type":          "response.content_part.done",
						"item_id":       messageItemID,
						"output_index":  outputIndex,
						"content_index": contentIndex,
						"part": map[string]interface{}{
							"type": "output_text",
							"text": fullText.String(),
						},
					})
					send("response.output_item.done", map[string]interface{}{
						"type":         "response.output_item.done",
						"output_index": outputIndex,
						"item": map[string]interface{}{
							"id":     messageItemID,
							"type":   "message",
							"role":   "assistant",
							"status": "completed",
							"content": []map[string]interface{}{{
								"type": "output_text",
								"text": fullText.String(),
							}},
						},
					})
					messageStarted = false
					outputIndex++
				}

				toolUses = append(toolUses, tu)
				args, _ := json.Marshal(tu.Input)
				fcID := generateOutputItemID("fc")
				send("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "in_progress",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": "",
					},
				})
				send("response.function_call_arguments.delta", map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"item_id":      fcID,
					"output_index": outputIndex,
					"delta":        string(args),
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "completed",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": string(args),
					},
				})
				outputIndex++
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(h.effectiveContextWindow(model)) / 100.0)
			},
			OnTokenUsage: func(usage KiroTokenUsage) { upstreamUsage = usage },
		}

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			if !responseStarted {
				lastErr = err
				excluded[account.ID] = true
				h.failAccount(convID, account, err)
				continue
			}
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    "server_error",
						"message": err.Error(),
					},
				},
			})
			h.recordFailureWithDetails("responses", model, account.ID, err)
			return
		}

		finalContent, _ := extractThinkingFromContent(fullText.String())
		reasoning := reasoningText.String()
		if !thinking {
			reasoning = ""
		}

		if messageStarted {
			send("response.content_part.done", map[string]interface{}{
				"type":          "response.content_part.done",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": finalContent,
				},
			})
			send("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":     messageItemID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]interface{}{{
						"type": "output_text",
						"text": finalContent,
					}},
				},
			})
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoning, toolUses)
		logger.Infof("[Responses] complete model=%s conv=%s account=%s input=%d output=%d upstream_input=%d uncached=%d cache_read=%d cache_write=%d cache_creation=%d cache_fields_present=%t",
			model, responsesConversationLogID(convID), account.ID, inputTokens, outputTokens, upstreamUsage.InputTokens,
			upstreamUsage.UncachedInputTokens, upstreamUsage.CacheReadInputTokens, upstreamUsage.CacheWriteInputTokens,
			upstreamUsage.CacheCreationInputTokens, upstreamUsage.CacheFieldsPresent)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("responses", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())
		h.promptCache.Update(account.ID, gptProfile)

		respObj := buildResponsesObject(respID, model, finalContent, toolUses, inputTokens, outputTokens, req, gptUsage.CacheReadInputTokens)
		respObj.CreatedAt = createdAt
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions
		h.rememberResponsesAccount(convID, respObj, account.ID)

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		send("response.completed", map[string]interface{}{
			"type":     "response.completed",
			"response": respObj,
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error": map[string]string{
					"type":    "server_error",
					"message": "No available accounts",
				},
			},
		})
		return
	}
	h.recordFailureWithDetails("responses", model, "", lastErr)
	send("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"status": "failed",
			"error": map[string]string{
				"type":    "server_error",
				"message": lastErr.Error(),
			},
		},
	})
}

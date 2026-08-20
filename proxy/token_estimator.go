package proxy

import (
	"encoding/json"
	"math"
)

// estimateApproxTokens is the character-based fallback estimator. It remains
// only as the degradation path when the real BPE tokenizer is unavailable; all
// request-path token counts now go through bpeTokenCount*.
func estimateApproxTokens(text string) int {
	if text == "" {
		return 0
	}

	runes := []rune(text)
	length := len(runes)
	if length == 0 {
		return 0
	}
	if length < 5 {
		return max(1, int(math.Ceil(float64(length)/3.0)))
	}

	var regularAscii, digits, symbols, nonASCII int
	for _, r := range runes {
		switch {
		case r >= 0x80:
			nonASCII++
		case r >= '0' && r <= '9':
			digits++
		case (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~'):
			symbols++
		default:
			regularAscii++
		}
	}

	estimated := int(math.Ceil(
		float64(regularAscii)/4.5 +
			float64(digits)/2.0 +
			float64(symbols)/1.5 +
			float64(nonASCII)/1.5,
	))

	if estimated < 1 {
		return 1
	}
	return estimated
}

// ==================== Claude 侧（cl100k_base） ====================

func estimateClaudeRequestInputTokens(req *ClaudeRequest) int {
	if req == nil {
		return 0
	}

	total := estimateClaudeValueTokens(req.System)

	for _, msg := range req.Messages {
		total += estimateClaudeValueTokens(msg.Content)
	}

	for _, tool := range req.Tools {
		total += bpeTokenCountClaude(tool.Name)
		total += bpeTokenCountClaude(tool.Description)
		total += estimateJSONTokensClaude(tool.InputSchema)
	}

	return total
}

func estimateClaudeOutputTokens(content, thinkingContent string, toolUses []KiroToolUse) int {
	total := bpeTokenCountClaude(content)
	total += bpeTokenCountClaude(thinkingContent)

	for _, tu := range toolUses {
		total += bpeTokenCountClaude(tu.Name)
		total += estimateJSONTokensClaude(tu.Input)
	}

	return total
}

func estimateClaudeValueTokens(v interface{}) int {
	switch value := v.(type) {
	case nil:
		return 0
	case string:
		return bpeTokenCountClaude(value)
	case []interface{}:
		total := 0
		for _, part := range value {
			total += estimateClaudeValueTokens(part)
		}
		return total
	case map[string]interface{}:
		typeName, _ := value["type"].(string)
		switch typeName {
		case "text":
			if text, ok := value["text"].(string); ok {
				return bpeTokenCountClaude(text)
			}
		case "thinking":
			if thinking, ok := value["thinking"].(string); ok {
				return bpeTokenCountClaude(thinking)
			}
		case "tool_use":
			total := 0
			if name, ok := value["name"].(string); ok {
				total += bpeTokenCountClaude(name)
			}
			if input, ok := value["input"]; ok {
				total += estimateJSONTokensClaude(input)
			}
			if total > 0 {
				return total
			}
		case "tool_result":
			if content, ok := value["content"]; ok {
				return estimateClaudeValueTokens(content)
			}
		}

		total := 0
		if text, ok := value["text"].(string); ok {
			total += bpeTokenCountClaude(text)
		}
		if thinking, ok := value["thinking"].(string); ok {
			total += bpeTokenCountClaude(thinking)
		}
		if content, ok := value["content"]; ok {
			total += estimateClaudeValueTokens(content)
		}
		if total > 0 {
			return total
		}

		return estimateJSONTokensClaude(value)
	default:
		return estimateJSONTokensClaude(value)
	}
}

// ==================== OpenAI 侧（o200k_base） ====================

func estimateOpenAIRequestInputTokens(req *OpenAIRequest) int {
	total, _ := estimateOpenAIRequestInputTokensDetailed(req)
	return total
}

// estimateOpenAIRequestInputTokensDetailed is the single-encode entry point for
// GPT requests: it BPE-encodes each message exactly once and returns both the
// grand total and the per-message counts. The cache profile builder reuses the
// per-message counts instead of encoding the input a second time.
func estimateOpenAIRequestInputTokensDetailed(req *OpenAIRequest) (total int, perMsg []int) {
	if req == nil {
		return 0, nil
	}

	perMsg = make([]int, len(req.Messages))
	for i, msg := range req.Messages {
		n := estimateOpenAIMessageTokens(msg)
		perMsg[i] = n
		total += n
	}

	for _, tool := range req.Tools {
		total += bpeTokenCountGPT(tool.Function.Name)
		total += bpeTokenCountGPT(tool.Function.Description)
		total += estimateJSONTokensGPT(tool.Function.Parameters)
	}

	return total, perMsg
}

// estimateOpenAIMessageTokens counts a single OpenAI message (content + tool
// calls + tool_call_id). Shared by the request-level estimator and the cache
// profile builder so a request's input text is BPE-encoded exactly once.
func estimateOpenAIMessageTokens(msg OpenAIMessage) int {
	total := estimateOpenAIContentTokens(msg.Content)
	total += bpeTokenCountGPT(msg.ToolCallID)
	for _, tc := range msg.ToolCalls {
		total += bpeTokenCountGPT(tc.Function.Name)
		total += bpeTokenCountGPT(tc.Function.Arguments)
	}
	return total
}

func estimateOpenAIContentTokens(content interface{}) int {
	switch value := content.(type) {
	case nil:
		return 0
	case string:
		return bpeTokenCountGPT(value)
	default:
		text := extractOpenAIMessageText(value)
		if text != "" {
			return bpeTokenCountGPT(text)
		}
		return estimateJSONTokensGPT(value)
	}
}

func estimateOpenAIOutputTokens(content, reasoningContent string, toolUses []KiroToolUse) int {
	total := bpeTokenCountGPT(content)
	total += bpeTokenCountGPT(reasoningContent)

	for _, tu := range toolUses {
		total += bpeTokenCountGPT(tu.Name)
		total += estimateJSONTokensGPT(tu.Input)
	}

	return total
}

// ==================== JSON 计数（真实 BPE，按协议选编码） ====================

func estimateJSONTokensClaude(v interface{}) int {
	if v == nil {
		return 0
	}

	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}

	return bpeTokenCountClaude(string(b))
}

func estimateJSONTokensGPT(v interface{}) int {
	if v == nil {
		return 0
	}

	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}

	return bpeTokenCountGPT(string(b))
}

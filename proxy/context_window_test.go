package proxy

import "testing"

func TestEffectiveContextWindow(t *testing.T) {
	h := &Handler{
		cachedModels: []ModelInfo{
			{ModelId: "gpt-5.6-luna", TokenLimits: &struct {
				MaxInputTokens  int `json:"maxInputTokens"`
				MaxOutputTokens int `json:"maxOutputTokens"`
			}{MaxInputTokens: 400000}},
			{ModelId: "claude-sonnet-4.6", TokenLimits: &struct {
				MaxInputTokens  int `json:"maxInputTokens"`
				MaxOutputTokens int `json:"maxOutputTokens"`
			}{MaxInputTokens: 1_000_000}},
			{ModelId: "claude-3-5-sonnet", TokenLimits: nil},
		},
	}

	// 精确匹配走动态窗口
	if got := h.effectiveContextWindow("gpt-5.6-luna"); got != 400000 {
		t.Fatalf("expected dynamic window 400000 for gpt-5.6-luna, got %d", got)
	}
	if got := h.effectiveContextWindow("claude-sonnet-4.6"); got != 1_000_000 {
		t.Fatalf("expected dynamic window 1000000 for claude-sonnet-4.6, got %d", got)
	}

	// TokenLimits 为 nil 的模型 → 回退硬编码
	if got := h.effectiveContextWindow("claude-3-5-sonnet"); got != getContextWindowSize("claude-3-5-sonnet") {
		t.Fatalf("expected fallback for model without TokenLimits, got %d", got)
	}

	// 未知模型 → 回退硬编码
	if got := h.effectiveContextWindow("no-such-model"); got != getContextWindowSize("no-such-model") {
		t.Fatalf("expected fallback for unknown model, got %d", got)
	}

	// nil handler → 安全回退
	var nilHandler *Handler
	if got := nilHandler.effectiveContextWindow("gpt-5.6-luna"); got != getContextWindowSize("gpt-5.6-luna") {
		t.Fatalf("expected safe fallback on nil handler, got %d", got)
	}
}

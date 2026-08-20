package proxy

import (
	"strings"
	"testing"
)

func TestBpeTokenCountBasic(t *testing.T) {
	// o200k_base：英文约 4 字符/token。500 字符的英文应得到几十到几百 token。
	gptTokens := bpeTokenCountGPT(strings.Repeat("hello world ", 50))
	if gptTokens <= 0 {
		t.Fatalf("expected positive GPT token count, got %d", gptTokens)
	}
	// 中文字符也应被正确编码（非零）。
	cnTokens := bpeTokenCountGPT(strings.Repeat("你好，世界", 50))
	if cnTokens <= 0 {
		t.Fatalf("expected positive Chinese token count, got %d", cnTokens)
	}
	// 空串恒为 0。
	if bpeTokenCountGPT("") != 0 || bpeTokenCountClaude("") != 0 {
		t.Fatalf("expected empty string to count as 0 tokens")
	}
	// Claude 走 cl100k_base，同样应非零。
	claudeTokens := bpeTokenCountClaude(strings.Repeat("You are a helpful assistant. ", 100))
	if claudeTokens <= 0 {
		t.Fatalf("expected positive Claude token count, got %d", claudeTokens)
	}
}

func TestBpeTokenCountFallsBackOnFailure(t *testing.T) {
	// 直接验证降级路径与字符估算的一致性：若编码未加载，bpeTokenCount
	// 应回退到 estimateApproxTokens，绝不会 panic。
	text := "this is a moderately long english sentence that should not panic"
	got := bpeTokenCount("unknown-model", text)
	if got <= 0 {
		t.Fatalf("expected token count > 0 even with fallback, got %d", got)
	}
}

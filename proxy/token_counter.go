package proxy

import (
	"os"
	"strings"
	"sync"

	"kiro-go/logger"

	"github.com/pkoukk/tiktoken-go"
)

// Real BPE token counting backed by tiktoken-go (the Go port of OpenAI's
// tiktoken). GPT models use o200k_base, Claude models use cl100k_base (the
// closest public encoding to Anthropic's internal tokenizer).
//
// Encodings are loaded lazily on first use and cached process-wide. The BPE
// vocab files are downloaded once from OpenAI on first load and cached on disk
// via TIKTOKEN_CACHE_DIR (set to <dataDir>/bpe). If loading or encoding ever
// fails (offline first run, corrupted cache, ...), we degrade gracefully to the
// character-based estimateApproxTokens so request handling never breaks.
//
// Encoding instances are NOT safe for concurrent use in tiktoken-go, so each
// call takes a read lock.

const (
	bpeEncodingGPT    = "o200k_base"
	bpeEncodingClaude = "cl100k_base"
)

var (
	bpeInitOnce   sync.Once
	bpeLoadWarned = false
	bpeMu         sync.RWMutex
	bpeGPT        *tiktoken.Tiktoken
	bpeClaude     *tiktoken.Tiktoken
	bpeApproxFlag = -1 // -1 未读取, 0 用 BPE, 1 强制字符估算
)

// bpeApproxForced reports whether KIRO_TOKENIZER=approx was set, evaluated once.
func bpeApproxForced() bool {
	if bpeApproxFlag == -1 {
		if os.Getenv("KIRO_TOKENIZER") == "approx" {
			bpeApproxFlag = 1
		} else {
			bpeApproxFlag = 0
		}
	}
	return bpeApproxFlag == 1
}

// ensureBpeLoaded initializes the two encodings exactly once. Failures are
// recorded but non-fatal: callers fall back to estimateApproxTokens.
func ensureBpeLoaded() {
	bpeInitOnce.Do(func() {
		bpeGPT = loadBpeEncoding(bpeEncodingGPT)
		bpeClaude = loadBpeEncoding(bpeEncodingClaude)
	})
}

func loadBpeEncoding(name string) *tiktoken.Tiktoken {
	enc, err := tiktoken.GetEncoding(name)
	if err != nil {
		if !bpeLoadWarned {
			bpeLoadWarned = true
			logger.Warnf("[Tokenizer] failed to load %s, falling back to character estimate: %v", name, err)
		}
		return nil
	}
	return enc
}

// isClaudeModel reports whether the model id should be tokenized with the
// cl100k_base encoding (Anthropic-family models).
func isClaudeModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "claude")
}

// bpeTokenCount returns the real BPE token count of text for the given model.
// On any error it degrades to the character-based approximation. Setting
// KIRO_TOKENIZER=approx forces the character-based estimator for environments
// that must trade token accuracy for CPU (e.g. extremely high concurrency with
// very long contexts).
func bpeTokenCount(model, text string) int {
	if text == "" || bpeApproxForced() {
		return estimateApproxTokens(text)
	}
	ensureBpeLoaded()

	bpeMu.RLock()
	defer bpeMu.RUnlock()

	var enc *tiktoken.Tiktoken
	if isClaudeModel(model) {
		enc = bpeClaude
	} else {
		enc = bpeGPT
	}
	if enc == nil {
		return estimateApproxTokens(text)
	}
	tokens := enc.EncodeOrdinary(text)
	if len(tokens) == 0 {
		return 0
	}
	return len(tokens)
}

// bpeTokenCountGPT counts tokens using the OpenAI o200k_base encoding.
func bpeTokenCountGPT(text string) int {
	return bpeTokenCount("gpt", text)
}

// bpeTokenCountClaude counts tokens using the cl100k_base encoding.
func bpeTokenCountClaude(text string) int {
	return bpeTokenCount("claude", text)
}

package proxy

// OpenAITokenDetails contains the cache components OpenAI-compatible clients
// understand. CachedTokens and CacheWriteTokens are subsets of the top-level
// input/prompt token count.
type OpenAITokenDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

func resolveOpenAICacheUsage(tracker *promptCacheTracker, accountID string, payload *KiroPayload, inputTokens int) *OpenAITokenDetails {
	if tracker == nil {
		return nil
	}
	profile := tracker.BuildOpenAIProfile(payload, inputTokens)
	if profile == nil {
		return nil
	}
	estimated := tracker.Compute(accountID, profile)
	// Store only after a successful upstream response.
	tracker.Update(accountID, profile)
	if estimated.CacheReadInputTokens == 0 && estimated.CacheCreationInputTokens == 0 {
		return nil
	}

	// Scale cache values proportionally to real input.
	// The breakpoints' CumulativeTokens are local estimates, but inputTokens
	// (TotalInputTokens in the profile) is the real value from upstream.
	// Scale so cache values are proportional to the real total.
	cachedTokens := estimated.CacheReadInputTokens
	cacheWriteTokens := estimated.CacheCreationInputTokens
	if profile.TotalInputTokens > 0 && len(profile.Breakpoints) > 0 {
		// Find the max cumulative tokens across breakpoints (local estimate)
		localMax := 0
		for _, bp := range profile.Breakpoints {
			if bp.CumulativeTokens > localMax {
				localMax = bp.CumulativeTokens
			}
		}
		if localMax > 0 && profile.TotalInputTokens > localMax*11/10 {
			// Only scale if real input is significantly larger (>10%)
			scale := float64(profile.TotalInputTokens) / float64(localMax)
			cachedTokens = int(float64(cachedTokens) * scale)
			cacheWriteTokens = int(float64(cacheWriteTokens) * scale)
		}
	}

	return &OpenAITokenDetails{
		CachedTokens:     cachedTokens,
		CacheWriteTokens: cacheWriteTokens,
	}
}

func buildOpenAIUsage(inputTokens, outputTokens int, details *OpenAITokenDetails) map[string]interface{} {
	usage := map[string]interface{}{
		"prompt_tokens":     inputTokens,
		"completion_tokens": outputTokens,
		"total_tokens":      inputTokens + outputTokens,
	}
	if details != nil {
		usage["prompt_tokens_details"] = details
	}
	return usage
}

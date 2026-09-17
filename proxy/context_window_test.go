package proxy

import "testing"

// TestGetContextWindowSize verifies models are classified into the correct
// context window. This drives the input-token count that clients use to decide
// when to compact; misclassifying opus-4.8 or opus-5 (1M) as 200K under-reports
// tokens by 5x and prevents timely compaction.
func TestGetContextWindowSize(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-4.8", 1_000_000},
		{"claude-opus-4-8", 1_000_000},
		{"claude-opus-4.7", 1_000_000},
		{"claude-opus-4.6", 1_000_000},
		{"claude-sonnet-4.6", 1_000_000},
		{"claude-opus-4.8-thinking", 1_000_000},
		{"CLAUDE-OPUS-4.8", 1_000_000},
		{"claude-opus-5", 1_000_000},
		{"claude-opus-5-thinking", 1_000_000},
		{"CLAUDE-OPUS-5", 1_000_000},
		{"claude-opus-5.1", 1_000_000},
		{"claude-opus-5-1", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-haiku-5", 1_000_000},
		{"claude-opus-6", 1_000_000},
		{"claude-opus-4.5", 200_000},
		{"claude-sonnet-4.5", 200_000},
		{"claude-sonnet-4", 200_000},
		{"claude-haiku-4.5", 200_000},
		{"claude-3-5-sonnet", 200_000},
		{"gpt-5.6-sol", 1_000_000},
		{"gpt-5.6", 1_000_000},
		{"GPT-5.6-SOL", 1_000_000},
		{"gpt-4o", 1_000_000},
		{"unknown-model", 200_000},
	}
	for _, c := range cases {
		if got := getContextWindowSize(c.model); got != c.want {
			t.Errorf("getContextWindowSize(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

// TestResolveFinalInputTokens verifies the max-of-three selection: an
// under-counted contextUsagePercentage conversion (e.g. a wrong context
// window guess) must not override a larger local estimate, and vice versa.
func TestResolveFinalInputTokens(t *testing.T) {
	cases := []struct {
		name      string
		real      int
		upstream  int
		estimated int
		want      int
		wantSrc   string
	}{
		{"all zero", 0, 0, 0, 0, "none"},
		{"upstream usage wins", 100, 5000, 300, 5000, "upstreamUsage"},
		{"percentage beats estimate", 8000, 0, 300, 8000, "contextUsagePercentage"},
		{"estimate beats low percentage", 3000, 0, 40000, 40000, "estimated"},
		{"estimate beats both", 3000, 2000, 40000, 40000, "estimated"},
		{"upstream only", 0, 500, 0, 500, "upstreamUsage"},
		{"estimate only", 0, 0, 700, 700, "estimated"},
	}
	for _, c := range cases {
		got, src := resolveFinalInputTokens(c.real, c.upstream, c.estimated)
		if got != c.want || src != c.wantSrc {
			t.Errorf("%s: resolveFinalInputTokens(%d, %d, %d) = (%d, %q), want (%d, %q)",
				c.name, c.real, c.upstream, c.estimated, got, src, c.want, c.wantSrc)
		}
	}
}

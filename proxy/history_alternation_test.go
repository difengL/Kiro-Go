package proxy

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// historyRoleSequence renders history as a U/A string and reports the longest
// run of same-role turns. A healthy conversation alternates, so maxRun == 1.
func historyRoleSequence(history []KiroHistoryMessage) (string, int) {
	var seq strings.Builder
	maxRun, run := 0, 0
	var prev byte
	for _, m := range history {
		c := byte('A')
		if m.UserInputMessage != nil {
			c = 'U'
		}
		seq.WriteByte(c)
		if c == prev {
			run++
		} else {
			run = 1
		}
		if run > maxRun {
			maxRun = run
		}
		prev = c
	}
	return seq.String(), maxRun
}

// buildToolLoopRequest reproduces Claude Code's real wire shape: an agentic loop
// where most assistant turns carry a tool_use and no visible text.
func buildToolLoopRequest(rounds int, textEveryRound, withThinking bool) *ClaudeRequest {
	msgs := []ClaudeMessage{{Role: "user", Content: "investigate the log"}}
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("toolu_%02d", i)
		blocks := []interface{}{}
		if withThinking {
			blocks = append(blocks, map[string]interface{}{
				"type": "thinking", "thinking": "Let me check the actual log data.",
			})
		}
		if textEveryRound || i%6 == 0 {
			blocks = append(blocks, map[string]interface{}{"type": "text", "text": "Checking the log."})
		}
		blocks = append(blocks, map[string]interface{}{
			"type": "tool_use", "id": id, "name": "Bash",
			"input": map[string]interface{}{"command": "tail -n 50 log"},
		})
		msgs = append(msgs, ClaudeMessage{Role: "assistant", Content: blocks})
		msgs = append(msgs, ClaudeMessage{Role: "user", Content: []interface{}{
			map[string]interface{}{
				"type": "tool_result", "tool_use_id": id,
				"content": fmt.Sprintf("result line %d", i),
			},
		}})
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "so which case is it?"})
	return &ClaudeRequest{
		Model:    "claude-opus-4.8",
		System:   "You are Claude Code.",
		Messages: msgs,
		Tools:    []ClaudeTool{{Name: "Bash", InputSchema: map[string]interface{}{"type": "object"}}},
	}
}

// TestHistoryAlternatesInToolLoop guards the premature-stop regression. Assistant
// turns holding only a tool_use (Claude Code's dominant shape) used to be dropped
// once their structured toolUses were stripped, fusing the surrounding user turns
// into runs of up to 6. Upstream read that as "the assistant speaks once and the
// user keeps going" and ended its turn after a single sentence. History must
// alternate regardless of whether assistant turns carry visible text.
func TestHistoryAlternatesInToolLoop(t *testing.T) {
	cases := []struct {
		name           string
		rounds         int
		textEveryRound bool
		withThinking   bool
	}{
		{"tool_use only", 18, false, false},
		{"text every round", 18, true, false},
		{"thinking plus tool_use", 18, false, true},
		{"long session", 60, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := buildToolLoopRequest(tc.rounds, tc.textEveryRound, tc.withThinking)
			history := ClaudeToKiro(req, false).ConversationState.History

			seq, maxRun := historyRoleSequence(history)
			if maxRun > 1 {
				t.Errorf("history has a run of %d same-role turns, want strict alternation\nsequence: %s", maxRun, seq)
			}

			// No turn may be emitted empty: a history of blank assistant turns
			// teaches the model to reply with nothing.
			for i, m := range history {
				if a := m.AssistantResponseMessage; a != nil && strings.TrimSpace(a.Content) == "" && len(a.ToolUses) == 0 {
					t.Errorf("history[%d]: empty assistant turn", i)
				}
			}
		})
	}
}

// TestHistoryNeverSynthesizesAssistantText locks the hard-won constraint on how
// elided tool calls may be repaired: assistant turns must contain only text the
// client actually sent.
//
// Substituting a sentence for the stripped call looks harmless per turn, but it
// repeats once per turn down the whole history, and the model reads that
// repetition as the shape a turn is supposed to have. Measured in production: a
// 75-byte placeholder came back as the model's entire reply — textBytes=75,
// text_len=75, thinking_len=708, stop=end_turn, no tool call. It thought, then
// recited the placeholder. Alternation is repaired by merging the neighbouring
// user turns instead; text in a user turn cannot become an example of what the
// model itself should emit.
func TestHistoryNeverSynthesizesAssistantText(t *testing.T) {
	// Every assistant string the fixture client sends, plus the priming reply
	// ClaudeToKiro prepends. Anything else in an assistant turn is synthesized.
	allowed := map[string]bool{
		"Checking the log.":                 true,
		"Let me check the actual log data.": true, // thinking, kept as fallback body
		"I will follow these instructions.": true, // system priming
	}

	cases := []struct {
		name           string
		rounds         int
		textEveryRound bool
		withThinking   bool
	}{
		{"tool_use only", 12, false, false},
		{"text every round", 12, true, false},
		{"thinking plus tool_use", 12, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := buildToolLoopRequest(tc.rounds, tc.textEveryRound, tc.withThinking)
			history := ClaudeToKiro(req, false).ConversationState.History

			seen := 0
			for i, m := range history {
				a := m.AssistantResponseMessage
				if a == nil {
					continue
				}
				seen++
				if c := strings.TrimSpace(a.Content); !allowed[c] {
					t.Errorf("history[%d]: assistant turn carries text the client never sent: %q", i, c)
				}
			}
			if seen == 0 {
				t.Fatal("no assistant turns in history")
			}
		})
	}
}

// TestToolResultsStayAttributed checks the tool call remains visible after its
// structured form is stripped: the result side must still name the tool that
// produced each output, since that is now the only record a call happened.
func TestToolResultsStayAttributed(t *testing.T) {
	req := buildToolLoopRequest(6, false, false)
	history := ClaudeToKiro(req, false).ConversationState.History

	attributed := 0
	for _, m := range history {
		if u := m.UserInputMessage; u != nil && strings.Contains(u.Content, "[Bash]") {
			attributed++
		}
	}
	if attributed == 0 {
		t.Error("no tool result names the tool that produced it")
	}
}

// TestThinkingBackfillsEmptyAssistantTurn covers the thinking+tool_use shape:
// thinking text is the only body such a turn has, so it may serve as the fallback
// content — but the extractor must report the substitution, because a body
// borrowed from reasoning is only safe to keep while its tool call is attached.
func TestThinkingBackfillsEmptyAssistantTurn(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "thinking", "thinking": "I should read the log first."},
		map[string]interface{}{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]interface{}{}},
	}
	text, toolUses, fromThinking := extractClaudeAssistantContent(content)
	if len(toolUses) != 1 {
		t.Fatalf("expected 1 tool use, got %d", len(toolUses))
	}
	if !strings.Contains(text, "read the log") {
		t.Errorf("thinking text should back an otherwise-empty assistant turn, got %q", text)
	}
	if !fromThinking {
		t.Error("a body taken from reasoning must be reported as such")
	}

	// A turn with real client text keeps it, and reports no substitution.
	withText := []interface{}{
		map[string]interface{}{"type": "thinking", "thinking": "I should read the log first."},
		map[string]interface{}{"type": "text", "text": "Reading the log."},
	}
	text, _, fromThinking = extractClaudeAssistantContent(withText)
	if text != "Reading the log." {
		t.Errorf("client text must win over reasoning, got %q", text)
	}
	if fromThinking {
		t.Error("client-visible text must not be reported as borrowed from reasoning")
	}
}

// TestOrphanedReasoningIsDropped locks the fix for the second measured stop.
//
// Claude Code's dominant assistant shape is thinking + tool_use with no visible
// text, so the reasoning block is the turn's only body. Reasoning is first-person
// planning prose — "Now I need to update the comment... Let me check the test
// file" — which works as a preamble only while the call it introduces is attached.
// Upstream rejects structured toolUses on every history turn but the active one,
// so those calls are stripped, and the narration is left behind as a complete
// assistant turn that announces an intention and does nothing.
//
// Repeated down a long tool loop, that becomes what the model believes an
// assistant turn is. Measured: msgs=76 history=50 curLen=12 ("继续执行") returned
// text_len=362 thinking_len=0 stop=end_turn — a paragraph of plans, no tool call,
// and the reasoning channel never entered, because in-context the plan *was* the
// answer shape.
func TestOrphanedReasoningIsDropped(t *testing.T) {
	const reasoning = "Let me check the actual log data."

	t.Run("stripped when the call is gone", func(t *testing.T) {
		req := buildToolLoopRequest(12, false, true)
		history := ClaudeToKiro(req, false).ConversationState.History

		for i, m := range history {
			a := m.AssistantResponseMessage
			if a == nil {
				continue
			}
			if strings.Contains(a.Content, reasoning) && len(a.ToolUses) == 0 {
				t.Errorf("history[%d]: reasoning narration survived without its tool call: %q", i, a.Content)
			}
		}
	})

	// The active tool turn is the one place the pairing is honest: the call is
	// still structured there, so the narration reads as a preamble to it.
	t.Run("kept on the active tool turn", func(t *testing.T) {
		req := buildToolLoopRequest(4, false, true)
		msgs := req.Messages
		// End on tool results so the last assistant turn becomes active.
		last := msgs[len(msgs)-2]
		req.Messages = append(msgs[:len(msgs)-1], last, ClaudeMessage{
			Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type": "tool_result", "tool_use_id": "toolu_04", "content": "fresh output",
				},
			},
		})
		req.Messages[len(req.Messages)-2] = ClaudeMessage{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "thinking", "thinking": reasoning},
			map[string]interface{}{
				"type": "tool_use", "id": "toolu_04", "name": "Bash",
				"input": map[string]interface{}{"command": "tail log"},
			},
		}}

		history := ClaudeToKiro(req, false).ConversationState.History
		if len(history) == 0 {
			t.Fatal("no history")
		}
		active := history[len(history)-1].AssistantResponseMessage
		if active == nil || len(active.ToolUses) == 0 {
			t.Fatal("expected the last history turn to be the active tool turn")
		}
		if !strings.Contains(active.Content, reasoning) {
			t.Errorf("active tool turn should keep its reasoning preamble, got %q", active.Content)
		}
	})
}

// TestElideMiddleKeepsBothEnds verifies oversized tool output keeps its head and
// tail: the tail carries the summary, error, or exit status that a head-only cut
// would discard. The result must stay valid UTF-8 even when cuts land mid-rune.
func TestElideMiddleKeepsBothEnds(t *testing.T) {
	body := "HEAD_MARKER" + strings.Repeat("x", 6000) + "TAIL_MARKER"
	out := elideMiddle(body, maxToolResultContinuationBytes)

	if len(out) > maxToolResultContinuationBytes {
		t.Errorf("elided output is %d bytes, over the %d limit", len(out), maxToolResultContinuationBytes)
	}
	if !strings.Contains(out, "HEAD_MARKER") {
		t.Error("head of tool output was lost")
	}
	if !strings.Contains(out, "TAIL_MARKER") {
		t.Error("tail of tool output was lost")
	}

	// Multi-byte runes must not be split by either cut point.
	wide := strings.Repeat("日志分析", 2000)
	if got := elideMiddle(wide, maxToolResultContinuationBytes); !utf8.ValidString(got) {
		t.Error("elided output is not valid UTF-8")
	}
}

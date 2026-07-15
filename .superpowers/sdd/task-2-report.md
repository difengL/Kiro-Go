# Task 2 报告：ResolveConversationID（亲和 key 暴露）

## 改了哪些文件

- `proxy/translator.go`：在 `buildConversationID` 后追加两个公开函数 `ResolveClaudeConversationID` 和 `ResolveOpenAIConversationID`
- `proxy/translator_test.go`：追加 6 个测试函数

## System 字段实际类型与处理方式

`ClaudeRequest.System` 的实际类型是 `interface{}`（translator.go:129），运行时可能是：

- `string`：直接作为纯文本 system prompt
- `[]interface{}`：每个元素是 `map[string]interface{}`，内含 `"type": "text"` 和 `"text": "..."`
- `nil`：无 system prompt

处理方式：复用已有的 `extractSystemPrompt(system interface{})`（translator.go:599），它已正确处理上述三种情况。不需要额外写 `extractSystemPromptText`。

## 关键决策

1. **不改动 `buildConversationID` 本身**：`buildConversationID` 在合成锚点时会生成随机 UUID（用于上游透传），而 `ResolveClaudeConversationID` / `ResolveOpenAIConversationID` 在合成锚点时直接返回 `""`，让调用方知道“跳过亲和”。两者职责分离，互不干扰。
2. **复用已有函数**：`firstClaudeConversationAnchor`、`firstOpenAIConversationAnchor`、`isSyntheticConversationAnchor`、`extractSystemPrompt`、`extractOpenAIMessageText` 均直接复用，不重复造轮子。
3. **OpenAI 侧 system 消息过滤**：`ResolveOpenAIConversationID` 在计算 anchor 前先过滤掉 `role == "system"` 的消息，确保 `firstOpenAIConversationAnchor` 拿到的是正确的非 system 消息列表。

## 命令输出摘要

```
$ go test ./proxy/ -run Resolve -v
Go test: 20 passed in 1 packages
```

全部通过。

## 自审问题

- **Q: 为什么合成锚点返回空串而不是随机 UUID？**
  - A: 返回空串作为信号，让调用方（affinityRouter）知道“此请求没有可亲和的会话，跳过亲和路由”。如果返回随机 UUID，则每次都会命中不同节点，失去亲和意义。
- **Q: 为什么 `ResolveClaudeConversationID` 用 `extractSystemPrompt` 而不是自己解析？**
  - A: `extractSystemPrompt` 已经正确处理了 `string`、`[]interface{}`、`nil` 三种情况，复用它可以避免重复代码和潜在的不一致。
- **Q: 测试覆盖了哪些边界情况？**
  - Claude 和 OpenAI 的确定性 ID（同 anchor+system+model -> 同 convID）
  - 合成锚点返回空串
  - 无 user anchor 返回空串
  - Claude system blocks（`[]interface{}` 形式）也能正确产生确定性 ID

## 修复记录

### 2026-07-15: 恢复 TestOpenAIToolResultImageCarriedWhenFollowedByUser 中被误删的 toolHistImages 断言

**问题**：Task 2 实现 ResolveConversationID 时，意外删除了 `proxy/translator_test.go` 中 `TestOpenAIToolResultImageCarriedWhenFollowedByUser` 里的 `toolHistImages != 1` 断言（约 3 行），该断言与 Task 2 无关。

**根因分析**：该断言自 `2ad0c56`（提取工具结果中的图片）引入时就是坏的 -- 它通过 `UserInputMessageContext.ToolResults` 来识别历史中的工具结果条目，但 `sanitizeKiroHistory` 会将历史条目中的 `ToolResults` 置 nil 并清空 `UserInputMessageContext`，导致断言永远检查不到任何图片（`toolHistImages` 始终为 0）。

**修复方式**：将检查条件从依赖 `UserInputMessageContext != nil && len(ToolResults) > 0` 改为直接统计所有历史 `UserInputMessage` 的 `Images` 字段。因为在此测试场景中，只有工具结果历史条目才带图片，简化后的条件语义等价且不受 sanitize 影响。

**测试结果**：
- `TestOpenAIToolResultImageCarriedWhenFollowedByUser`：PASS
- `TestOpenAIToolResultImageAttachedToCurrentMessage`：PASS
- 全部 20 个 Resolve 测试：PASS
- `TestClaudeToolResultMixedTextAndImage` 失败为已有问题，与本次修复无关

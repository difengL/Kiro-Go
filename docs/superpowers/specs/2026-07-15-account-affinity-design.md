# 账号亲和性（Session Affinity）模式设计

日期：2026-07-15
状态：已批准，待实现

## 背景与动机

当前账号池采用**加权轮询（Weighted Round-Robin）**，完全无状态：每次请求独立选号，与历史请求无关。核心在 `pool/account.go:189` 的 `GetNextForModelExcluding`，游标 `currentIndex` 用 `atomic.AddUint64(&p.currentIndex, 1) % n` 在加权展开后的扁平账号切片里取下一个槽位。

代理已有一套 prompt cache 追踪（`proxy/cache_tracker.go`），按 `accountID` 分区（`entriesByAccount`），TTL 默认 5 分钟。但它是**纯报告层**——只在响应里写回 `CacheReadInputTokens` / `CacheCreationInputTokens`，并不影响下一个请求选哪个账号。因此同一会话的第二次请求一旦被轮询到别的账号，前一个账号刚建的缓存就白费，新账号报 0 read、全 creation。

会话标识也已存在但未用于选号：`proxy/translator.go:1818` 的 `buildConversationID(modelID, systemPrompt, anchor)` 对"同 model + 同 system + 同首条 user 消息"生成**确定性 UUID**（`uuid.NewSHA1`）。同一会话后续请求带不同长度历史，但首条 user message 相同 → ConversationID 相同。这是天然的亲和 key。

本设计把这两个已有能力串起来：用 ConversationID 做亲和 key，绑定到首次命中的账号，后续同 key 请求优先回路由到同一账号 → 命中该账号已建的 prompt cache → `CacheReadInputTokens` 真正生效。

## 需求决策

- **亲和 key 来源**：复用确定性 ConversationID。客户端零侵入，不要求改请求头。
- **亲和账号不可用时**：自动迁移到新号并更新映射。
- **启用范围**：开关可控，默认开启。可一键关闭回退到纯加权轮询。
- **亲和 TTL**：5 分钟（对齐 prompt cache 最小 TTL），可配置。

## 实现方案

方案 A：Pool 层集中式亲和路由器。在 `pool.AccountPool` 内新增独立子模块 `affinityRouter`，对外暴露 `SelectForConversation` / `Remember`。handler 把"先查亲和 → 命中且可用就返回 → 否则走加权轮询 → 成功后回写映射"统一委托给 Pool 的方法。

选 A 的理由：亲和性的核心难点是"亲和账号当前是否可用"的判定，而这套判定（冷却、token 过期、配额、模型支持、excluded）已完整存在于 `GetNextForModelExcluding`。把亲和路由器做成 Pool 内部子模块能直接复用这套判定，保证一致性，避免在四个 handler（Claude 流式/非流式、OpenAI 流式/非流式）各写一遍。account.go 偏大的问题，通过把 affinityRouter 拆到同包下独立文件 `pool/affinity.go` 解决。

## 设计详节

### 第 1 节：亲和 key 的获取与提前暴露

当前 `buildConversationID` 在 `ClaudeToKiro` / `OpenAIToKiro` 转换内部生成，handler 选号时（`handler.go:890`）拿不到它；且合成锚点会返回随机 UUID，无法做亲和。

设计：

1. 抽取纯函数 `ResolveConversationID(req)`，在 `translator.go` 暴露。输入为已解析的 ClaudeRequest / OpenAIRequest，内部复用 `firstClaudeConversationAnchor` / `firstOpenAIConversationAnchor` + system prompt + model 调 `buildConversationID`。handler 在选号前调用它拿到 convID。

2. **合成锚点处理**：当 `isSyntheticConversationAnchor` 命中（占位符 "."、"begin conversation" 等），当前 `buildConversationID` 返回随机 UUID，无法做亲和。**注意**：`buildConversationID` 当前还被 `ClaudeToKiro` / `OpenAIToKiro` 内部用来生成发给上游 Kiro 的 `ConversationState.ConversationID`，**该透传行为必须保持不变**（上游仍需随机 UUID）。因此 `ResolveConversationID` 不修改 `buildConversationID` 本身，而是在其返回值上做一层判断：若 `isSyntheticConversationAnchor(anchor)` 则 `ResolveConversationID` 返回**空字符串**（亲和 key 用），而 `ClaudeToKiro` 继续拿原随机 UUID（上游透传用）。两者解耦。Pool 的亲和路由器看到空 key 直接跳过亲和、走加权轮询——行为退化为今天的样子，零副作用。

3. **Responses API（`/v1/responses`）**：有 `previous_response_id`，但那是 OpenAI 侧概念。为保持一致，也用 `buildConversationID`（model + system + 首条 user）做 key，不引入 `previous_response_id` 作为亲和 key，避免和 Claude/OpenAI 两路语义不一致。

安全阀：空 key = 跳过亲和。

### 第 2 节：affinityRouter 数据结构与并发安全

位置：新建 `pool/affinity.go`，作为 `AccountPool` 的内部子模块，不另起包。

```go
type affinityEntry struct {
    accountID string
    lastUsed  time.Time
}

type affinityRouter struct {
    mu       sync.RWMutex
    bindings map[string]affinityEntry  // convID → entry
    ttl      time.Duration
}
```

并发模型：

- 复用 Pool 既有并发范式：读多写少用 `sync.RWMutex`，与 `cooldowns` / `errorCounts` 一致。
- 亲和映射的读（查绑定）和写（回写/迁移/清理）都经过 `affinityRouter.mu`，与 Pool 的 `p.mu` 是**两把独立锁**——绝不在持有一把时嵌套拿另一把，避免死锁。亲和路由器只通过 Pool 暴露的查询方法（`GetByID`、可用性判定）与 Pool 交互，不直接碰 Pool 内部字段。

TTL 与清理：

- TTL 默认 5 分钟，可配置。
- 查亲和时做**惰性过期**：`now - lastUsed > ttl` 视为失效，当作未绑定走轮询，并顺手删除该条，避免映射表只增不减。
- 额外加**后台清理**：复用已有 `backgroundRefresh` 定时器周期性扫描删除过期条目，防止"只查从不写"的死会话残留。

容量边界：无硬上限，依赖 TTL 清理。未来如需要再加 LRU 上限，YAGNI 暂不做。

自检：两把锁不嵌套；TTL 双保险（惰性 + 后台）；空 key 永不入表。

### 第 3 节：选号决策流程（亲和 → 迁移 → 轮询）

新增 Pool 方法作为 handler 统一入口：

```go
// SelectForConversation 选择账号：先亲和命中，否则加权轮询。
// convID 为空时退化为加权轮询（兼容合成锚点）。
// 成功选中后由 handler 回调 Remember 绑定。
func (p *AccountPool) SelectForConversation(convID, model string, excluded map[string]bool) *config.Account
```

流程：

```
SelectForConversation(convID, model, excluded):
  1. 若 convID == "" → 直接 GetNextForModelExcluding(model, excluded)，返回
  2. 查亲和: entry, ok := affinity.lookup(convID)
     - !ok 或已过期 → 跳到步骤 4（首次绑定）
  3. 命中且有绑定:
     a. acc := GetByID(entry.accountID)
     b. 若 acc != nil 且 isAccountUsable(acc, model, excluded, now) → 返回 acc（亲和命中，cache 命中窗口成立）
     c. 否则（账号已不可用）→ 跳到步骤 4（自动迁移）
  4. 走加权轮询: acc := GetNextForModelExcluding(model, excluded)
     - acc == nil → 返回 nil（沿用现有 fallback 语义，handler 进入下一轮重试或报错）
     - 否则返回 acc（此时尚未绑定；绑定推迟到请求成功后，见第 4 节）
```

**可用性判定 `isAccountUsable`**：把 `GetNextForModelExcluding` 内部那套判定（模型支持、冷却、token 过期、配额、excluded）抽成内部方法 `isAccountUsable(acc, model, excluded, now) bool`，亲和命中分支和轮询分支都调它。这是方案 A 一致性的关键——亲和与轮询共用同一个"可用"定义，不会出现亲和放行但池认为不可用的撕裂。

**亲和命中但不可用 = 迁移信号**：步骤 3c 不报错，直接落步骤 4 选新号。选号阶段还不写映射——因为新号可能请求失败要进重试。绑定时机见第 4 节。

**空 key 安全阀**：步骤 1 保证合成锚点、无 system+首句的请求完全不受影响，行为等同今天。

自检：亲和与轮询共用 `isAccountUsable`；迁移不报错；绑定不在选号阶段做。

### 第 4 节：绑定时机、迁移写入与失败处理

**绑定时机——成功后才回写**。选号阶段不写映射。绑定在请求实际成功之后由 handler 调用：

```go
// Remember 在请求成功后绑定 convID → accountID，刷新 lastUsed。
func (p *AccountPool) Remember(convID, accountID string)
```

- `convID == ""` 时直接返回，不入表（安全阀）。
- 写入 `bindings[convID] = {accountID, now}`，覆盖旧绑定（迁移场景天然成立：旧号失败 → 新号成功 → 覆盖为新号）。

**handler 接入点**（以 Claude 流式为例，其余三个对称）：

```
handleClaudeStream (proxy/handler.go:847):
  convID := translator.ResolveConversationID(req)   // 新增，选号前
  for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
      account := h.pool.SelectForConversation(convID, model, excluded)  // 替换原 GetNextForModelExcluding
      ensureValidToken(account)
      promptCache.Compute(account.ID, cacheProfile)
      err := CallKiroAPI(...)
      if err == nil {
          h.pool.Remember(convID, account.ID)   // 新增：成功后才绑定
          RecordSuccess / promptCache.Update / 返回 SSE
          return
      }
      // 失败：excluded[account.ID]=true; handleAccountFailure; continue
  }
```

**为什么绑定在 `if err == nil` 分支**：失败重试时若在选号就绑定，会把失败账号写进映射，下个同会话请求又粘到这个刚失败的号。成功才绑定保证映射里只记"刚跑通且已建 cache 的号"。

**迁移写入是隐式的**：迁移场景下，新号成功 → `Remember` 用新 accountID 覆盖旧条目。无需专门的"删除旧绑定"调用——覆盖即迁移。

**亲和命中同号的刷新**：亲和命中、成功后仍调 `Remember`，刷新 `lastUsed` 续命 TTL，保证活跃会话不被清。

**失败处理（已有，不动）**：`excluded[account.ID]=true` + `handleAccountFailure` 完全沿用，冷却/熔断/降级逻辑（`account_failover.go`）零改动。亲和路由器不碰失败处理，只负责选号和绑定。

自检：绑定只在成功分支；迁移靠覆盖；失败链路零改动；空 key 不绑定。

### 第 5 节：配置开关与默认值

在 `config/config.go` 的配置结构里新增两个字段，沿用现有 JSON 持久化 + admin 面板可改范式：

```go
AffinityEnabled    bool          `json:"affinityEnabled"`    // 默认 true
AffinityTTLMinutes int           `json:"affinityTTLMinutes"` // 默认 5
```

默认值与降级：

- `AffinityEnabled = true`（默认开）。关掉后 `SelectForConversation` 直接走 `GetNextForModelExcluding`，`Remember` 直接 return——亲和路由器完全旁路，行为等同今天。
- `AffinityTTLMinutes` 默认 5，范围校验 `[1, 60]`，越界回退到 5。
- 运行时改：沿用现有 admin 配置更新路径，改完触发 `pool.Reload()` 时一并重建 affinityRouter 的 TTL。已绑定的条目按新 TTL 在下次惰性检查时自然过期，不强行清空——避免改配置瞬间抖动。

配置读取：新增 `config.GetAffinityEnabled()` / `config.GetAffinityTTLMinutes()`，和 `GetAllowOverUsage` 等现有 getter 风格一致。

Admin 面板：`web/` 前端配置页加这两个开关 + TTL 输入，复用现有配置表单的提交链路。

自检：开关关 = 完全旁路（零行为差异）；TTL 越界回退；改配置不强行清映射。

### 第 6 节：测试策略

1. **affinityRouter 单元测试**（`pool/affinity_test.go`，纯结构，不依赖 Pool）：
   - 空 key lookup → 返回未命中，不写表
   - 绑定后 lookup 命中同 accountID
   - TTL 过期后 lookup → 返回未命中 + 条目被惰性删除
   - 后台清理删除过期条目、保留活跃条目
   - `Remember` 覆盖旧绑定（迁移语义）

2. **SelectForConversation 集成测试**（`pool/account_test.go`，构造多账号 Pool）：
   - 空 key → 走轮询，行为 = `GetNextForModelExcluding`
   - 首次请求无绑定 → 轮询选号，选号阶段未写映射
   - 成功 `Remember` 后，同 convID 再次请求 → 命中同一账号
   - 亲和账号置为冷却 → 再次请求 → 迁移到另一可用号，且新号成功后映射被覆盖
   - 亲和账号被 excluded → 迁移（与冷却对称）
   - `AffinityEnabled=false` → 亲和完全旁路，两次同 convID 请求可能命中不同账号
   - 并发：N goroutine 同 convID 并发请求，断言亲和命中后无数据竞争（`go test -race`）

3. **Handler 层验证**（`proxy/handler_test.go`，若已有测试基建则补，否则手测脚本）：
   - 关键断言：成功响应后 `bindings` 表里出现该 convID；失败重试不写映射。
   - 用 mock CallKiroAPI 注入失败/成功。

4. **端到端手测脚本**（不入 CI，放 `docs/`）：用同一首条 user message 连发 3 次请求，断言第 2、3 次的 `cache_read_input_tokens > 0` 且落到同一账号（看 admin 面板 RequestCount 增量集中在一个号上）。

成功标准：

- `go test ./pool/... ./proxy/... -race` 全绿。
- 亲和开启时，同会话连续请求的 `CacheReadInputTokens` 从第 2 次起 > 0（今天为 0）。
- 亲和关闭时，行为与主分支一致（回归基线）。

自检：迁移、空 key、并发 race、开关旁路都有显式用例。

## 切入点清单

- `proxy/translator.go`：新增 `ResolveConversationID`；**不修改** `buildConversationID` 本身（其随机 UUID 仍用于上游 Kiro 透传），仅在 `ResolveConversationID` 出口对合成锚点返回空串作为亲和 key。
- `pool/affinity.go`：新文件，`affinityRouter` 结构 + lookup/remember/cleanup。
- `pool/account.go`：`AccountPool` 持有 `affinity *affinityRouter`；新增 `SelectForConversation` / `Remember`；抽出 `isAccountUsable` 内部方法；`Reload` 时重建 affinity TTL；`GetByID`（若不存在则补）。
- `proxy/handler.go`：四个 handler（Claude 流式/非流式、OpenAI 流式/非流式）选号改 `SelectForConversation`，成功分支加 `Remember`。
- `config/config.go`：新增两配置字段 + getter + 范围校验。
- `web/`：配置页加开关 + TTL 输入。
- `pool/affinity_test.go` / `pool/account_test.go` / `proxy/handler_test.go`：测试。

# 账号亲和性（Session Affinity）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在加权轮询之上增加"会话亲和性"模式：同一 ConversationID 的后续请求优先路由到首次命中的账号，以命中该账号已建的 prompt cache，提高 `CacheReadInputTokens`。

**Architecture:** Pool 层集中式亲和路由器。在 `pool.AccountPool` 内新增 `affinityRouter` 子模块（独立文件 `pool/affinity.go`），对外暴露 `SelectForConversation` / `Remember`。handler 在选号前用 `translator.ResolveConversationID` 取亲和 key，调 `SelectForConversation` 选号，请求成功后调 `Remember` 绑定。亲和与轮询共用 `isAccountUsable` 判定。

**Tech Stack:** Go 1.21，纯 `net/http`，配置 JSON 持久化，`sync.RWMutex` 并发，`github.com/google/uuid`。

## Global Constraints

- 所有新代码遵循现有并发范式：读多写少用 `sync.RWMutex`，`atomic` 用于游标。亲和映射锁与 Pool 主锁是两把独立锁，绝不嵌套持有。
- `buildConversationID` 的上游 Kiro 透传行为不可改动（`translator.go:309`、`:1251` 仍需原值）。亲和 key 仅经新增的 `ResolveConversationID` 出口。
- 合成锚点亲和 key 返回空串，空 key 在亲和路由器里直接旁路走轮询，不入映射表。
- 默认 `AffinityEnabled=true`、`AffinityTTLMinutes=5`，TTL 范围 `[1,60]` 越界回退 5。
- 失败/冷却/熔断链路（`proxy/account_failover.go`、`pool/account.go` 的 `RecordError`/冷却）零改动。亲和路由器只负责选号与绑定。
- 每个 task 以 TDD 顺序：先写失败测试 → 跑测试见失败 → 最小实现 → 跑测试见通过 → 提交。频繁提交。
- 提交信息后缀 `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`。
- 工作目录 `D:\project\Kiro-Go`，shell 为 bash（Git Bash），用 `go test ./...` 跑测试。

## File Structure

- **`proxy/translator.go`**（修改）：新增 `ResolveConversationID`（Claude + OpenAI 两个变体或一个接口分发）。不改 `buildConversationID`。
- **`pool/affinity.go`**（新建）：`affinityRouter` 结构、`lookup` / `remember` / `cleanup` 方法，独立锁，TTL。
- **`pool/affinity_test.go`**（新建）：affinityRouter 纯结构单元测试。
- **`pool/account.go`**（修改）：`AccountPool` 持有 `affinity *affinityRouter`；抽出 `isAccountUsable`；新增 `SelectForConversation` / `Remember`；`Reload` 重建 affinity TTL；构造时初始化 affinity。
- **`pool/account_test.go`**（新建或追加）：`SelectForConversation` 集成测试。
- **`config/config.go`**（修改）：新增 `AffinityEnabled` / `AffinityTTLMinutes` 字段 + `GetAffinityEnabled` / `GetAffinityTTLMinutes` / `UpdateAffinity*` + 范围校验。
- **`config/config_test.go`**（新建或追加）：配置默认值与范围校验测试。
- **`proxy/handler.go`**（修改）：四个 handler（`:890` Claude 流式、`:1450` Claude 非流式、`:1637` OpenAI 流式、`:2014` OpenAI 非流式）选号改 `SelectForConversation`，成功分支加 `Remember`。需把 convID 透传进这些函数。
- **`proxy/handler_test.go`**（新建或追加，若基建允许）：成功后绑定、失败重试不绑定的断言。
- **`web/`**（修改）：配置页加亲和开关 + TTL 输入，复用现有配置表单提交链路。
- **`docs/account-affinity-manual-test.md`**（新建）：端到端手测脚本。

---

### Task 1: 配置字段与 getter（AffinityEnabled / AffinityTTLMinutes）

**Files:**
- Modify: `config/config.go:172-176`（在 `AllowOverUsage` 附近加字段）、`config/config.go:822-840`（仿 `GetAllowOverUsage`/`UpdateAllowOverUsage` 加 getter/setter）
- Test: `config/config_test.go`（新建或追加）

**Interfaces:**
- Produces: `GetAffinityEnabled() bool`、`GetAffinityTTLMinutes() int`、`UpdateAffinitySettings(enabled bool, ttlMinutes int) error`。默认 `enabled=true`、`ttl=5`。TTL 范围 `[1,60]`，越界回退 5。

- [ ] **Step 1: 写失败测试**

新建 `config/config_test.go`（若已存在则追加）：

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func withTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// 临时切换 configPath 指向测试目录
	old := configDir
	configDir = dir
	t.Cleanup(func() { configDir = old })
	return dir
}

func TestAffinityDefaults(t *testing.T) {
	withTempConfigDir(t)
	cfg := &AppConfig{}
	applyAffinityDefaults(cfg)
	if !cfg.AffinityEnabled {
		t.Fatalf("AffinityEnabled default want true")
	}
	if cfg.AffinityTTLMinutes != 5 {
		t.Fatalf("AffinityTTLMinutes default want 5, got %d", cfg.AffinityTTLMinutes)
	}
}

func TestGetAffinityTTLMinutesClamps(t *testing.T) {
	withTempConfigDir(t)
	globalCfg = &AppConfig{AffinityEnabled: true, AffinityTTLMinutes: 0}
	if got := GetAffinityTTLMinutes(); got != 5 {
		t.Fatalf("ttl 0 -> want 5, got %d", got)
	}
	globalCfg.AffinityTTLMinutes = 120
	if got := GetAffinityTTLMinutes(); got != 5 {
		t.Fatalf("ttl 120 -> want 5, got %d", got)
	}
	globalCfg.AffinityTTLMinutes = 30
	if got := GetAffinityTTLMinutes(); got != 30 {
		t.Fatalf("ttl 30 -> want 30, got %d", got)
	}
}

func TestGetAffinityEnabledDefaultTrue(t *testing.T) {
	withTempConfigDir(t)
	globalCfg = nil // 触发默认
	if !GetAffinityEnabled() {
		t.Fatalf("GetAffinityEnabled nil config want true")
	}
}
```

> 注：若 `config` 包内部变量名不是 `globalCfg` / `configDir`，按实际名调整（查 `config/config.go` 顶部）。测试目的是验证默认值与 clamp，变量名以现有为准。

- [ ] **Step 2: 跑测试见失败**

Run: `go test ./config/ -run Affinity -v`
Expected: FAIL — `applyAffinityDefaults` undefined / 字段不存在。

- [ ] **Step 3: 加配置字段与默认值**

在 `config/config.go` 的 `AllowOverUsage` 字段附近加：

```go
	// AffinityEnabled enables session affinity: same ConversationID routes to the
	// same account to hit prompt cache. Default true.
	AffinityEnabled bool `json:"affinityEnabled,omitempty"`
	// AffinityTTLMinutes is how long a conversation→account binding stays alive
	// without activity. Clamped to [1,60], default 5.
	AffinityTTLMinutes int `json:"affinityTTLMinutes,omitempty"`
```

新增默认值与 getter（仿 `GetAllowOverUsage`）：

```go
// applyAffinityDefaults fills affinity defaults for zero-value fields.
func applyAffinityDefaults(cfg *AppConfig) {
	if !cfg.AffinityEnabled && cfg.AffinityTTLMinutes == 0 {
		// 未显式配置：开 + 5 分钟。注意 false 也可能是显式关闭，
		// 但 JSON omitempty 下未配置时两字段同时为零值，此时按默认开处理。
		cfg.AffinityEnabled = true
		cfg.AffinityTTLMinutes = 5
	}
}

// GetAffinityEnabled returns whether session affinity is enabled. Defaults to true.
func GetAffinityEnabled() bool {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	if cfg == nil || !cfg.AffinityEnabled && cfg.AffinityTTLMinutes == 0 {
		return true // nil 或未配置默认开
	}
	return cfg.AffinityEnabled
}

// GetAffinityTTLMinutes returns the affinity TTL in minutes, clamped to [1,60]. Default 5.
func GetAffinityTTLMinutes() int {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	if cfg == nil {
		return 5
	}
	v := cfg.AffinityTTLMinutes
	if v < 1 || v > 60 {
		return 5
	}
	return v
}

// UpdateAffinitySettings updates affinity settings and persists the change.
func UpdateAffinitySettings(enabled bool, ttlMinutes int) error {
	if ttlMinutes < 1 || ttlMinutes > 60 {
		ttlMinutes = 5
	}
	cfgMu.Lock()
	cfg.AffinityEnabled = enabled
	cfg.AffinityTTLMinutes = ttlMinutes
	cfgMu.Unlock()
	return saveConfig()
}
```

> 调整锁名/变量名与现有 `cfg`、`cfgMu`、`saveConfig` 一致。

- [ ] **Step 4: 跑测试见通过**

Run: `go test ./config/ -run Affinity -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add config/config.go config/config_test.go
git commit -m "$(cat <<'EOF'
feat(config): add affinityEnabled and affinityTTLMinutes settings

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: ResolveConversationID（亲和 key 暴露）

**Files:**
- Modify: `proxy/translator.go`（在 `buildConversationID` 附近，约 `:1825` 后加新函数）
- Test: `proxy/translator_test.go`（新建或追加）

**Interfaces:**
- Consumes: `buildConversationID(modelID, systemPrompt, anchor string)`（`translator.go:1818`）、`firstClaudeConversationAnchor`（`:1787`）、`firstOpenAIConversationAnchor`（`:1804`）、`isSyntheticConversationAnchor`（`:1827`）。
- Produces:
  - `ResolveClaudeConversationID(req *ClaudeRequest) string`
  - `ResolveOpenAIConversationID(req *OpenAIRequest) string`
  - 返回空串当 anchor 为合成锚点；否则返回确定性 UUID。

- [ ] **Step 1: 写失败测试**

新建/追加 `proxy/translator_test.go`：

```go
package main

import (
	"strings"
	"testing"
)

func TestResolveClaudeConversationID_Deterministic(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4",
		System: "You are a helpful assistant.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Hello, what is 1+1?"},
			{Role: "assistant", Content: "2"},
			{Role: "user", Content: "And 2+2?"},
		},
		MaxTokens: 100,
	}
	id1 := ResolveClaudeConversationID(req)
	if id1 == "" {
		t.Fatal("want non-empty convID for real anchor")
	}
	// 第二次请求：历史更长但首条 user message 不变
	req2 := &ClaudeRequest{
		Model: "claude-sonnet-4",
		System: "You are a helpful assistant.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Hello, what is 1+1?"},
			{Role: "assistant", Content: "2"},
			{Role: "user", Content: "And 2+2?"},
			{Role: "assistant", Content: "4"},
			{Role: "user", Content: "And 3+3?"},
		},
		MaxTokens: 100,
	}
	id2 := ResolveClaudeConversationID(req2)
	if id1 != id2 {
		t.Fatalf("same anchor+system+model should produce same convID: %q vs %q", id1, id2)
	}
}

func TestResolveClaudeConversationID_SyntheticAnchorEmpty(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "."},
		},
		MaxTokens: 100,
	}
	if id := ResolveClaudeConversationID(req); id != "" {
		t.Fatalf("synthetic anchor should yield empty convID, got %q", id)
	}
}

func TestResolveClaudeConversationID_NoAnchorEmpty(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4",
		Messages: []ClaudeMessage{{Role: "assistant", Content: "hi"}},
		MaxTokens: 100,
	}
	if id := ResolveClaudeConversationID(req); id != "" {
		t.Fatalf("no user anchor should yield empty convID, got %q", id)
	}
}

func TestResolveOpenAIConversationID_Deterministic(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-4",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Hello, what is 1+1?"},
			{Role: "assistant", Content: "2"},
		},
	}
	id1 := ResolveOpenAIConversationID(req)
	if !strings.Contains(id1, "-") {
		t.Fatalf("want uuid-like convID, got %q", id1)
	}
}
```

- [ ] **Step 2: 跑测试见失败**

Run: `go test ./proxy/ -run Resolve -v`
Expected: FAIL — `ResolveClaudeConversationID` undefined。

- [ ] **Step 3: 写实现**

在 `proxy/translator.go` 的 `buildConversationID` 后追加：

```go
// extractSystemPromptText 把 Claude system 字段（可能是 string 或 []ClaudeSystemBlock）归一化为纯文本。
func extractSystemPromptText(s interface{}) string {
	switch v := s.(type) {
	case string:
		return v
	case []interface{}:
		var b strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// ResolveClaudeConversationID returns a deterministic conversation key for
// affinity routing. Returns "" for synthetic/absent anchors (skip affinity).
// Does NOT change buildConversationID's upstream-facing behavior.
func ResolveClaudeConversationID(req *ClaudeRequest) string {
	if req == nil {
		return ""
	}
	anchor := firstClaudeConversationAnchor(req.Messages)
	if isSyntheticConversationAnchor(anchor) {
		return ""
	}
	return buildConversationID(req.Model, extractSystemPromptText(req.System), anchor)
}

// ResolveOpenAIConversationID returns a deterministic conversation key for
// affinity routing. Returns "" for synthetic/absent anchors.
func ResolveOpenAIConversationID(req *OpenAIRequest) string {
	if req == nil {
		return ""
	}
	var nonSystem []OpenAIMessage
	var systemText string
	for _, m := range req.Messages {
		if m.Role == "system" {
			systemText += extractOpenAIMessageText(m.Content)
			continue
		}
		nonSystem = append(nonSystem, m)
	}
	anchor := firstOpenAIConversationAnchor(nonSystem)
	if isSyntheticConversationAnchor(anchor) {
		return ""
	}
	return buildConversationID(req.Model, systemText, anchor)
}
```

> `extractOpenAIMessageText` 已存在于 translator.go，直接复用。若 `ClaudeRequest.System` 在现有代码里已用别的类型（如 `[]ClaudeSystemBlock`），按实际类型调整 `extractSystemPromptText`。

- [ ] **Step 4: 跑测试见通过**

Run: `go test ./proxy/ -run Resolve -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add proxy/translator.go proxy/translator_test.go
git commit -m "$(cat <<'EOF'
feat(proxy): expose ResolveConversationID for affinity routing

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: affinityRouter 数据结构与单元测试

**Files:**
- Create: `pool/affinity.go`
- Test: `pool/affinity_test.go`

**Interfaces:**
- Produces（内部，pool 包内可见）:
  - `type affinityRouter struct { mu sync.RWMutex; bindings map[string]affinityEntry; ttl time.Duration }`
  - `type affinityEntry struct { accountID string; lastUsed time.Time }`
  - `func newAffinityRouter(ttl time.Duration) *affinityRouter`
  - `func (a *affinityRouter) lookup(convID string, now time.Time) (string, bool)` — 返回 accountID；过期则删除并返回 false；空 key 返回 false 不查。
  - `func (a *affinityRouter) remember(convID, accountID string, now time.Time)` — 空 key 直接 return；否则写/覆盖 + 刷新 lastUsed。
  - `func (a *affinityRouter) cleanup(now time.Time)` — 删除所有过期条目。
  - `func (a *affinityRouter) setTTL(ttl time.Duration)` — 运行时改 TTL。

- [ ] **Step 1: 写失败测试**

新建 `pool/affinity_test.go`：

```go
package pool

import (
	"testing"
	"time"
)

func TestAffinity_LookupMissOnEmpty(t *testing.T) {
	a := newAffinityRouter(5 * time.Minute)
	now := time.Now()
	if id, ok := a.lookup("", now); ok || id != "" {
		t.Fatalf("empty key lookup should miss, got id=%q ok=%v", id, ok)
	}
	if id, ok := a.lookup("unknown", now); ok || id != "" {
		t.Fatalf("unknown key should miss, got id=%q ok=%v", id, ok)
	}
}

func TestAffinity_RememberAndLookup(t *testing.T) {
	a := newAffinityRouter(5 * time.Minute)
	now := time.Now()
	a.remember("conv1", "acc1", now)
	if id, ok := a.lookup("conv1", now); !ok || id != "acc1" {
		t.Fatalf("want acc1 hit, got id=%q ok=%v", id, ok)
	}
}

func TestAffinity_RememberOverwritesMigration(t *testing.T) {
	a := newAffinityRouter(5 * time.Minute)
	now := time.Now()
	a.remember("conv1", "acc1", now)
	a.remember("conv1", "acc2", now.Add(time.Second))
	if id, ok := a.lookup("conv1", now.Add(2*time.Second)); !ok || id != "acc2" {
		t.Fatalf("migration: want acc2, got id=%q ok=%v", id, ok)
	}
}

func TestAffinity_LookupExpiresAfterTTL(t *testing.T) {
	a := newAffinityRouter(5 * time.Minute)
	t0 := time.Now()
	a.remember("conv1", "acc1", t0)
	// 6 分钟后应过期
	if id, ok := a.lookup("conv1", t0.Add(6*time.Minute)); ok || id != "" {
		t.Fatalf("expired should miss, got id=%q ok=%v", id, ok)
	}
	// 过期条目应被惰性删除
	a.mu.RLock()
	_, present := a.bindings["conv1"]
	a.mu.RUnlock()
	if present {
		t.Fatalf("expired entry should be lazily deleted")
	}
}

func TestAffinity_RememberNoOpOnEmptyKey(t *testing.T) {
	a := newAffinityRouter(5 * time.Minute)
	a.remember("", "acc1", time.Now())
	a.mu.RLock()
	n := len(a.bindings)
	a.mu.RUnlock()
	if n != 0 {
		t.Fatalf("empty key should not be stored, got len=%d", n)
	}
}

func TestAffinity_CleanupDropsExpiredKeepsActive(t *testing.T) {
	a := newAffinityRouter(5 * time.Minute)
	t0 := time.Now()
	a.remember("expired", "acc1", t0)
	a.remember("active", "acc2", t0.Add(6*time.Minute))
	a.cleanup(t0.Add(6 * time.Minute)) // expired 过期，active 仍在窗口
	a.mu.RLock()
	_, expPresent := a.bindings["expired"]
	_, actPresent := a.bindings["active"]
	a.mu.RUnlock()
	if expPresent {
		t.Fatal("cleanup should drop expired")
	}
	if !actPresent {
		t.Fatal("cleanup should keep active")
	}
}

func TestAffinity_LookupRefreshDoesNotResetTTL(t *testing.T) {
	// lookup 只读，不刷新 lastUsed；只有 remember 刷新。
	a := newAffinityRouter(5 * time.Minute)
	t0 := time.Now()
	a.remember("conv1", "acc1", t0)
	// 多次 lookup 不刷新
	for i := 0; i < 3; i++ {
		a.lookup("conv1", t0.Add(time.Duration(i)*time.Minute))
	}
	if id, ok := a.lookup("conv1", t0.Add(6*time.Minute)); ok || id != "" {
		t.Fatalf("lookup should not refresh TTL; expected expiry, got id=%q ok=%v", id, ok)
	}
}
```

- [ ] **Step 2: 跑测试见失败**

Run: `go test ./pool/ -run Affinity -v`
Expected: FAIL — `newAffinityRouter` undefined。

- [ ] **Step 3: 写实现**

新建 `pool/affinity.go`：

```go
package pool

import (
	"sync"
	"time"
)

// affinityEntry 会话→账号的亲和绑定。lastUsed 用于 TTL 判定。
type affinityEntry struct {
	accountID string
	lastUsed  time.Time
}

// affinityRouter 是 AccountPool 的内部子模块，维护 convID→accountID 的亲和映射。
// 用独立的 sync.RWMutex，绝不与 AccountPool.mu 嵌套持有。
type affinityRouter struct {
	mu       sync.RWMutex
	bindings map[string]affinityEntry
	ttl      time.Duration
}

func newAffinityRouter(ttl time.Duration) *affinityRouter {
	return &affinityRouter{
		bindings: make(map[string]affinityEntry),
		ttl:      ttl,
	}
}

// lookup 返回 convID 绑定的 accountID。空 key 或未绑定或过期则返回 ("", false)。
// 过期条目被惰性删除。lookup 不刷新 lastUsed。
func (a *affinityRouter) lookup(convID string, now time.Time) (string, bool) {
	if convID == "" {
		return "", false
	}
	a.mu.Lock() // 写锁：可能删除过期条目
	defer a.mu.Unlock()
	e, ok := a.bindings[convID]
	if !ok {
		return "", false
	}
	if now.Sub(e.lastUsed) > a.ttl {
		delete(a.bindings, convID)
		return "", false
	}
	return e.accountID, true
}

// remember 绑定 convID→accountID 并刷新 lastUsed。空 key 直接 return。
// 覆盖旧绑定（迁移语义）：旧号失败→新号成功→覆盖为新号。
func (a *affinityRouter) remember(convID, accountID string, now time.Time) {
	if convID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bindings[convID] = affinityEntry{accountID: accountID, lastUsed: now}
}

// cleanup 删除所有过期条目。由后台定时器调用。
func (a *affinityRouter) cleanup(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, e := range a.bindings {
		if now.Sub(e.lastUsed) > a.ttl {
			delete(a.bindings, id)
		}
	}
}

// setTTL 运行时更新 TTL。已绑定的条目按新 TTL 在下次 lookup/cleanup 时自然过期。
func (a *affinityRouter) setTTL(ttl time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ttl = ttl
}
```

- [ ] **Step 4: 跑测试见通过**

Run: `go test ./pool/ -run Affinity -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add pool/affinity.go pool/affinity_test.go
git commit -m "$(cat <<'EOF'
feat(pool): add affinityRouter for session→account bindings

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: AccountPool 持有 affinity，抽出 isAccountUsable

**Files:**
- Modify: `pool/account.go:16-24`（结构体加字段）、`:32-42`（构造初始化）、`:49-66`（Reload 重建 TTL）、`:189-256`（抽出 isAccountUsable）
- Test: `pool/account_test.go`

**Interfaces:**
- Consumes: Task 1 的 `config.GetAffinityEnabled` / `GetAffinityTTLMinutes`，Task 3 的 `affinityRouter`。
- Produces:
  - `func (p *AccountPool) isAccountUsable(acc *config.Account, model string, excluded map[string]bool, now time.Time) bool`（包内可见，需 Pool.mu.RLock 持有时调用）。
  - `AccountPool` 结构新增 `affinity *affinityRouter` 字段。

- [ ] **Step 1: 写失败测试**

新建/追加 `pool/account_test.go`：

```go
package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// newTestPool 用临时配置构造一个池，便于测试。
func newTestPool(t *testing.T, accounts []config.Account) *AccountPool {
	t.Helper()
	dir := t.TempDir()
	// 通过 config 包加载到临时目录；若 config 没有公开的测试注入入口，
	// 直接手工构造 AccountPool（绕过单例）。
	p := &AccountPool{
		accounts:   accounts,
		cooldowns:  make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists: make(map[string]map[string]bool),
		affinity:   newAffinityRouter(5 * time.Minute),
	}
	p.totalAccounts = len(accounts)
	return p
}

func TestIsAccountUsable_HappyPath(t *testing.T) {
	acc := config.Account{ID: "a1", Enabled: true, ExpiresAt: time.Now().Add(1 * time.Hour).Unix()}
	p := newTestPool(t, []config.Account{acc})
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.isAccountUsable(&acc, "claude-sonnet-4", nil, time.Now()) {
		t.Fatal("fresh enabled account should be usable")
	}
}

func TestIsAccountUsable_Excluded(t *testing.T) {
	acc := config.Account{ID: "a1", Enabled: true, ExpiresAt: time.Now().Add(1 * time.Hour).Unix()}
	p := newTestPool(t, []config.Account{acc})
	p.mu.RLock()
	defer p.mu.RUnlock()
	excluded := map[string]bool{"a1": true}
	if p.isAccountUsable(&acc, "claude-sonnet-4", excluded, time.Now()) {
		t.Fatal("excluded account should not be usable")
	}
}

func TestIsAccountUsable_Cooldown(t *testing.T) {
	acc := config.Account{ID: "a1", Enabled: true, ExpiresAt: time.Now().Add(1 * time.Hour).Unix()}
	p := newTestPool(t, []config.Account{acc})
	p.cooldowns["a1"] = time.Now().Add(10 * time.Minute)
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.isAccountUsable(&acc, "claude-sonnet-4", nil, time.Now()) {
		t.Fatal("cooled-down account should not be usable")
	}
}

func TestIsAccountUsable_TokenExpiringSoon(t *testing.T) {
	acc := config.Account{ID: "a1", Enabled: true, ExpiresAt: time.Now().Add(60 * time.Second).Unix()}
	p := newTestPool(t, []config.Account{acc})
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.isAccountUsable(&acc, "claude-sonnet-4", nil, time.Now()) {
		t.Fatal("account expiring within 120s should not be usable")
	}
}
```

> 注：若 `config.Account` 字段名/类型不同（如 ExpiresAt 类型），按实际调整。测试目的明确即可。

- [ ] **Step 2: 跑测试见失败**

Run: `go test ./pool/ -run IsAccountUsable -v`
Expected: FAIL — `isAccountUsable` undefined。

- [ ] **Step 3: 修改结构体与构造**

`pool/account.go:16-24` 加字段：

```go
type AccountPool struct {
	mu            sync.RWMutex
	accounts      []config.Account
	totalAccounts int
	currentIndex  uint64
	cooldowns     map[string]time.Time
	errorCounts   map[string]int
	modelLists    map[string]map[string]bool
	affinity      *affinityRouter // 会话亲和映射（独立锁，不与 mu 嵌套）
}
```

`pool/account.go:32-42` 构造里初始化：

```go
		poolOnce.Do(func() {
			pool = &AccountPool{
				cooldowns:   make(map[string]time.Time),
				errorCounts: make(map[string]int),
				modelLists:  make(map[string]map[string]bool),
				affinity:    newAffinityRouter(time.Duration(config.GetAffinityTTLMinutes()) * time.Minute),
			}
			pool.Reload()
		})
```

`pool/account.go:49-66` 的 `Reload` 末尾加 TTL 重建：

```go
	p.accounts = weighted
	p.totalAccounts = len(enabled)
	if p.affinity != nil {
		p.affinity.setTTL(time.Duration(config.GetAffinityTTLMinutes()) * time.Minute)
	}
```

- [ ] **Step 4: 抽出 isAccountUsable**

在 `pool/account.go` 的 `GetNextForModelExcluding` 上方加：

```go
// isAccountUsable 判断账号当前是否可用于选号。
// 必须在持有 p.mu.RLock 时调用。亲和命中分支与轮询分支共用此判定。
func (p *AccountPool) isAccountUsable(acc *config.Account, model string, excluded map[string]bool, now time.Time) bool {
	if acc == nil {
		return false
	}
	if excluded != nil && excluded[acc.ID] {
		return false
	}
	if !p.accountHasModel(acc.ID, model) {
		return false
	}
	if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
		return false
	}
	if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
		return false
	}
	if isQuotaBlocked(*acc, config.GetAllowOverUsage()) {
		return false
	}
	return true
}
```

把 `GetNextForModelExcluding` 主循环里的内联判定替换为 `if !p.isAccountUsable(acc, model, excluded, now) { seen[acc.ID] = true; continue }`（保留 atomic 游标推进与 seen 去重）。fallback 分支也可改用 `isAccountUsable`，但 fallback 语义略不同（挑冷却最短），保留原逻辑不动以免引入回归——仅主循环复用。

- [ ] **Step 5: 跑测试见通过**

Run: `go test ./pool/ -run IsAccountUsable -v && go test ./pool/... -race`
Expected: PASS，race 无报警。原有 pool 测试不回归。

- [ ] **Step 6: 提交**

```bash
git add pool/account.go pool/account_test.go
git commit -m "$(cat <<'EOF'
refactor(pool): extract isAccountUsable, hold affinityRouter

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: SelectForConversation 与 Remember

**Files:**
- Modify: `pool/account.go`（新增方法）
- Test: `pool/account_test.go`

**Interfaces:**
- Consumes: Task 4 的 `isAccountUsable`、`GetByID`（已存在 `:259`）、`GetNextForModelExcluding`、Task 1 的 `config.GetAffinityEnabled`。
- Produces:
  - `func (p *AccountPool) SelectForConversation(convID, model string, excluded map[string]bool) *config.Account`
  - `func (p *AccountPool) Remember(convID, accountID string)`

- [ ] **Step 1: 写失败测试**

追加到 `pool/account_test.go`：

```go
func TestSelectForConversation_EmptyKeyDegradesToRoundRobin(t *testing.T) {
	a1 := config.Account{ID: "a1", Enabled: true, ExpiresAt: futureExpiry()}
	a2 := config.Account{ID: "a2", Enabled: true, ExpiresAt: futureExpiry()}
	p := newTestPool(t, []config.Account{a1, a2})
	acc := p.SelectForConversation("", "claude-sonnet-4", nil)
	if acc == nil {
		t.Fatal("empty key should still return an account via round-robin")
	}
}

func TestSelectForConversation_AffinityHitReturnsSameAccount(t *testing.T) {
	a1 := config.Account{ID: "a1", Enabled: true, ExpiresAt: futureExpiry()}
	a2 := config.Account{ID: "a2", Enabled: true, ExpiresAt: futureExpiry()}
	p := newTestPool(t, []config.Account{a1, a2})
	first := p.SelectForConversation("conv1", "claude-sonnet-4", nil)
	if first == nil {
		t.Fatal("first select nil")
	}
	p.Remember("conv1", first.ID)
	second := p.SelectForConversation("conv1", "claude-sonnet-4", nil)
	if second == nil || second.ID != first.ID {
		t.Fatalf("affinity hit: want %q, got %q", first.ID, second.IDOrEmpty())
	}
}

func TestSelectForConversation_MigratesWhenBoundAccountCooledDown(t *testing.T) {
	a1 := config.Account{ID: "a1", Enabled: true, ExpiresAt: futureExpiry()}
	a2 := config.Account{ID: "a2", Enabled: true, ExpiresAt: futureExpiry()}
	p := newTestPool(t, []config.Account{a1, a2})
	p.Remember("conv1", "a1")
	p.cooldowns["a1"] = time.Now().Add(10 * time.Minute) // a1 冷却
	acc := p.SelectForConversation("conv1", "claude-sonnet-4", nil)
	if acc == nil {
		t.Fatal("should migrate")
	}
	if acc.ID == "a1" {
		t.Fatal("should not return cooled-down a1")
	}
}

func TestSelectForConversation_MigratesWhenExcluded(t *testing.T) {
	a1 := config.Account{ID: "a1", Enabled: true, ExpiresAt: futureExpiry()}
	a2 := config.Account{ID: "a2", Enabled: true, ExpiresAt: futureExpiry()}
	p := newTestPool(t, []config.Account{a1, a2})
	p.Remember("conv1", "a1")
	acc := p.SelectForConversation("conv1", "claude-sonnet-4", map[string]bool{"a1": true})
	if acc == nil || acc.ID == "a1" {
		t.Fatalf("should migrate off excluded a1, got %v", acc)
	}
}

func TestRemember_NoOpOnEmptyKey(t *testing.T) {
	p := newTestPool(t, nil)
	p.Remember("", "a1")
	if id, ok := p.affinity.lookup("", time.Now()); ok || id != "" {
		t.Fatal("empty key must not bind")
	}
}

// helper
func futureExpiry() int64 { return time.Now().Add(1 * time.Hour).Unix() }
```

> 若 `*config.Account` 缺 `IDOrEmpty` 之类方法，直接用 `acc.ID`。上面的 `second.IDOrEmpty()` 改为 `second.ID`。

- [ ] **Step 2: 跑测试见失败**

Run: `go test ./pool/ -run SelectForConversation -v`
Expected: FAIL — 方法 undefined。

- [ ] **Step 3: 写实现**

在 `pool/account.go` 加：

```go
// SelectForConversation 选择账号：先亲和命中，否则加权轮询。
// convID 为空时退化为 GetNextForModelExcluding（兼容合成锚点）。
// 亲和命中但账号不可用时，自动迁移到轮询选出的新号。
// 绑定由 handler 在请求成功后调 Remember 完成。
func (p *AccountPool) SelectForConversation(convID, model string, excluded map[string]bool) *config.Account {
	// 亲和未启用或空 key：直接轮询
	if !config.GetAffinityEnabled() || convID == "" {
		return p.GetNextForModelExcluding(model, excluded)
	}

	now := time.Now()

	// 1. 查亲和（affinity 独立锁，不持 p.mu）
	if boundID, ok := p.affinity.lookup(convID, now); ok {
		// 2. 命中：校验该账号当前可用（需持 p.mu.RLock）
		p.mu.RLock()
		acc := p.getAccountByIDLocked(boundID)
		usable := acc != nil && p.isAccountUsable(acc, model, excluded, now)
		p.mu.RUnlock()
		if usable {
			return acc // 亲和命中，cache 窗口成立
		}
		// 不可用：迁移，落 to 轮询
	}
	// 3. 轮询
	return p.GetNextForModelExcluding(model, excluded)
}

// Remember 在请求成功后绑定 convID→accountID，刷新 lastUsed。
// convID 为空时直接返回（安全阀）。
func (p *AccountPool) Remember(convID, accountID string) {
	if !config.GetAffinityEnabled() || convID == "" {
		return
	}
	p.affinity.remember(convID, accountID, time.Now())
}

// getAccountByIDLocked 在已持有 p.mu.RLock 时按 ID 查账号。
func (p *AccountPool) getAccountByIDLocked(id string) *config.Account {
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return &p.accounts[i]
		}
	}
	return nil
}
```

> 不复用现有 `GetByID`（`:259`），因为它自带 RLock/RUnlock，会和 `isAccountUsable` 要求的已持锁调用冲突。`getAccountByIDLocked` 是不带锁的内部版。

- [ ] **Step 4: 跑测试见通过**

Run: `go test ./pool/ -run "SelectForConversation|Remember" -v && go test ./pool/... -race`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add pool/account.go pool/account_test.go
git commit -m "$(cat <<'EOF'
feat(pool): add SelectForConversation and Remember for affinity

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: 后台清理亲和映射

**Files:**
- Modify: `pool/account.go`（找到现有 `backgroundRefresh` 或等价定时器）

**Interfaces:**
- Consumes: Task 3 的 `affinityRouter.cleanup`。

- [ ] **Step 1: 定位后台定时器**

Run: `grep -n "backgroundRefresh\|ticker\|time.NewTicker" pool/account.go`
找到现有的后台刷新 goroutine。若不存在，在 `GetPool` 的 `poolOnce.Do` 里启动一个。

- [ ] **Step 2: 写失败测试**

追加到 `pool/affinity_test.go`：

```go
func TestAffinity_CleanupRemovesExpiredOnly(t *testing.T) {
	a := newAffinityRouter(1 * time.Minute)
	t0 := time.Now()
	a.remember("old", "acc1", t0.Add(-2*time.Minute)) // 已过期
	a.remember("new", "acc2", t0)                     // 活跃
	a.cleanup(t0)
	a.mu.RLock()
	_, oldPresent := a.bindings["old"]
	_, newPresent := a.bindings["new"]
	a.mu.RUnlock()
	if oldPresent {
		t.Fatal("cleanup should remove expired")
	}
	if !newPresent {
		t.Fatal("cleanup should keep active")
	}
}
```

- [ ] **Step 3: 跑测试见通过**（实现已在 Task 3 完成，此步验证 cleanup 在 pool 集成下可用）

Run: `go test ./pool/ -run Cleanup -v`
Expected: PASS。

- [ ] **Step 4: 接入后台定时器**

在 `backgroundRefresh`（或新定时器）的 tick 回调里加：

```go
// 清理过期亲和映射
func (p *AccountPool) backgroundCleanupAffinity() {
	if p.affinity == nil {
		return
	}
	p.affinity.cleanup(time.Now())
}
```

并在每分钟 tick 调用 `pool.backgroundCleanupAffinity()`。若现有定时器周期 > 1 分钟，在其 tick 内附带调用即可（TTL 至少 1 分钟，1 分钟清理一次足够）。

- [ ] **Step 5: 跑全量测试**

Run: `go test ./pool/... -race`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add pool/account.go pool/affinity_test.go
git commit -m "$(cat <<'EOF'
feat(pool): wire affinity cleanup into background refresh

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: handler 接入亲和选号（Claude 流式）

**Files:**
- Modify: `proxy/handler.go:802`（`handleClaudeMessagesInternal` 解析 req 后取 convID 并透传）、`:847`（`handleClaudeStream` 签名加 convID）、`:890`（选号改 `SelectForConversation`）、成功分支加 `Remember`

**Interfaces:**
- Consumes: Task 2 的 `ResolveClaudeConversationID`，Task 5 的 `SelectForConversation` / `Remember`。
- Produces: `handleClaudeStream` 多一个 `convID string` 参数。

- [ ] **Step 1: 写失败测试**

若 `proxy/handler_test.go` 存在且有用 mock 注入 CallKiroAPI 的基建，追加：

```go
func TestClaudeStream_BindsConversationOnSuccess(t *testing.T) {
	// 用两个可用账号；mock CallKiroAPI 成功。
	// 断言：成功后 pool.affinity.bindings 含该 convID。
	t.Skip("requires mock CallKiroAPI infrastructure; if absent, rely on manual test Task 11")
}
```

> 若无 mock 基建，标记 Skip 并依赖 Task 11 手测。写测试骨架仍保留意图。

- [ ] **Step 2: 跑测试见失败/跳过**

Run: `go test ./proxy/ -run BindsConversation -v`
Expected: SKIP 或 FAIL（基建缺失）。无回归即继续。

- [ ] **Step 3: 改 handleClaudeStream 签名与选号**

`proxy/handler.go:847` 签名加 `convID string`：

```go
func (h *Handler) handleClaudeStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, convID string) {
```

`:890` 选号改：

```go
		account := h.pool.SelectForConversation(convID, model, excluded)
```

在成功分支（`CallKiroAPI` 返回 nil err 之后、return 之前）加：

```go
		h.pool.Remember(convID, account.ID)
```

- [ ] **Step 4: 更新调用方传 convID**

`proxy/handler.go:802` 的 `handleClaudeMessagesInternal` 在 `ClaudeToKiro` 之后、调 `handleClaudeStream` 之前加：

```go
	convID := ResolveClaudeConversationID(&req)
```

并把 `convID` 作为最后参数传给 `handleClaudeStream(...)`。同理 `handleClaudeNonStream` 调用处也传 convID（见 Task 8）。

- [ ] **Step 5: 构建并跑测试**

Run: `go build ./... && go test ./proxy/...`
Expected: 构建通过，测试无回归。

- [ ] **Step 6: 提交**

```bash
git add proxy/handler.go proxy/handler_test.go
git commit -m "$(cat <<'EOF'
feat(proxy): route Claude stream via affinity, remember on success

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: handler 接入亲和选号（Claude 非流式 + OpenAI 流式/非流式）

**Files:**
- Modify: `proxy/handler.go:1444`（`handleClaudeNonStream`）、`:1450`、`:1578`（`handleOpenAIChat` 取 convID）、`:1617`（`handleOpenAIStream`）、`:1637`、`:2008`（`handleOpenAINonStream`）、`:2014`

**Interfaces:**
- Consumes: Task 2 的 `ResolveClaudeConversationID` / `ResolveOpenAIConversationID`，Task 5 的 `SelectForConversation` / `Remember`。

- [ ] **Step 1: 写失败测试（同 Task 7，Skip 占位）**

```go
func TestOpenAIStream_BindsConversationOnSuccess(t *testing.T) {
	t.Skip("requires mock CallKiroAPI; rely on manual test Task 11")
}
```

- [ ] **Step 2: 跑测试见跳过**

Run: `go test ./proxy/ -run BindsConversation -v`
Expected: SKIP。

- [ ] **Step 3: Claude 非流式**

`handleClaudeNonStream` 签名加 `convID string`，`:1450` 选号改 `SelectForConversation(convID, model, excluded)`，成功分支加 `Remember`。调用方（`handleClaudeMessagesInternal` 里非流式分支）传 Task 7 已算出的 `convID`。

- [ ] **Step 4: OpenAI 流式**

`handleOpenAIChat`（`:1578` 附近）解析 `OpenAIRequest` 后取 `convID := ResolveOpenAIConversationID(&req)`，透传给 `handleOpenAIStream`。`handleOpenAIStream`（`:1617`）签名加 `convID`，`:1637` 选号改 `SelectForConversation`，成功分支加 `Remember`。

- [ ] **Step 5: OpenAI 非流式**

`handleOpenAINonStream`（`:2008`）签名加 `convID`，`:2014` 选号改 `SelectForConversation`，成功分支加 `Remember`。调用方传 convID。

- [ ] **Step 6: 构建并跑全量测试**

Run: `go build ./... && go test ./... -race`
Expected: 构建通过，全量测试 PASS，race 无报警。

- [ ] **Step 7: 提交**

```bash
git add proxy/handler.go proxy/handler_test.go
git commit -m "$(cat <<'EOF'
feat(proxy): route Claude non-stream and OpenAI handlers via affinity

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: admin 配置 API 暴露亲和设置

**Files:**
- Modify: `proxy/handler.go` admin API（找现有 `AllowOverUsage` 的 admin 端点，仿照加 `/admin/config/affinity`）

**Interfaces:**
- Consumes: Task 1 的 `config.GetAffinityEnabled` / `GetAffinityTTLMinutes` / `UpdateAffinitySettings`。
- Produces: `GET /admin/config/affinity`、`PUT/POST /admin/config/affinity`。

- [ ] **Step 1: 定位现有 admin 端点**

Run: `grep -n "AllowOverUsage\|allowOverUsage\|/admin/config" proxy/handler.go`

- [ ] **Step 2: 写失败测试（占位，若有 admin 测试基建）**

```go
func TestAdminAffinity_GetAndPut(t *testing.T) {
	t.Skip("requires admin API test harness; manual verify Task 11")
}
```

- [ ] **Step 3: 加端点**

仿现有 over-usage 端点，加：

```go
// GET 返回 {affinityEnabled, affinityTTLMinutes}
// PUT/POST 接受同结构，调 config.UpdateAffinitySettings，再 pool.Reload()
```

在 PUT 处理成功后调 `pool.GetPool().Reload()`（让新 TTL 生效）。

- [ ] **Step 4: 构建并跑测试**

Run: `go build ./... && go test ./...`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add proxy/handler.go
git commit -m "$(cat <<'EOF'
feat(admin): expose affinity settings via admin API

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: 前端配置页加亲和开关与 TTL 输入

**Files:**
- Modify: `web/` 下配置页 HTML/JS（找现有 allowOverUsage 复选框附近）

- [ ] **Step 1: 定位前端配置表单**

Run: `grep -rn "allowOverUsage" web/`

- [ ] **Step 2: 加 UI**

在 allowOverUsage 复选框旁加：
- 复选框 `affinityEnabled`（label: 会话亲和性）
- 数字输入 `affinityTTLMinutes`（label: 亲和 TTL（分钟，1-60））

提交逻辑复用现有配置保存函数，把两个字段加入 PUT `/admin/config/affinity` 的 body。

- [ ] **Step 3: 构建并启动手测**

Run: `go build ./... && ./kiro-go`（或项目实际启动命令）
手动在 admin 面板切换开关、改 TTL，确认保存后刷新页面值保留。

- [ ] **Step 4: 提交**

```bash
git add web/
git commit -m "$(cat <<'EOF'
feat(web): add affinity toggle and TTL input to config page

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: 端到端手测脚本与文档

**Files:**
- Create: `docs/account-affinity-manual-test.md`

- [ ] **Step 1: 写手测脚本**

```markdown
# 账号亲和性 端到端手测

## 前置
- 至少 2 个可用账号（admin 面板可见 RequestCount）
- affinityEnabled=true, affinityTTLMinutes=5

## 步骤
1. 用同一首条 user message 连发 3 次 /v1/messages（messages 数组逐次增长，但首条 user 不变）。
   例：首条 user = "Explain prompt caching."
2. 每次响应里检查 usage.cache_read_input_tokens：
   - 第 1 次：0（或 cache_creation > 0）
   - 第 2、3 次：> 0（命中同账号缓存）
3. admin 面板看 RequestCount：3 次请求集中在同一个账号。

## 关闭回归
1. affinityEnabled=false。
2. 同样连发 3 次。预期：可能分散到不同账号，cache_read 可能 0（回归基线）。

## 迁移验证
1. 亲和绑定到 a1（连发 1 次确认）。
2. 在 admin 面板把 a1 临时禁用（或等冷却）。
3. 再发同会话请求。预期：路由到 a2，且成功后 a1 的绑定被覆盖为 a2。
```

- [ ] **Step 2: 跑手测并记录结果**

按脚本执行，记录实际 cache_read_input_tokens 数值与账号分布。若与预期不符，回到对应 task 修。

- [ ] **Step 3: 提交**

```bash
git add docs/account-affinity-manual-test.md
git commit -m "$(cat <<'EOF'
docs: add account affinity end-to-end manual test

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review 结果

**1. Spec coverage:**
- 第 1 节亲和 key 获取 → Task 2。✓
- 第 2 节数据结构与并发 → Task 3。✓（两把锁不嵌套：Task 5 的 `SelectForConversation` 先释放 affinity.lookup 再持 p.mu.RLock，确认无嵌套。）
- 第 3 节选号决策 → Task 5。✓
- 第 4 节绑定时机/迁移/失败 → Task 5（Remember）+ Task 7/8（成功分支 Remember）。✓
- 第 5 节配置开关 → Task 1 + Task 9（admin API）+ Task 10（前端）。✓
- 第 6 节测试 → Task 3/4/5 单元集成测试 + Task 7/8 handler 测试 + Task 11 端到端。✓

**2. Placeholder scan:** Task 7/8/9 的 handler 测试用了 `t.Skip` 占位——这是诚实的，因为未确认 handler 层有 mock CallKiroAPI 基建。已在 Task 11 用端到端手测兜底。其余步骤均含实际代码。无 "TBD/TODO/implement later"。

**3. Type consistency:**
- `ResolveClaudeConversationID` / `ResolveOpenAIConversationID` 在 Task 2 定义，Task 7/8 调用——签名一致。
- `SelectForConversation(convID, model string, excluded map[string]bool) *config.Account` 在 Task 5 定义，Task 7/8 调用——一致。
- `Remember(convID, accountID string)` 一致。
- `isAccountUsable(acc *config.Account, model string, excluded map[string]bool, now time.Time) bool` 在 Task 4 定义，Task 5 调用——一致。
- `affinityRouter.lookup/remember/cleanup` 在 Task 3 定义，Task 5/6 调用——一致。

无 spec 需求遗漏，无类型不一致。计划就绪。

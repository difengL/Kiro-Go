# Task 1 Report: 配置字段与 getter（AffinityEnabled / AffinityTTLMinutes）

## 改动的文件

- `config/config.go` — 新增 2 个结构体字段 + 4 个函数
- `config/config_test.go` — 追加 3 个测试函数

## 关键实现决策

### config 包实际变量名核对

Brief 中使用的占位名与代码库实际名称的映射：

| Brief 占位名 | 实际名称 | 定义位置 |
|---|---|---|
| `AppConfig` | `Config` | `config/config.go:147` |
| `globalCfg` | `cfg` | `config/config.go:238` |
| `cfgMu` | `cfgLock` (类型 `sync.RWMutex`) | `config/config.go:239` |
| `saveConfig()` | `Save()` | `config/config.go:336` |
| `configDir` | `cfgPath` | `config/config.go:240` |

### 新增结构体字段（在 `AllowOverUsage` 之后，约 177-181 行）

```go
AffinityEnabled    bool `json:"affinityEnabled,omitempty"`
AffinityTTLMinutes int  `json:"affinityTTLMinutes,omitempty"`
```

位置紧跟在 `AllowOverUsage` 字段后（line 175 之后），与 brief 要求一致。

### 新增函数（在 `UpdateAllowOverUsage` 之后，`GetLogLevel` 之前）

1. **`applyAffinityDefaults(c *Config)`** — 两字段均为零值时应用默认值（enabled=true, TTL=5）。说明：该函数目前在测试中直接调用，尚未集成到 `Load()` 流程中；后续 task 如需在加载配置时自动应用默认值，可加入 `Load()` 内。

2. **`GetAffinityEnabled() bool`** — 遵循现有 getter 模式（`cfgLock.RLock()` + `defer cfgLock.RUnlock()`）；nil config 或未显式配置时返回 `true`。

3. **`GetAffinityTTLMinutes() int`** — nil config 返回 5；越界值 `[1,60]` 外返回 5。

4. **`UpdateAffinitySettings(enabled bool, ttlMinutes int) error`** — ttl 越界 clamp 到 5；持写锁修改 cfg 后释放锁再 `Save()` 持久化。

### 测试适配

- 测试直接操作包级 singleton `cfg`/`cfgLock`（与现有测试中调用 `Init()` 再通过 getter 读取的方式不同），因为 affinity getter 有 nil-config 默认逻辑需要验证。
- 测试末尾恢复 `cfg = nil` 避免污染其他测试。
- 变量名从 brief 的 `globalCfg`/`cfgMu` 调整为实际的 `cfg`/`cfgLock`。

## 跑过的命令与输出摘要

### Step 2: 测试预期失败（构建错误）
```
go test ./config/ -run Affinity -v
# undefined: applyAffinityDefaults / AffinityEnabled / AffinityTTLMinutes
FAIL
```

### Step 4: 测试通过
```
go test ./config/ -run Affinity -v
=== RUN   TestAffinityDefaults
--- PASS: TestAffinityDefaults
=== RUN   TestGetAffinityTTLMinutesClamps
--- PASS: TestGetAffinityTTLMinutesClamps
=== RUN   TestGetAffinityEnabledDefaultTrue
--- PASS: TestGetAffinityEnabledDefaultTrue
PASS (3/3)
```

### 全量回归
```
go test ./config/ -v
20 passed (17 pre-existing + 3 new)
```

## 自审发现的问题

无。实现严格遵循现有代码范式（锁模式、getter 风格、Save 模式），测试覆盖了默认值、越界 clamp、nil config 三条核心路径。

## Commit

```
5e9ff6c feat(config): add affinityEnabled and affinityTTLMinutes settings
```

---

## 修复: UpdateAffinitySettings 锁模式不一致

### 问题

`UpdateAffinitySettings` 使用了 `cfgLock.Lock()` 后显式 `cfgLock.Unlock()`，在释放锁之后才调用 `Save()`。这与同文件其他所有 update 函数（`UpdateAllowOverUsage`、`UpdateProxySettings`、`UpdateThinkingConfig` 等）的范式不一致——后者都使用 `defer cfgLock.Unlock()` 并在持锁状态下调用 `Save()`，防止序列化时 cfg 被并发修改。

### 修复内容

将 `UpdateAffinitySettings` 的锁模式改为与 `UpdateAllowOverUsage` 完全一致：

```go
// 修复前
cfgLock.Lock()
cfg.AffinityEnabled = enabled
cfg.AffinityTTLMinutes = ttlMinutes
cfgLock.Unlock()
return Save()

// 修复后
cfgLock.Lock()
defer cfgLock.Unlock()
cfg.AffinityEnabled = enabled
cfg.AffinityTTLMinutes = ttlMinutes
return Save()
```

只改了锁模式，未改动字段、参数或任何其他函数。

### 测试命令与输出

```
$ go test ./config/ -run Affinity -v
=== RUN   TestAffinityDefaults
--- PASS: TestAffinityDefaults (0.00s)
=== RUN   TestGetAffinityTTLMinutesClamps
--- PASS: TestGetAffinityTTLMinutesClamps (0.00s)
=== RUN   TestGetAffinityEnabledDefaultTrue
--- PASS: TestGetAffinityEnabledDefaultTrue (0.00s)
PASS

$ go vet ./config/
(无输出，通过)

$ go test ./config/ -v
20 passed (全量回归通过)
```

### Commit

```
fix(config): hold lock while saving affinity settings
```

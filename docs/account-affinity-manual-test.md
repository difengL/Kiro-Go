# 账号亲和性 端到端手测

## 前置
- 至少 2 个可用账号（admin 面板可见 RequestCount）
- affinityEnabled=true, affinityTTLMinutes=5

## 步骤

### 1. 验证亲和性生效（cache 命中）
用同一首条 user message 连发 3 次 `/v1/messages`（messages 数组逐次增长，但首条 user 不变）。

```bash
# 第 1 次：只有首条 user
curl -s http://localhost:PORT/v1/messages \
  -H "x-api-key: YOUR_KEY" \
  -H "content-type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 100,
    "messages": [{"role": "user", "content": "Explain prompt caching in one sentence."}]
  }' | jq '.usage'

# 第 2 次：加 assistant 回复 + 新 user
curl -s http://localhost:PORT/v1/messages \
  -H "x-api-key: YOUR_KEY" \
  -H "content-type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 100,
    "messages": [
      {"role": "user", "content": "Explain prompt caching in one sentence."},
      {"role": "assistant", "content": "Prompt caching..."},
      {"role": "user", "content": "Give me more detail."}
    ]
  }' | jq '.usage'

# 第 3 次：继续加长
curl -s http://localhost:PORT/v1/messages \
  -H "x-api-key: YOUR_KEY" \
  -H "content-type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 100,
    "messages": [
      {"role": "user", "content": "Explain prompt caching in one sentence."},
      {"role": "assistant", "content": "Prompt caching..."},
      {"role": "user", "content": "Give me more detail."},
      {"role": "assistant", "content": "More detail..."},
      {"role": "user", "content": "And examples?"}
    ]
  }' | jq '.usage'
```

**预期**：
- 第 1 次：`cache_read_input_tokens = 0`（或 `cache_creation_input_tokens > 0`）
- 第 2、3 次：`cache_read_input_tokens > 0`（命中同账号缓存）
- admin 面板看 RequestCount：3 次请求集中在同一个账号

### 2. 验证关闭亲和性回归基线
1. admin 面板关闭 `affinityEnabled`
2. 同样连发 3 次请求
3. **预期**：可能分散到不同账号，`cache_read_input_tokens` 可能 0

### 3. 验证迁移
1. 亲和绑定到 a1（连发 1 次确认）
2. 在 admin 面板把 a1 临时禁用（或等冷却）
3. 再发同会话请求
4. **预期**：路由到 a2，且成功后 a1 的绑定被覆盖为 a2

### 4. 验证合成锚点跳过亲和
1. 发送首条 user 为 "." 的请求
2. 再发同首条 user 的请求
3. **预期**：两次可能命中不同账号（合成锚点亲和 key 为空，走纯轮询）

### 5. 验证 TTL 过期
1. 设置 affinityTTLMinutes=1
2. 发一次请求建立绑定
3. 等待 2 分钟
4. 再发同会话请求
5. **预期**：绑定已过期，可能路由到不同账号

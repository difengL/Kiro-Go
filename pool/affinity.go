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

// unbind 删除 convID 的绑定（无论是否过期）。空 key 直接 return。
// 用于坏号失败后立即解绑，避免死绑定占据 TTL 窗口。
func (a *affinityRouter) unbind(convID string) {
	if convID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.bindings, convID)
}

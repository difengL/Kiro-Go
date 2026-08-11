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

func TestAffinity_UnbindRemovesBinding(t *testing.T) {
	a := newAffinityRouter(5 * time.Minute)
	now := time.Now()
	a.remember("conv1", "acc1", now)
	a.unbind("conv1")
	if id, ok := a.lookup("conv1", now); ok || id != "" {
		t.Fatalf("unbind should remove binding, got id=%q ok=%v", id, ok)
	}
	// 空 key 是安全阀，不应 panic
	a.unbind("")
}

func TestAffinity_UnbindNoOpOnMissingKey(t *testing.T) {
	a := newAffinityRouter(5 * time.Minute)
	a.unbind("unknown")
	a.mu.RLock()
	n := len(a.bindings)
	a.mu.RUnlock()
	if n != 0 {
		t.Fatalf("unbind missing key should be no-op, got len=%d", n)
	}
}

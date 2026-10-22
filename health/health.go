// Package health 提供存活 / 就绪探针。
//
// 一个 handler，两种用法：
//
//	不注册任何检查 → 纯存活探针（进程还在响应 = 活着），容器 liveness 用它；
//	注册依赖检查   → 就绪探针（MySQL/Redis 断了就 503），容器 readiness 用它。
//
// 检查是现查的，不带缓存：缓存会让「刚恢复」和「刚挂掉」都延迟一个周期才反映出来。
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// 单次探测的总超时：探针必须比任何调用方都先返回，卡住就等于探针失效。
const probeTimeout = 3 * time.Second

var (
	mu     sync.RWMutex
	checks = map[string]func(context.Context) error{}
)

// Register 注册一个依赖检查，名字用于在响应里指出是谁挂了（同一个名字重复注册会覆盖）。
func Register(name string, check func(context.Context) error) {
	mu.Lock()
	defer mu.Unlock()
	checks[name] = check
}

// Handler 返回探针端点。
//
//	200 {"status":"ok"}
//	503 {"status":"degraded","failing":{"mysql":"dial tcp ..."}}
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()

		mu.RLock()
		snapshot := make(map[string]func(context.Context) error, len(checks))
		for name, check := range checks {
			snapshot[name] = check
		}
		mu.RUnlock()

		failing := map[string]string{}
		for name, check := range snapshot {
			if err := check(ctx); err != nil {
				failing[name] = err.Error()
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if len(failing) == 0 {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "degraded", "failing": failing})
	})
}

// Check 是一次探测的结果。
type Check struct {
	Name      string
	OK        bool
	Error     string
	LatencyMs int64
}

// Report 现查一次并返回结构化结果（admin 的运维页用它，与 Handler 同源）。
func Report(ctx context.Context) []Check {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	mu.RLock()
	names := make([]string, 0, len(checks))
	snapshot := make(map[string]func(context.Context) error, len(checks))
	for name, check := range checks {
		names = append(names, name)
		snapshot[name] = check
	}
	mu.RUnlock()
	sort.Strings(names)

	out := make([]Check, 0, len(names))
	for _, name := range names {
		start := time.Now()
		err := snapshot[name](ctx)
		c := Check{Name: name, OK: err == nil, LatencyMs: time.Since(start).Milliseconds()}
		if err != nil {
			c.Error = err.Error()
		}
		out = append(out, c)
	}
	return out
}

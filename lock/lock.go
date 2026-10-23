// Package lock 定义分布式互斥锁的协议，不绑定任何具体驱动。
//
// 框架只规定协议：SetNX 抢占、随机 token 标识持有者、释放与续期都用 Lua
// 比对 token、后台协程按 ttl/3 续期。驱动由项目提供二十行适配器：
//
//	type redisClient struct{ rdb *redis.Client }
//
//	func (c redisClient) SetNX(ctx context.Context, key, val string, ttl time.Duration) (bool, error) {
//		return c.rdb.SetNX(ctx, key, val, ttl).Result()
//	}
//
//	func (c redisClient) EvalInt(ctx context.Context, script string, keys []string, args ...any) (int64, error) {
//		return c.rdb.Eval(ctx, script, keys, args...).Int64()
//	}
//
// 这样本包不 import go-redis，也不拥有被引用的类型；换成集群客户端或其它
// 支持 SetNX + Eval 的存储，只要实现 Client 两个方法即可。
package lock

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"sync"
	"time"
)

// 释放与续期都必须「先比对 token 再动手」：裸 DEL 会删掉别人超时后重新拿到的锁。
// EvalInt 的返回值约定为 1=成功、0=token 不匹配（锁已不属于自己）。
const (
	unlockScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0`

	extendScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0`
)

var (
	// ErrNotHeld 表示当前 Mutex 没有持有锁（未拿到、已经释放，或释放后又调用了 Unlock）。
	ErrNotHeld = errors.New("lock: not held")

	// ErrLostLock 表示持有期间锁已经丢了：续期失败，或释放时发现 token 已不匹配。
	// 此时临界区可能已经与别人重叠，调用方不能再把这段操作当成互斥的。
	ErrLostLock = errors.New("lock: lost lock")

	// errAlreadyHeld 防止同一个 Mutex 重入：TryLock 返回错误，Lock 直接失败而不是自旋。
	errAlreadyHeld = errors.New("lock: already held")
)

// Client 是锁协议需要的最小驱动面。
//
// SetNX 在 key 不存在时写入 val 并设置 ttl，返回是否写入成功。
// EvalInt 执行 Lua 脚本并返回整数结果，keys/args 的语义与 Redis EVAL 一致。
type Client interface {
	SetNX(ctx context.Context, key, val string, ttl time.Duration) (bool, error)
	EvalInt(ctx context.Context, script string, keys []string, args ...any) (int64, error)
}

// Mutex 是单个 key 上的互斥锁。零值不可用，必须用 New 创建。
type Mutex struct {
	c   Client
	key string
	ttl time.Duration

	// acquireMu 串行化本实例的 TryLock，避免并发抢占时 state 被两次写入。
	acquireMu sync.Mutex

	// mu 保护 cur 与 session.lost。
	mu  sync.Mutex
	cur *session
}

// session 是一次成功抢占的全部状态；Unlock 只认自己这一次的 token 与协程。
type session struct {
	token string
	stop  chan struct{} // Unlock 关闭，通知 watch 退出
	done  chan struct{} // watch 关闭，Unlock 等它真正退出后再释放
	lost  error         // 受 Mutex.mu 保护
}

// New 创建一把锁。ttl 必须为正数，否则后续获取会返回错误（New 本身不返回 error）。
func New(c Client, key string, ttl time.Duration) *Mutex {
	return &Mutex{c: c, key: key, ttl: ttl}
}

// TryLock 尝试获取一次，不等待。被占用返回 (false, nil)；驱动错误原样返回。
// 同一个 Mutex 已经持有时返回错误，避免重入。
func (m *Mutex) TryLock(ctx context.Context) (bool, error) {
	if err := m.validate(); err != nil {
		return false, err
	}
	if ctx == nil {
		return false, errors.New("lock: nil context")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	m.acquireMu.Lock()
	defer m.acquireMu.Unlock()

	// 等 acquireMu 期间 ctx 可能已经结束，不能再拿一把「已过期」的锁。
	if err := ctx.Err(); err != nil {
		return false, err
	}

	m.mu.Lock()
	held := m.cur != nil
	m.mu.Unlock()
	if held {
		return false, fmt.Errorf("%w: %q", errAlreadyHeld, m.key)
	}

	token, err := newToken()
	if err != nil {
		return false, fmt.Errorf("lock: generate token: %w", err)
	}

	ok, err := m.c.SetNX(ctx, m.key, token, m.ttl)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	s := &session{
		token: token,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	m.mu.Lock()
	m.cur = s
	m.mu.Unlock()

	go m.watch(s)
	return true, nil
}

// Lock 阻塞直到拿到锁或 ctx 结束。
//
// 每次失败后做指数退避加随机抖动，避免大量实例在同一毫秒反复撞击同一个 key。
// 驱动错误也会重试：网络抖动不应该让一次「等到锁」变成「直接失败」。
func (m *Mutex) Lock(ctx context.Context) error {
	if err := m.validate(); err != nil {
		return err
	}
	if ctx == nil {
		return errors.New("lock: nil context")
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		ok, err := m.TryLock(ctx)
		if err == nil && ok {
			return nil
		}
		if errors.Is(err, errAlreadyHeld) {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		timer := time.NewTimer(retryDelay(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Unlock 释放锁。只有仍持有 token 的那一方能删掉 key。
//
// 未持有时返回 ErrNotHeld（可以重复调用，不会二次删除）；后台续期已经失败时
// 仍尽力释放，但返回 ErrLostLock，让调用方知道临界区可能不再独占。
func (m *Mutex) Unlock(ctx context.Context) error {
	if m == nil {
		return ErrNotHeld
	}

	m.mu.Lock()
	s := m.cur
	if s == nil {
		m.mu.Unlock()
		return ErrNotHeld
	}
	m.cur = nil
	close(s.stop)
	m.mu.Unlock()

	// 先等续期协程退出，再删 key：否则它可能在删除之后又把 TTL 续上。
	<-s.done

	m.mu.Lock()
	lost := s.lost
	m.mu.Unlock()

	n, err := m.c.EvalInt(ctx, unlockScript, []string{m.key}, s.token)
	if lost != nil {
		if err != nil {
			return errors.Join(lost, err)
		}
		return lost
	}
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLostLock
	}
	return nil
}

// Extend 手动续期一次。未持有时返回 ErrNotHeld；token 已不匹配或驱动失败时
// 标记锁已丢失（后续 Unlock 返回 ErrLostLock）。
func (m *Mutex) Extend(ctx context.Context) error {
	if m == nil {
		return ErrNotHeld
	}

	m.mu.Lock()
	s := m.cur
	if s == nil {
		m.mu.Unlock()
		return ErrNotHeld
	}
	if s.lost != nil {
		lost := s.lost
		m.mu.Unlock()
		return lost
	}
	m.mu.Unlock()

	n, err := m.evalExtend(ctx, s)
	if err != nil {
		m.markLost(s)
		return err
	}
	if n == 0 {
		m.markLost(s)
		return ErrLostLock
	}
	return nil
}

// Once 是「抢占一次、用完释放」的便捷形式。
//
// 拿到锁返回 acquired=true 和带 Lua 校验的 release；被别人占着返回
// (false, nil, nil)；驱动错误原样返回，所以「被占」与「出错」不会混淆。
func Once(ctx context.Context, c Client, key string, ttl time.Duration) (acquired bool, release func(context.Context) error, err error) {
	m := New(c, key, ttl)
	ok, err := m.TryLock(ctx)
	if err != nil {
		return false, nil, err
	}
	if !ok {
		return false, nil, nil
	}
	return true, m.Unlock, nil
}

func (m *Mutex) validate() error {
	if m == nil {
		return errors.New("lock: nil mutex")
	}
	if m.c == nil {
		return errors.New("lock: nil client")
	}
	if m.ttl <= 0 {
		return fmt.Errorf("lock: ttl must be positive, got %s", m.ttl)
	}
	return nil
}

// watch 每 ttl/3 续期一次。第一次失败就停：锁可能已经过期，继续「续」只是自欺。
func (m *Mutex) watch(s *session) {
	defer close(s.done)

	interval := m.ttl / 3
	if interval <= 0 {
		interval = m.ttl
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 续期调用必须有上限，否则驱动卡住时 Unlock 会一直等 done。
	timeout := m.ttl / 2
	if timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	if timeout <= 0 {
		timeout = m.ttl
	}

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}

		// ticker 与 stop 同时就绪时可能多走一轮，先确认自己还没被 Unlock。
		select {
		case <-s.stop:
			return
		default:
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		n, err := m.evalExtend(ctx, s)
		cancel()
		if err != nil || n == 0 {
			m.markLost(s)
			return
		}
	}
}

func (m *Mutex) evalExtend(ctx context.Context, s *session) (int64, error) {
	ms := m.ttl.Milliseconds()
	if ms < 1 { // 正数 ttl 但不足 1ms 时 PEXPIRE 0 会直接删 key
		ms = 1
	}
	return m.c.EvalInt(ctx, extendScript, []string{m.key}, s.token, ms)
}

func (m *Mutex) markLost(s *session) {
	m.mu.Lock()
	if m.cur == s && s.lost == nil {
		s.lost = ErrLostLock
	}
	m.mu.Unlock()
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// retryDelay 返回 [d/2, d] 的随机退避，d 从 20ms 指数增长到 500ms。
func retryDelay(attempt int) time.Duration {
	const (
		base = 20 * time.Millisecond
		max  = 500 * time.Millisecond
	)
	if attempt > 10 {
		attempt = 10
	}
	d := base << attempt
	if d > max || d <= 0 {
		d = max
	}
	half := d / 2
	return half + time.Duration(mrand.Int64N(int64(half)+1))
}

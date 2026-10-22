// Package core 是生成物要求的运行时契约：进程内 gRPC 注册表、消息上限、
// 调用/被调用拦截器（trace 传播 + panic 恢复 + 可选的访问日志钩子）。
//
// 这里只有**机制**，没有策略：
//   - 要跨进程传播什么，由项目通过 Propagator 提供（框架不定义载荷的形状）；
//   - 访问日志怎么写、写到哪，由项目通过 Observer 提供（框架不定义日志格式）。
//
// 所以这个包不 import 任何业务 proto、不依赖 logger/ORM/MQ。
package core

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"google.golang.org/grpc"
)

// MaxMsgSize 是进程内 gRPC 单条消息的上限（收发都按它）。
//
// SDK 默认 4MiB 装不下多模态载荷：一个请求带 4 张 2MiB 图片就是 8MiB，撞上直接
// ResourceExhausted，而错误信息只会说「消息过大」，很难定位。进程内 loopback +
// 一次内存拷贝，放大到 32MiB 的代价可以接受；真正的体积收口在业务输入层，
// 不靠这条兜底。
const MaxMsgSize = 32 << 20

// GlobalRegistry 是进程内服务注册表。
var GlobalRegistry = &ServiceRegistry{
	services: make(map[string]string),
	clients:  make(map[string]any),
	mu:       sync.RWMutex{},
}

// ServiceRegistry 按 gRPC 服务名记录监听地址与复用中的客户端连接。
type ServiceRegistry struct {
	services map[string]string // 服务名 → 地址
	clients  map[string]any    // 服务名 → client
	mu       sync.RWMutex
}

// RegisterAddr 记录服务监听地址。
func (s *ServiceRegistry) RegisterAddr(serviceDesc grpc.ServiceDesc, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services[serviceDesc.ServiceName] = addr
}

// RegisterClient 缓存客户端连接，供同进程调用复用。
func (s *ServiceRegistry) RegisterClient(serviceDesc grpc.ServiceDesc, client any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[serviceDesc.ServiceName] = client
}

// GetClient 取已缓存的客户端。
func (s *ServiceRegistry) GetClient(serviceDesc grpc.ServiceDesc) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	client, exists := s.clients[serviceDesc.ServiceName]
	return client, exists
}

// Discover 查服务地址。
func (s *ServiceRegistry) Discover(serviceDesc grpc.ServiceDesc) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	addr, exists := s.services[serviceDesc.ServiceName]
	return addr, exists
}

// MustDiscover 查服务地址，不存在直接 panic：生成的调用方代码要求「注册先于调用」，
// 静默返回空地址只会把错误推迟到一次莫名的连接失败。
func (s *ServiceRegistry) MustDiscover(serviceDesc grpc.ServiceDesc) string {
	addr, exists := s.Discover(serviceDesc)
	if !exists {
		panic("microservice not found: " + serviceDesc.ServiceName)
	}
	return addr
}

// ShutdownContext 返回「进程收到 SIGINT/SIGTERM 时取消」的 ctx。
//
// 生成的注册代码在 ctx.Done 时 GracefulStop，后台循环也靠这个 ctx 退出；
// 但没人监听信号就等于它们永远不会被触发——SIGTERM 直接杀进程，在途请求被切断。
// 所以这一步必须是框架的，不能指望每个项目都想起来。
func ShutdownContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

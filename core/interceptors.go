package core

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TraceKey 是身份载荷（项目自己的结构）在 metadata 里的 key。
// 追踪上下文走标准的 traceparent/baggage，不占这个 key。
const TraceKey = "TraceKey"

// Propagator 决定「跨进程要带什么身份信息」。框架负责什么时候带、走哪个 key，
// 项目负责编码成什么——通常是自己的调用主体结构。
type Propagator interface {
	// Encode 从 ctx 提取要传播的内容；ok=false 表示本次调用没有可传播的东西。
	Encode(ctx context.Context) (raw string, ok bool)
	// Decode 把收到的内容写回 ctx（被调用方）。
	Decode(ctx context.Context, raw string) context.Context
}

// Observer 接收每次 RPC 的结果。server=false 表示调用方视角（发起调用）。
//
// 它是**观察点**，不是日志接口：访问日志（observe）、指标（metrics）都从这里
// 挂上去，两者互不覆盖，所以注册是 Add 而不是 Set。
type Observer interface {
	Unary(ctx context.Context, method string, req any, dur time.Duration, err error, server bool)
	Stream(ctx context.Context, method string, dur time.Duration, err error, server bool)
}

var (
	propagator  atomic.Value // Propagator：身份载荷只有一份，所以是 Set
	observers   atomic.Value // []Observer：观察者可以叠加
	defaultUnix atomic.Int64 // 默认超时的纳秒数，0 = 不补
)

// SetPropagator 注册身份载荷的传播实现。不设置 = 不传播身份（单项目/演示默认行为）。
func SetPropagator(p Propagator) { propagator.Store(&p) }

// AddObserver 追加一个观察者。日志、指标各注册一个，互不影响。
func AddObserver(o Observer) {
	cur, _ := observers.Load().([]Observer)
	next := make([]Observer, 0, len(cur)+1)
	observers.Store(append(append(next, cur...), o))
}

// SetDefaultTimeout 给调用方的一元调用补默认超时：ctx 自带 deadline 时不动它，
// 没带时才补。0 = 不补（默认）。
//
// 流式调用不补：流的生命周期由业务决定（SSE 可能持续几分钟），
// 一刀切会切断正常的长连接；要给流设上限就在调用点自己 WithTimeout。
func SetDefaultTimeout(d time.Duration) { defaultUnix.Store(int64(d)) }

func currentPropagator() Propagator {
	v, ok := propagator.Load().(*Propagator)
	if !ok || v == nil || *v == nil {
		return nil
	}
	return *v
}

func currentObservers() []Observer {
	v, _ := observers.Load().([]Observer)
	return v
}

// withDefaultDeadline 在没有 deadline 时补一个默认超时。
func withDefaultDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	d := time.Duration(defaultUnix.Load())
	if _, ok := ctx.Deadline(); !ok && d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return ctx, func() {}
}

// outbound 把身份载荷附到 outgoing metadata 上（追踪上下文由 injectTrace 负责）。
func outbound(ctx context.Context) context.Context {
	p := currentPropagator()
	if p == nil {
		return ctx
	}
	raw, ok := p.Encode(ctx)
	if !ok {
		return ctx
	}
	md, has := metadata.FromOutgoingContext(ctx)
	if has {
		md = md.Copy()
	} else {
		md = metadata.MD{}
	}
	md.Set(TraceKey, raw)
	return metadata.NewOutgoingContext(ctx, md)
}

// inbound 把收到的身份载荷写回 ctx。
func inbound(ctx context.Context) context.Context {
	p := currentPropagator()
	if p == nil {
		return ctx
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	raw := md.Get(TraceKey)
	if len(raw) == 0 || raw[0] == "" {
		return ctx
	}
	return p.Decode(ctx, raw[0])
}

// rpcAttrs 是 span 上的标准属性（OTel 语义约定 rpc.*）。
func rpcAttrs(method string) []attribute.KeyValue {
	service, name := strings.TrimPrefix(method, "/"), method
	if i := strings.LastIndex(service, "/"); i >= 0 {
		service, name = service[:i], service[i+1:]
	}
	return []attribute.KeyValue{
		attribute.String("rpc.system", "grpc"),
		attribute.String("rpc.service", service),
		attribute.String("rpc.method", name),
	}
}

// endRPC 结束 span 并把 gRPC 状态码记上去。
func endRPC(span trace.Span, err error) {
	if err != nil {
		span.SetAttributes(attribute.Int64("rpc.grpc.status_code", int64(status.Code(err))))
	}
	EndSpan(span, err)
}

// isLoopback 判断目标是不是本进程：模块化单体的服务间调用走 127.0.0.1，
// 没有网络跳，client 侧再记一条 span/日志只是把同一个事实说两遍。
func isLoopback(cc *grpc.ClientConn) bool {
	if cc == nil {
		return false
	}
	target := cc.Target()
	host := target
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// MetadataCallerInterceptor 是调用方一元拦截器：起 span + 传播 + 默认超时 + 观察。
//
// 进程内调用（loopback）只传播不起 span、不记日志：服务端那条已经覆盖，
// 否则一次调用会同时出现 client 和 server 两个节点、两条日志。
func MetadataCallerInterceptor(
	ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
) error {
	if isLoopback(cc) {
		ctx = injectTrace(outbound(ctx))
		ctx, cancel := withDefaultDeadline(ctx)
		defer cancel()
		return invoker(ctx, method, req, reply, cc, opts...)
	}

	ctx, span := StartSpan(ctx, method, SpanKindClient, rpcAttrs(method)...)
	ctx = injectTrace(outbound(ctx))
	ctx, cancel := withDefaultDeadline(ctx)
	defer cancel()

	start := time.Now()
	err := invoker(ctx, method, req, reply, cc, opts...)
	endRPC(span, err)

	for _, o := range currentObservers() {
		o.Unary(ctx, method, req, time.Since(start), err, false)
	}
	return err
}

// MetadataCalleeInterceptor 是被调用方一元拦截器：还原父 span + 起 span + panic 恢复 + 观察。
// 返回值里的 error 必须是 gRPC status，否则网关拿不到可读信息。
func MetadataCalleeInterceptor(
	ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (resp any, err error) {
	ctx = extractTrace(inbound(ctx))
	ctx, span := StartSpan(ctx, info.FullMethod, SpanKindServer, rpcAttrs(info.FullMethod)...)

	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			err = status.Errorf(codes.Internal, "%v", r)
		}
		endRPC(span, err)
		for _, o := range currentObservers() {
			o.Unary(ctx, info.FullMethod, req, time.Since(start), err, true)
		}
	}()
	return handler(ctx, req)
}

// MetadataStreamCallerInterceptor 是调用方流式拦截器。
//
// span 覆盖整条流：由 tracedClientStream 在失败或 Recv 到 EOF 时结束，
// 否则一条 SSE 的耗时会被记成「建立连接」的那几微秒。
func MetadataStreamCallerInterceptor(
	ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string,
	streamer grpc.Streamer, opts ...grpc.CallOption,
) (grpc.ClientStream, error) {
	if isLoopback(cc) {
		return streamer(injectTrace(outbound(ctx)), desc, cc, method, opts...)
	}

	ctx, span := StartSpan(ctx, method, SpanKindClient, rpcAttrs(method)...)
	ctx = injectTrace(outbound(ctx))

	start := time.Now()
	stream, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		endRPC(span, err)
	}

	for _, o := range currentObservers() {
		o.Stream(ctx, method, time.Since(start), err, false)
	}
	if err != nil {
		return stream, err
	}
	return &tracedClientStream{ClientStream: stream, span: span}, nil
}

// MetadataStreamCalleeInterceptor 是被调用方流式拦截器。
func MetadataStreamCalleeInterceptor(
	srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler,
) (err error) {
	ctx := extractTrace(inbound(ss.Context()))
	ctx, span := StartSpan(ctx, info.FullMethod, SpanKindServer, rpcAttrs(info.FullMethod)...)

	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			err = status.Errorf(codes.Internal, "%v", r)
		}
		endRPC(span, err)
		for _, o := range currentObservers() {
			o.Stream(ctx, info.FullMethod, time.Since(start), err, true)
		}
	}()
	return handler(srv, &wrappedServerStream{ServerStream: ss, ctx: ctx})
}

// tracedClientStream 在流结束时收尾 span（正常 EOF 不算错误）。
type tracedClientStream struct {
	grpc.ClientStream
	span trace.Span
	once sync.Once
}

func (s *tracedClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		if err == io.EOF {
			err = nil
		}
		s.finish(err)
	}
	return err
}

func (s *tracedClientStream) finish(err error) {
	s.once.Do(func() { endRPC(s.span, err) })
}

// wrappedServerStream 让处理器拿到注入了身份与追踪的 ctx。
type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context { return w.ctx }

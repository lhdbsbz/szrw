package core

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

// TracerName 是所有框架 span 的 instrumentation scope 名。
const TracerName = "github.com/lhdbsbz/szrw"

// 追踪走标准协议：W3C traceparent + baggage，经 gRPC metadata 传。
// 不自定义头名、不自定义编码——任何 OTel SDK / 服务网格 / 网关都认。
const (
	SpanKindServer = trace.SpanKindServer
	SpanKindClient = trace.SpanKindClient
)

// metadataCarrier 把 gRPC metadata 借给 OTel 的 TextMapPropagator。
type metadataCarrier struct{ md metadata.MD }

// parentSpanKey 存当前 span 的父 id（OTel 的 Span 接口不暴露 Parent）。
type parentSpanKey struct{}

func (c metadataCarrier) Get(key string) string {
	if v := c.md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

func (c metadataCarrier) Set(key, value string) { c.md.Set(key, value) }

func (c metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(c.md))
	for k := range c.md {
		keys = append(keys, k)
	}
	return keys
}

// injectTrace 把当前 span 注入 outgoing metadata（写 traceparent/baggage）。
func injectTrace(ctx context.Context) context.Context {
	md, ok := metadata.FromOutgoingContext(ctx)
	if ok {
		md = md.Copy()
	} else {
		md = metadata.MD{}
	}
	otel.GetTextMapPropagator().Inject(ctx, metadataCarrier{md: md})
	return metadata.NewOutgoingContext(ctx, md)
}

// extractTrace 从 incoming metadata 还原父 span（读 traceparent/baggage）。
func extractTrace(ctx context.Context) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, metadataCarrier{md: md})
}

// StartSpan 起一个 span。拦截器用它，项目自己埋点（缓存、外部 HTTP、批处理）也用它，
// 这样树是连着的：父 span 从 ctx 里取，不需要手传参数。
func StartSpan(ctx context.Context, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	// 父 id 在 Start 之后就取不到了（ctx 里已换成新 span），先存进 ctx：
	// 日志带上它，离线也能把树拼回来，不依赖任何 tracing 后端。
	parent := trace.SpanContextFromContext(ctx).SpanID()
	ctx, span := otel.Tracer(TracerName).Start(ctx, name,
		trace.WithSpanKind(kind),
		trace.WithAttributes(attrs...),
	)
	if parent.IsValid() {
		ctx = context.WithValue(ctx, parentSpanKey{}, parent.String())
	}
	return ctx, span
}

// ParentSpanID 返回当前 span 的父 span id（根 span 为空串）。
func ParentSpanID(ctx context.Context) string {
	id, _ := ctx.Value(parentSpanKey{}).(string)
	return id
}

// EndSpan 收尾：错误记进 span 并标 Error；nil 不标（未采样时是空操作）。
func EndSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// TraceID 返回当前链路 id（32 位十六进制）；没有 span 时返回空串。
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// SpanID 返回当前 span id（16 位十六进制）；没有 span 时返回空串。
func SpanID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.SpanID().String()
}

// headerCarrier 让 OTel 从 HTTP 请求头读写 traceparent/baggage。
type headerCarrier http.Header

func (c headerCarrier) Get(key string) string { return http.Header(c).Get(key) }
func (c headerCarrier) Set(key, value string) { http.Header(c).Set(key, value) }
func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// ExtractTraceHTTP 从 HTTP 请求头还原上游链路（标准 traceparent/baggage）。
// HTTP 入口（网关、gin 中间件）用它做根 span 的父上下文。
func ExtractTraceHTTP(ctx context.Context, h http.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, headerCarrier(h))
}

// Package gateway 装配 grpc-gateway 的 mux。
//
// 它固定的是**两端必须一致**的部分，否则生成的 TS 类型解析不了、流式会被包坏：
//
//   - JSON 口径：camelCase（UseProtoNames=false）+ 输出零值（EmitUnpopulated）
//   - 枚举用名字 + 反序列化忽略未知字段
//   - 纯 server-streaming 且挂了 HTTP 注解的方法按 SSE 逐块直出，**不经过响应改写**
//   - trace 传播不在网关里做：调用方拦截器（core.MetadataCallerInterceptor）
//     已经会把 ctx 里的内容编码进 metadata，这里再写一次只会多一套口径
//
// 它不管的是**产品风格**，全部通过 Options 交出去：
//
//   - 成功响应长什么样（信封 {code,message,data}? 原样 proto JSON?）
//   - 错误怎么写给客户端（恒 200 + 信封? 标准 4xx/5xx?）
//   - 哪些 gRPC 响应 metadata 变成 HTTP 头
package gateway

import (
	"context"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// RegisterFunc 挂载生成的网关路由，由 gen/wiring.RegisterGateways 提供。
type RegisterFunc func(mux *runtime.ServeMux)

// Options 是网关的三个策略钩子。都不设置时：响应原样返回、错误走标准 HTTP 状态、
// 响应 metadata 一律不外泄——也就是最保守的一套行为。
type Options struct {
	// Register 必填：把 proto 里的 google.api.http 注解挂到 mux 上。
	Register RegisterFunc

	// Streaming 是「纯 server-streaming 且挂了 HTTP 注解」的方法全名集合，
	// 由 gen/wiring.StreamingGatewayMethods 生成。这些方法不套 RewriteResponse。
	Streaming map[string]bool

	// RewriteResponse 决定成功响应体。返回 (改写后的值, nil) 就按它序列化；
	// 返回 (nil, err) 交给 WriteError。nil = 原样返回 proto JSON。
	RewriteResponse func(ctx context.Context, resp proto.Message) (any, error)

	// WriteError 决定错误怎么写。nil = grpc-gateway 默认（gRPC code → HTTP 状态）。
	WriteError func(w http.ResponseWriter, err error)

	// OutgoingHeader 决定哪个 gRPC 响应 metadata key 允许变成 HTTP 头。
	// nil = 全部不外泄（metadata 常含内部信息，默认外泄是事故）。
	OutgoingHeader func(key string) (string, bool)
}

// marshaler 是唯一口径：改了它，生成的 TS 类型与前端解析就会对不上。
func marshaler() runtime.Marshaler {
	return &runtime.JSONPb{
		MarshalOptions: protojson.MarshalOptions{
			UseProtoNames:   false, // camelCase，而不是 proto 字段名
			EmitUnpopulated: true,  // 前端拿到稳定的字段形状，不用到处判 undefined
			UseEnumNumbers:  false, // 枚举用名字（"MESSAGE_TYPE_TEXT"），不是数字
		},
		UnmarshalOptions: protojson.UnmarshalOptions{
			DiscardUnknown: true, // 旧客户端多传字段不应报错
		},
	}
}

// New 构造挂好路由的 mux。调用方决定把它挂到哪个 HTTP 引擎（gin/net/http/...）。
func New(opts Options) *runtime.ServeMux {
	if opts.Register == nil {
		panic("gateway.New: Options.Register 必填（gen/wiring.RegisterGateways）")
	}

	m := marshaler()
	mux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, m),
		runtime.WithForwardResponseRewriter(rewriter(opts)),
		runtime.WithOutgoingHeaderMatcher(headerMatcher(opts)),
		runtime.WithErrorHandler(func(ctx context.Context, mux *runtime.ServeMux, _ runtime.Marshaler,
			w http.ResponseWriter, r *http.Request, err error) {
			if opts.WriteError != nil {
				opts.WriteError(w, err)
				return
			}
			runtime.DefaultHTTPErrorHandler(ctx, mux, m, w, r, err)
		}),
		runtime.WithRoutingErrorHandler(func(ctx context.Context, mux *runtime.ServeMux, _ runtime.Marshaler,
			w http.ResponseWriter, r *http.Request, httpStatus int) {
			if opts.WriteError != nil {
				opts.WriteError(w, status.Error(httpCode(httpStatus), http.StatusText(httpStatus)))
				return
			}
			runtime.DefaultRoutingErrorHandler(ctx, mux, m, w, r, httpStatus)
		}),
	)
	opts.Register(mux)
	return mux
}

// rewriter 只在有策略时改写；流式方法永远直出。
func rewriter(opts Options) runtime.ForwardResponseRewriter {
	return func(ctx context.Context, resp proto.Message) (any, error) {
		if method, ok := runtime.RPCMethod(ctx); ok && opts.Streaming[method] {
			return resp, nil
		}
		if opts.RewriteResponse == nil {
			return resp, nil
		}
		return opts.RewriteResponse(ctx, resp)
	}
}

func headerMatcher(opts Options) runtime.HeaderMatcherFunc {
	return func(key string) (string, bool) {
		if opts.OutgoingHeader == nil {
			return "", false
		}
		return opts.OutgoingHeader(key)
	}
}

// httpCode 把 HTTP 状态映射成最接近的 gRPC code，让 WriteError 无论从哪条路径来
// （业务错误 / 路由错误）都只看到 gRPC status。
func httpCode(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusMethodNotAllowed:
		return codes.Unimplemented
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusRequestTimeout:
		return codes.DeadlineExceeded
	case http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	default:
		return codes.Internal
	}
}

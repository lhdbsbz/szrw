// Package observe 是「什么都不写也能看清调用」的默认实现：
// 每次 RPC 一条结构化日志（带 trace_id + span_id），HTTP 入口起根 span。
//
// 追踪上下文走 W3C traceparent/baggage（见 szrw-runtime/tracing），本包不自定义格式。
// 产品要换日志落盘/字段，用 core.AddObserver 再挂一个观察者，或整体替换本包。
package observe

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/lhdbsbz/szrw/core"
)

// Install 装上默认访问日志。重复调用无副作用。
//
// 它只注册观察者，不装 TracerProvider（那是 tracing.Install 的事）：
// 没装 provider 时日志里的 trace_id/span_id 为空，但调用本身不受影响。
func Install() {
	core.AddObserver(slogObserver{})
}

// traceHeader 是 HTTP 侧对外暴露的链路号头：响应里回写 trace id，
// 客户端报障时直接给出这个号就能定位整条链路。
const traceHeader = "X-Trace-Id"

// Handler 给 HTTP 入口包一层：从请求头还原上游链路（标准 traceparent），
// 起一个 server span 作为整条链路的根，并把 trace id 回写到响应头。
func Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := core.ExtractTraceHTTP(r.Context(), r.Header)
		ctx, span := core.StartSpan(ctx, r.Method+" "+r.URL.Path, core.SpanKindServer,
			attribute.String("http.request.method", r.Method),
			attribute.String("url.path", r.URL.Path),
		)
		defer span.End()

		w.Header().Set(traceHeader, core.TraceID(ctx))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// slogObserver 是默认访问日志：结构化、走标准库 slog、不引第三方。
type slogObserver struct{}

func (slogObserver) Unary(ctx context.Context, method string, req any, dur time.Duration, err error, server bool) {
	fields := []any{
		slog.String("call", callKind(server)),
		slog.String("method", method),
		slog.Int64("duration_ms", dur.Milliseconds()),
		slog.String("trace_id", core.TraceID(ctx)),
		slog.String("span_id", core.SpanID(ctx)),
	}
	if err != nil {
		fields = append(fields, slog.String("error", err.Error()))
		// 失败才带请求体：成功路径带会让日志体积随流量爆炸。
		if req != nil {
			if raw, mErr := json.Marshal(req); mErr == nil {
				fields = append(fields, slog.String("request", string(raw)))
			}
		}
		slog.ErrorContext(ctx, method, fields...)
		return
	}
	slog.InfoContext(ctx, method, fields...)
}

func (slogObserver) Stream(ctx context.Context, method string, dur time.Duration, err error, server bool) {
	fields := []any{
		slog.String("call", callKind(server)),
		slog.String("method", method),
		slog.Int64("duration_ms", dur.Milliseconds()),
		slog.String("trace_id", core.TraceID(ctx)),
		slog.String("span_id", core.SpanID(ctx)),
	}
	if err != nil {
		fields = append(fields, slog.String("error", err.Error()))
		slog.ErrorContext(ctx, method, fields...)
		return
	}
	slog.InfoContext(ctx, method, fields...)
}

func callKind(server bool) string {
	if server {
		return "GRPC_SERVER"
	}
	return "GRPC"
}

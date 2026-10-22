// Package tracing 装 OpenTelemetry 的 TracerProvider。
//
// 装了什么都不会变慢：没配导出端点时，span 照常创建（trace id / span id 有效，
// 日志里能串成一棵树），只是不往外发。要发就设标准环境变量：
//
//	OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318   # OTLP/HTTP
//	OTEL_SERVICE_NAME=service                            # 资源上的服务名
//
// 也就是说：**追踪的落地不是框架替项目选后端**，而是项目设一个环境变量，
// 指向 Jaeger / Tempo / 任意 OTLP 兼容后端。
package tracing

import (
	"context"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

var provider *sdktrace.TracerProvider

// Install 设置全局传播器与 TracerProvider。重复调用只生效一次。
func Install(processors ...sdktrace.SpanProcessor) {
	if provider != nil {
		return
	}

	// W3C 标准：traceparent + tracestate + baggage。不自定义头名、不自定义编码。
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	opts := []sdktrace.TracerProviderOption{
		// 父子一致：入口决定采不采，下游跟着走，不会出现「一半有 span 一半没」。
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithResource(newResource()),
	}
	for _, p := range processors {
		opts = append(opts, sdktrace.WithSpanProcessor(p))
	}
	if exp := exporter(context.Background()); exp != nil {
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	provider = sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(provider)
}

// Shutdown 把缓冲区里的 span 发完。进程退出前调用（main 里 defer）。
func Shutdown(ctx context.Context) error {
	if provider == nil {
		return nil
	}
	return provider.Shutdown(ctx)
}

// exporter 只在配了端点时创建；没配 = 不导出（span 仍然生成，日志照旧能串树）。
func exporter(ctx context.Context) sdktrace.SpanExporter {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		// 配错了不该让进程起不来：退回「只生成不导出」，日志里能看见降级原因。
		otel.Handle(err)
		return nil
	}
	return exp
}

func newResource() *resource.Resource {
	name := os.Getenv("OTEL_SERVICE_NAME")
	if name == "" {
		name = "szrw-service"
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(name)))
	if err != nil {
		return resource.Default()
	}
	return res
}

// Provider 返回已安装的 TracerProvider（未安装时先安装），供挂额外的 exporter/processor。
func Provider() *sdktrace.TracerProvider {
	Install()
	return provider
}

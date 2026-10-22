package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"

	"github.com/lhdbsbz/szrw/core"
	"github.com/lhdbsbz/szrw/gateway"
	"github.com/lhdbsbz/szrw/health"
	"github.com/lhdbsbz/szrw/metrics"
	"github.com/lhdbsbz/szrw/obsdb"
	"github.com/lhdbsbz/szrw/observe"
	"github.com/lhdbsbz/szrw/tracing"
	"github.com/lhdbsbz/szrw-template/gen/wiring"
)

// main 只做三件事：注册服务、挂 HTTP 网关、跑起来。
// 所有「哪个服务、哪个路由」的信息都在生成物里，这里没有一个服务名——
// 加一个域不需要改这个文件。
func main() {
	// SIGINT/SIGTERM → 取消 ctx → gRPC GracefulStop、后台循环、HTTP 退出。
	ctx, stop := core.ShutdownContext()
	defer stop()

	// 1) 注册全部进程内服务；各服务用 start(...) 挂的后台循环在末尾统一放行。
	wiring.BootAll(ctx)

	// 2) HTTP 网关：JSON 口径、流式直出、路由装配都由框架固定，
	//    项目只决定「成功响应长什么样」——这里保持原样返回 proto JSON。
	//    想套统一信封 {code,message,data}，就给 Options.RewriteResponse。
	mux := gateway.New(gateway.Options{
		Register:  func(m *runtime.ServeMux) { wiring.RegisterGateways(ctx, m) },
		Streaming: wiring.StreamingGatewayMethods,
	})

	// 调用链路的配套设施由框架给默认实现：trace 传播 + 每次 RPC 一条结构化日志 + RED 指标。
	// observe.Handler 负责 HTTP 入口这一段：沿用上游 X-Trace-Id，没有就新生成，并回写响应头。
	// 产品要自己的身份信封或日志落盘策略，用 core.SetPropagator / core.AddObserver 覆盖。
	observe.Install()

	// è§æµåºï¼æ¬æ¬¡è¿è¡ç span + æ¥å¿åæ¬å° SQLiteï¼éå¯æ¢æ°åºï¼ä¸éè¦ schema è¿ç§»ï¼ã
	// å®å¿é¡»å»ºå¨ tracing.Install ä¹åï¼provider æ tracer åç¼å­ processorï¼æäº span å¨ä¸¢ã
	obs, err := obsdb.Open(obsdb.Options{Dir: "logs"})
	if err != nil {
		log.Printf("è§æµåºä¸å¯ç¨ï¼ä»ä¿ç stdout æ¥å¿ä¸ /metrics: %v", err)
		tracing.Install()
	} else {
		defer obs.Close()
		tracing.Install(obs.Processor())
		slog.SetDefault(slog.New(obs.Handler(slog.Default().Handler())))
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracing.Shutdown(shutdown)
	}()
	metrics.Install()
	// 调用方一元调用的默认截止时间（流不封顶：SSE 可能持续几分钟）。
	core.SetDefaultTimeout(10 * time.Second)

	// 运维面与业务面分开挂：探针/指标不是 RPC，不进网关。
	root := http.NewServeMux()
	root.Handle("/healthz", health.Handler())
	root.Handle("/metrics", metrics.Handler())
	root.Handle("/", observe.Handler(mux))

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Println("http://localhost:8080/grpc-gateway/GreeterService/SayHello")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

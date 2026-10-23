# szrw

框架的**运行时契约**：生成物要求的那几个符号，以及所有项目都必须一致的机制。

只放「两边必须一致才不出错」的东西，不放业务、不放驱动、不放日志后端。

```
core/       GlobalRegistry、MaxMsgSize、四个拦截器、span 生成与传播、默认超时、ShutdownContext
gateway/    grpc-gateway mux 装配：唯一 JSON 口径、流式直出、路由挂载
observe/    trace id + span id 日志（slog）+ HTTP 入口根 span
tracing/    OpenTelemetry TracerProvider；配 OTEL_EXPORTER_OTLP_ENDPOINT 才导出
metrics/    RED 指标（次数/耗时），Prometheus 文本格式，带 Go 运行时指标
obsdb/      本次运行的 span + 日志写本地 SQLite（重启换新库，单库 500MB×3 轮转）
health/     存活/就绪探针：不注册检查=存活，注册了=就绪（MySQL/Redis 断了就 503）
log/        slog 门面：级别/输出装配、WithContext 带 trace 字段、JSON 安全序列化（大二进制折叠、深度封顶）
webstatic/  SPA/静态站点 Handler：缓存头矩阵、路径穿越拒绝、ETag/304、SPA 回落规则
lock/       分布式互斥锁协议（不绑驱动）：token + Lua 校验释放、ttl/3 续期
sqlx/       只用于日志的 SQL 参数内联与头尾截断，不执行 SQL
```

## 追踪走标准，不用自造格式

- 传播：W3C `traceparent` / `tracestate` / `baggage`（任何 OTel SDK、服务网格、网关都认）
- span：拦截器自动生成，`rpc.system`/`rpc.service`/`rpc.method`/`rpc.grpc.status_code` 是标准属性
- 日志：每条带 `trace_id` / `span_id` / `parent_span_id`，**不依赖任何后端就能把树拼回来**
- 导出：设 `OTEL_EXPORTER_OTLP_ENDPOINT` 就发给 Jaeger/Tempo/任意 OTLP 后端；不设就只生成不导出
- 进程内调用（loopback）不起 client span、不记 caller 日志：模块化单体没有网络跳，
  服务端那条已经说完了同一件事；跨进程调用才两侧各一条

所以「落地到哪个后端」是项目设一个环境变量的事，不是框架替它选。

## 为什么要有这个仓

生成物（`gen/local_service_center/*.go`）里写着：

```go
grpc.ChainUnaryInterceptor(core.MetadataCalleeInterceptor)
core.GlobalRegistry.RegisterAddr(...)
grpc.MaxRecvMsgSize(core.MaxMsgSize)
```

这段代码在**每个项目**里都会生成。它依赖的符号如果由各项目自己实现，就会出现
「同一个生成物契约、两份实现、语义不同」——模板曾经就是 service 的简化版，
`metadata.go` 169 行 vs 67 行，跨进程时 trace 直接丢。所以这些符号只能有一份。

## 一行就能用

```go
observe.Install()   // 每次 RPC 一条结构化日志（trace_id + span_id）
tracing.Install()   // W3C 传播 + TracerProvider；在 main 里 defer tracing.Shutdown(ctx)
```

模板就是这么用的：`curl` 一次，调用方与服务端两条日志带同一个 trace_id、不同 span_id，
而且 `parent_span_id` 把两层串起来。

## 扩展点

框架只管**什么时候**做，内容由项目决定：

```go
// core：跨进程传播什么（通常是自己的调用主体结构）
core.SetPropagator(myPropagator)

// core：访问日志怎么写、写到哪（可以叠加多个：日志、指标各一个）
core.AddObserver(myObserver)

// core：调用方一元调用的默认截止时间（0=不补；流不封顶）
core.SetDefaultTimeout(30 * time.Second)

// metrics：内置指标名不想用？自己实现 Observer 再 AddObserver
metrics.Install() // szrw_rpc_requests_total / szrw_rpc_duration_seconds

// gateway：成功响应长什么样（信封? 原样?）、错误怎么写、哪些响应头出去
gateway.New(gateway.Options{
    Register:        func(m *runtime.ServeMux) { wiring.RegisterGateways(ctx, m) },
    Streaming:       wiring.StreamingGatewayMethods,
    RewriteResponse: myEnvelope,        // 不设 = 原样返回 proto JSON
    WriteError:      myErrorWriter,     // 不设 = 标准 gRPC→HTTP 状态
    OutgoingHeader:  myHeaderMatcher,   // 不设 = 响应 metadata 一律不外泄
})
```

都不设置时：不传播、不记录、原样返回、标准错误码——所以 `go run .` 起来一个演示
服务不需要任何配置。

网关里**固定不变**的东西：JSON 口径（camelCase + EmitUnpopulated + 枚举用名字 +
忽略未知字段）、纯 server-streaming 按 SSE 直出（不套响应改写）、trace 只在调用方
拦截器里传播（网关不重复写 metadata）。

## 进程级的三件事

```go
ctx, stop := core.ShutdownContext()      // SIGINT/SIGTERM → 取消；GracefulStop、后台循环都靠它
defer stop()

metrics.Install()                        // RPC 指标 + Go 运行时指标
srv.Handle("/metrics", metrics.Handler()) // 只给内网/反代白名单

health.Register("mysql", func(ctx context.Context) error { return db.Ping() })
srv.Handle("/healthz", health.Handler())
```

以前这三件事各项目自己写一遍，写漏的后果分别是：SIGTERM 切在途请求、出事时没有
QPS/延迟曲线、探针只能探测「进程还活着」。

## 不做的事

- 不定义调用主体的形状（那是项目自己的结构）
- 不定义日志后端与采集：`log` 只包标准库 slog，格式开关是门面的选项，落盘/轮转/采集器归项目
- 不碰 ORM / MQ / 配置（`sqlx` 只把 SQL 参数内联进日志，不执行 SQL）
- 不提供启动骨架：`wiring.BootAll` 已经保证了后台循环的放行时机，启动顺序里
  其余步骤（配置、基础设施、业务预热）都是项目事实

## 代码生成器（同仓）

生成器产出的 Go 代码引用本模块的符号（`core.Xxx`），所以两者不再分开版本：

```
cmd/protoc-gen-szrw-codegen/   protoc 插件（一个二进制，按 registry/frontend_api 分派）
internal/{config,registrygen,frontendgen,validate,naming,httpx}/
scripts/generate.sh            项目侧唯一要调的一行（生成 + gen/ 原子替换 + 规则校验）
scripts/build-host.{ps1,sh}    发布链的宿主层（dot-source）：attestation、builder 镜像、三个固定挂载、load/push
scripts/frontends.sh           前端站点谓词的 shell 实现（读 gen/frontends.json）
scripts/build-builder.sh       构建工具链镜像
Dockerfile.builder             Go + Node + protoc 的唯一来源
```

`generate.sh` 的固定部分写死在框架里（proto 根 = `proto/`，第三方 = `proto_third/`，
产物 = `gen/`，五个插件与固定选项），项目只决定「什么时候跑」和 `codegen.yaml` 里的
`registry` / `frontend_api.targets`。

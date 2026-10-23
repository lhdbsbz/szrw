# AGENTS.md — szrw

框架的运行时契约。**只放「所有项目必须一致才不出错」的机制。**

## 一句话判据

> **机制由框架提供，机制里存放什么东西由项目决定。**

- 机制 = 什么时候传播、走哪个通道、什么时候发事件、事件有哪些字段、网关怎么装配
- 内容 = 传播的载荷是什么、日志写成什么样、响应信封长什么样

所以本仓对外只有三类东西：

1. **契约**（不可替换）：`core.GlobalRegistry`、`MaxMsgSize`、`TraceKey`、四个拦截器、
   `core.StartSpan`（span 身份与传播格式）、`core.ShutdownContext`，以及 `gateway.New`
   固定的 JSON 口径与 SSE 直出。生成物直接引用它们。
2. **扩展点**（内容入口）：`core.Propagator`、`core.Observer`（可叠加）、`core.SetDefaultTimeout`、
   `gateway.Options` 的三个函数、`health.Register`。
3. **参考实现**（可整体替换）：`observe`（trace + 日志）、`metrics`（Prometheus 指标）。
   它们是「新项目什么都不写也能跑」的默认内容，不是框架的一部分；产品用自己的实现时
   它们不是被配置，而是被忽略。`health` 的 handler 就是契约（探针形状必须一致）。

## 进不进这里

1. 生成物直接引用它吗？→ 契约
2. 两端不一致会静默出错吗？→ 契约
3. 只影响本产品的取舍吗？→ **不要**，做成扩展点或留给项目

## 硬约束

- **不 import 任何业务 proto**。`Propagator.Encode/Decode` 的载荷类型由项目定义。
- **追踪只用标准协议**：W3C `traceparent`/`baggage`，不自定义头名与编码。
  日志里的 `trace_id`/`span_id`/`parent_span_id` 来自 span 本身，不另生成一套。
  「发到哪个后端」由 `OTEL_EXPORTER_OTLP_ENDPOINT` 决定，框架不内置 Jaeger/Tempo 依赖。
- **进程内调用只留一份证据**：目标地址是 loopback 时，caller 侧不起 span、不记日志
  （没有网络跳，服务端那条就是它）。同一个事实不说两遍；跨进程调用才两側都有。
- **不引入第三方 logger / ORM / MQ / 配置**。`Observer` 只把事件交出去；`observe` 用标准库 slog，`log` 是 slog 之上的门面（不引依赖），`sqlx` 只把 SQL 参数内联进日志（不执行 SQL）。
- **配套设施要给默认实现，不能只留空钩子**。`observe.Install()` + `metrics.Install()` +
  `health.Handler()` 三行就要能让新项目拿到 trace、日志、指标、探针；产品想换成自己的
  再用钩子覆盖——「提供默认」与「强制唯一」是两件事。
- **默认超时只补一元调用**。流（SSE/WS 桥）的生命周期由业务决定，一刀切会切断正常长连接；
  默认值 0（不补），由项目显式设置——超时时长是产品取舍，不是框架事实。
- **观测库只活一次运行**：`obsdb` 启动删旧库、新建；运行内单库 500MB 轮转、本轮最多 3 个；
  重启即换新库，所以**没有 schema 迁移**——加字段直接改 `schema` 常量。写永不阻塞业务
  （异步批，队列满即丢观测数据）。
- **processor 只在 provider 构造时传**：`tracing.Install(obs.Processor())`。
  `TracerProvider` 按 tracer 名缓存 processor 列表，晚注册的这条链路 span 会全丢。

- **网关只固定跨端一致的 JSON 口径与流式直出**，信封/错误码/响应头一律是 `gateway.Options` 的钩子。
- **不提供启动骨架**。`wiring.BootAll` 已经固定了「后台循环在 HTTP 之前解锁」这一条；
  其余启动顺序（配置 → 基础设施 → 业务预热）是项目事实，塞进来就是过度设计。
- 包名保持 `core`：生成的代码用 `core.Xxx` 引用，改名等于让所有项目的生成物失效。
- 版本策略：`@latest`，不锁补丁版本；破坏性改动直接改，下游重新生成。

## 改动后必须验证

```bash
go build ./... && go vet ./...

# 用真实项目验证（生成物会引用本包的符号）
cd ../service && ./build.ps1 -ProtoOnly
cd template && ./scripts/gen.ps1 && go build ./...
```

## 生成器与运行时分不开

- 生成器模板里出现 `core.Xxx` 时，改这个符号必须**同一次提交**改 `internal/registrygen`；
  分开发版本会产出编译不过的代码，而错误信息不会指向本仓。
- `scripts/generate.sh` 是项目侧唯一入口，别让项目自己拼 protoc 参数；proto 规则只写在
  `internal/validate`（构建期失败），不要退化成项目里的测试。
- 工具链镜像只有 `Dockerfile.builder` 一份，项目不自己定义镜像。

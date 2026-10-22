# szrw-service-template

**框架的最小可运行项目**：一个 proto 服务、一次生成、`go run .` 就能被 HTTP 调用。
用它看清「哪些是框架给的，哪些必须自己写」。

```bash
./scripts/gen.ps1        # 生成 gen/ 与 microservice/ 骨架（容器里跑 protoc）
go run .                 # http://localhost:8080

curl -X POST localhost:8080/grpc-gateway/GreeterService/SayHello -d '{"name":"szrw"}'
# {"message":"hello, szrw"}
```

## 一个项目需要哪些文件（按「谁负责」分类）

| 文件 | 谁生成 | 说明 |
| --- | --- | --- |
| `proto/**/*.proto` | **你写** | 唯一的契约来源。`google.api.http` 注解决定 HTTP 路由 |
| `codegen.yaml` | 你写 | 只描述生成物的位置与形状；默认值就是标准布局 |
| `gen/proto/**` | protoc 官方插件 | `--go_out` / `--go-grpc_out` / `--grpc-gateway_out` |
| `gen/local_service_center/` | `protoc-gen-szrw` | 每服务一个注册文件 + 客户端获取 |
| `gen/wiring/wiring_gen.go` | 同上 | `BootAll`（注册全部服务）+ `Gate`（启动放行）+ `RegisterGateways`（路由） |
| `gen/startup/startup.go` | 同上 | 启动钩子契约（只有类型） |
| `microservice/<域>/rpc.go` | 同上（`--scaffold` 时补缺失） | `var Service` / `Main` / `ServiceServer` |
| `microservice/<域>/<rpc_snake>.go` | 同上（缺失时） | 一 rpc 一个文件，签名 + `codes.Unimplemented` |
| `main.go` | **你写**（约 3 句） | 只做：`BootAll` → `RegisterGateways` → `http.Server` |
| `scripts/gen.sh` / `gen.ps1` | 你写（一次） | protoc 调用，见「生成」一节 |

**手写业务只有 `microservice/<域>/*.go` 里被脚手架生成过的那些函数体。**

## 校验：没有脚本

proto 契约规则住在生成器里（缺省开启，没有开关），
这个仓**没有**任何校验脚本，也不需要写：

| 规则 | 谁拦 |
| --- | --- |
| 目录布局（`{dir}.proto` + `{dir}_core_model.proto`、`common/` 只能 `common.proto`） | 生成器 |
| 一个文件一个 service；core model 不声明 service | 生成器 |
| service proto 不 import 另一个 service proto；core 不 import service | 生成器 |
| `google.api.http` 必须等于 `<http_route_prefix>/{Service}/{Rpc}` | 生成器 |
| 整数字段只能是 `int64`（JSON 里才会统一是字符串） | 生成器 |

项目特有的约定（`*Id` 命名、分页 DTO、某客户端字段冻结）才需要自己写测试。

## 生成

前置：`protoc` + `protoc-gen-go` / `protoc-gen-go-grpc` / `protoc-gen-grpc-gateway` /
`protoc-gen-szrw` 在 `PATH`。

```bash
go install github.com/lhdbsbz/szrw/cmd/protoc-gen-szrw@latest
./scripts/gen.sh
```

本机没有 protoc 时用容器（`scripts/gen.ps1` 就是这么做的）：

```powershell
.\scripts\gen.ps1 -Codegen D:\workspace\szrw
```

兄弟仓（都需与本仓同级）：

- `../szrw`：生成器 + proto 规则 + protoc 调用层

`scripts/gen.sh` / `gen.ps1` 只是一层壳：真正干活的是框架仓的
`../szrw/scripts/generate.sh`（插件、protoc 选项、proto 契约校验、`gen/` 原子替换）。
加 RPC 时传 `--scaffold`（ps1: `-Scaffold`）；不传就是 check —— 缺实现即失败。

`scripts/gen.ps1` 用的是**框架工具链镜像** `szrw-builder:latest`（Go + Node + protoc + 官方插件 +
staticcheck/deadcode/govulncheck），定义在 `szrw/Dockerfile.builder`，先跑一次：

```bash
../szrw/scripts/build-builder.sh
```

生成器本身从 `-Codegen` 指的源码目录 `go install`，所以改生成器不必先发布。

`codegen.yaml` 只有两段（段存在 = 生成），**没有任何布局开关**：`registry: {}` + 可选
`frontend_api.targets`。加 RPC 时模板脚本传 `scaffold=write` 补骨架；CI 不传，缺实现即失败。

## 加一个域

```bash
# 1) 写 proto/greeter2/greeter2.proto（service + rpc + google.api.http）
# 2) 生成
./scripts/gen.sh
# 3) 填 microservice/greeter2/<rpc_snake>.go 里的函数体
```

不需要改 `main.go`、不需要改 `codegen.yaml`、不需要写 `wiring`：
生成器会补 `microservice/greeter2/rpc.go` 与每个 rpc 的骨架文件，并把注册与路由接上
（`--scaffold`，只补缺失、不覆盖）。

## 运行时契约从哪里来

`gen/local_service_center/*.go` 里引用的这几个符号由 **`github.com/lhdbsbz/szrw/core`**
提供（`github.com/lhdbsbz/szrw`；本地由工作区 `go.work` 指向工作副本）：

| 符号 | 用途 |
| --- | --- |
| `core.GlobalRegistry` | 服务名 ↔ 地址/客户端的进程内注册表 |
| `core.MaxMsgSize` | 单条 gRPC 消息上限（收发都按它） |
| `core.Metadata{Caller,Callee}Interceptor` | 一元调用的身份/追踪传播 |
| `core.MetadataStream{Caller,Callee}Interceptor` | 流式调用的同一件事 |

这些都**不是项目代码**：不要在本仓再写一份。传播什么、日志怎么写，由项目通过
`core.SetPropagator` / `core.AddObserver` 决定；本模板什么都没设置，用的是框架
`observe.Install()` 给的默认实现（trace id + 每次 RPC 一条结构化日志）。

### 运维面（本模板已经接好，别另写一套）

| 端点 / 调用 | 谁提供 | 作用 |
| --- | --- | --- |
| `GET /healthz` | `health.Handler()` | 存活；`health.Register("mysql", ping)` 后变就绪探针（断了 503） |
| `GET /metrics` | `metrics.Handler()` | RED 指标 `szrw_rpc_requests_total` / `szrw_rpc_duration_seconds`，含 Go 运行时指标 |
| `observe.Install()` | 框架 | 每次 RPC 一条日志（`trace_id` + `span_id`） |
| `obsdb.Open()` | 框架 | 本次运行的 span + 日志写本地 SQLite（`logs/obs-*.db`），重启换新库 |
| `tracing.Install()` | 框架 | W3C `traceparent` 传播 + TracerProvider；`OTEL_EXPORTER_OTLP_ENDPOINT` 决定导不导出 |
| `core.ShutdownContext()` | 框架 | SIGINT/SIGTERM → 优雅停机 |
| `core.SetDefaultTimeout(d)` | 框架 | 一元调用的默认截止时间；流不封顶 |

`/healthz`、`/metrics` 挂在网关**外**（它们不是 RPC）；`/metrics` 只给内网或反代白名单。

## 目录

```
proto/                契约（common + greeter：{dir}.proto 写接口，{dir}_core_model.proto 写实体）
proto_third/          protoc 需要的第三方 proto（google/api）
gen/                  生成物，可整体删除重建
microservice/greeter/ 实现：rpc.go + 一 rpc 一个文件
pkg/                  运行时（待抽成 szrw）
scripts/              生成脚本
main.go               装配入口
```

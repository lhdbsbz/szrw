# AGENTS.md — szrw-service-template

框架的最小可运行项目。**手写只有三样：proto、`codegen.yaml`、`main.go`；其余全是生成物或骨架。**

**怎么加**（RPC / 表 / 页面 / 站点 / MQ、验收清单、静态检查）：见框架规则 [`../docs/framework.md`](../docs/framework.md)。本文件只写这个模板自己的事实。

## 命令

```powershell
.\scripts\gen.ps1     # 生成（容器里跑 protoc；宿主只需 Docker）
go run .              # http://localhost:8080
.\build.ps1 -SkipPush # 生成 + 交叉编译 + 打包镜像（不推送）
curl -X POST localhost:8080/grpc-gateway/GreeterService/SayHello -d '{"name":"szrw"}'
```

发布链只写项目段（四个事实：镜像变量 / bake 文件 / bake target / 是否多站点 staging）+ 一次 `Invoke-SzrwBuild`（shell 是 `szrw_invoke_build`），机制在 `../szrw/scripts/build-host.*`：
别再往 `build.ps1` 里抄「关 attestation、确保 builder 镜像、docker run 挂载、bake 推送」。

`.\scripts\gen.ps1` 默认从同级 `..\szrw` 装插件（`-Codegen` 可改），所以改生成器不必先发布。
宿主有 protoc 时可直接 `bash scripts/gen.sh`。

## 死线

- **框架已吸纳的机制直接用**：`szrw/log`（slog 门面 + 安全 JSON）、`szrw/webstatic`（站点 Handler）、`szrw/lock`（锁协议）、`szrw/sqlx`（SQL 日志内联）。本仓自己的横切工具放 `pkg/`；驱动适配器也在项目侧（框架定协议，项目给驱动）。
- **`gen/` 是生成物**：可整体删除重建，任何手改都会在下次生成时丢失。
- **不要手写 `microservice/<域>/rpc.go` 和 `<rpc_snake>.go`**：`--scaffold` 会补，而且一 rpc 一文件的规则由生成器强制。
- **运行时契约在 `szrw`**（普通模块依赖；本地由工作区 `go.work` 指向工作副本，CI 用 `go.mod` 的伪版本）：本仓不提供 `core`，也不要自己实现拦截器。
- `proto_third/google/api/*.proto` 是 protoc 的 include 依赖，升级 grpc-gateway 时一起换。
- **校验不要再写脚本**：proto 契约规则（布局/import 方向/HTTP 路由/整数宽度）由生成器的 validate 负责，这个仓不该出现校验 bash/脚本。项目特有的命名约定才写测试，放 `contracts/`（`go test ./contracts`），模板自带一条 ID 口径守卫。
- 生成失败时 `gen/` 会被自动还原（先挪到 `gen.prev` 再生成），所以坏 proto 不会把仓删空。
- **改这个模板之前先问**：它是「最小」模板，不是另一个产品。加能力要能回答「新项目真的需要它吗」。
- `scripts/gen.ps1` 必须是 **UTF-8 with BOM**：PowerShell 5.1 按 ANSI 解码无 BOM 的中文注释会解析失败。

## 哪些还能收进框架

1. `proto_third/google/api/*`（505 行，每个项目都复制一遍）→ 发布成可 `-I` 的模块或随插件分发。
2. `proto/common/common.proto`（`Empty` + 分页）→ 分页是框架约定，消息却定义在每个项目里。

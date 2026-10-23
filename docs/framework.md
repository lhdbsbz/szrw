# 框架规则（跨项目不变）

**唯一真相**：从 `template/` 起步的下一个项目照着做，不用另找文档。这里只写换项目依然成立的事。

| 层 | 位置 | 只写什么 |
| --- | --- | --- |
| 框架规则 | 本文件 | 生成链、目录落点、ID/分页口径、事务与事件、运行时契约、前端接法、构建发布、验收门槛 |
| 项目事实 | 该仓 `AGENTS.md` | 这个仓怎么跑、有哪些例外、基线、债务 |
| 行为契约 | 该仓 `docs/**` | 域边界、数据归属、字段取值 |

**判据**：这条规则换个项目还成立吗？成立 → 本文件；只在某个仓成立 → 该仓 `AGENTS.md`；描述系统现在怎么工作 → `docs/`。同一事实只写一处；两处不一致时以代码与生成器为准，再回来改这里。

## 1. 心智模型

一个业务能力 = **一个 proto RPC**（唯一源）→ 生成 Go/TS/JS 客户端与装配代码 → 实现在 `microservice/<域>` → 前端只调生成的客户端。

```
proto/**/*.proto ──scripts/generate.sh──┬─> gen/proto/**              go / go-grpc / go-json / grpc-gateway
                                        ├─> gen/local_service_center/**  Register<X>Service / Get<X>Service()
                                        ├─> gen/wiring/wiring_gen.go     BootAll / Gate / RegisterGateways
                                        ├─> gen/startup/startup.go       启动钩子类型（startup.Func）
                                        ├─> gen/frontends.json          前端站点清单（构建脚本读它）
                                        └─> codegen.yaml 的 frontend_api.targets 目录
                                            <serviceLower>Api.{ts,js} + grpcGatewayHelper.{ts,js}
microservice/<域>/   var Service + Main(ctx, start) + 一 rpc 一个文件
前端                 生成的 Api 对象 -> 项目请求层 -> 页面
```

**加 RPC、加域都不改 `main.go`，也不写任何注册代码**：装配是生成物。

手写只有三样：proto、`codegen.yaml`、`main.go`（约三句：`BootAll` → `RegisterGateways` → `http.Server`）。

需要读源码时看这里（别猜）：

| 机制 | 位置 |
| --- | --- |
| proto 契约规则（布局 / import / 路由 / 整数宽度） | `internal/validate` |
| 注册文件、`BootAll`/`Gate`/`RegisterGateways`、骨架 | `internal/registrygen`（骨架见 `scaffold.go`） |
| 前端客户端与 helper 生成 | `internal/frontendgen`（模板在 `runtime/`） |
| `codegen.yaml` 的 schema 与唯一常量 | `internal/config` |
| protoc 调用层、`gen/` 原子替换 | `scripts/generate.sh` |
| 身份传播 / span / 观察点 / 注册表 / 停机 | `core` |
| 网关 mux 与 JSON 口径 | `gateway` |
| 日志门面 / 安全 JSON | `log` |
| 静态站点与 SPA 的缓存头、路径安全 | `webstatic` |
| 分布式锁协议（驱动无关） | `lock` |
| SQL 参数内联（只用于日志） | `sqlx` |

## 2. 目录、命名与什么进框架

### 2.1 吸纳判据

框架只收「所有项目必须一致才不出错」的机制。三条判据里第一条是硬约束，后两条任一成立就该收：

| 判据 | 说明 |
| --- | --- |
| 不拥有被引用的类型 | 不 import 业务 proto，不 import ORM / 驱动 / 供应商 SDK；参数与返回值是接口或标准库类型，换驱动不用改框架 |
| 只有一种正确实现 | 两种写法里必有一处错（trace 传播、JSON 口径、缓存头、锁释放顺序），「各写一份」本身就是 bug |
| 已经有人在重复，或框架自己写错 | 两份实现就收一份；框架里的错必须在一处修，否则每个项目重付一次 |

红线（命中任一条就不收）：

- **配置一键改行为的策略**：重试次数、默认超时、日志级别与格式、轮转保留几天。做成 `Options` 或钩子（项目在装配时给），不做 `if config.xxx`。
- **绑供应商 / 拓扑 / 凭证**：Redis 客户端、ORM、MQ、对象存储、OTLP 后端、连接串。框架不 import 它们，也不假设单机还是集群。
- **只服务单一产品**：SSO 白名单、业务字段、某个后台页面。产品事实留在项目仓。

驱动绑定的东西靠接口适配：框架定协议，项目给适配器。`lock.Client` 是范例——框架只要求 `SetNX` 与 `EvalInt` 两个方法，go-redis 适配器在项目里，框架不 import 任何 Redis 库。

### 2.2 目录三态

服务的唯一判据是 **proto 里有 `service` 声明**；目录名、包名、有没有 `var Service` 都不算。

| 形态 | 判据 | 规则 |
| --- | --- | --- |
| 真服务 | `proto/<目录>/*.proto` 声明了 `service` | 实现目录与 proto 目录同名；`var Service` + `Main(ctx, start)`；一个 rpc 一个文件；service 名全局唯一 |
| 服务内实现子包 | 位于某个服务目录内部（更深一层） | 不注册服务、没有 `Main`；可以有 `var Service` 当命名空间；同服务可以直接 import |
| 命名空间 | 只做分组、自身不声明 service 的一层 proto 目录 | 子目录各自是服务；只允许一层且必须显式登记（登记在项目守卫里）；子目录的服务名不得重复 |

- 命名空间不是「目录分类」，是登记过的事实；当前使用方只有 `open_api`，其服务名带 `Open` 前缀（`OpenMemoryService`），避免与进程内同名服务打架。
- 四条机械检查（正向：每个 proto service 有实现；反向：每个一层实现目录要么是服务、要么是命名空间；命名空间白名单；service 名唯一）由使用方项目的 `contracts` 守卫做；生成器另一侧只查跨域 import 与被直接 import 的子包是否注册了服务。两边都不靠 review。
- 目录漂移是渐进的：先把「模块」挂在别的服务下，然后一半 RPC 变成一行转发。所以判定要写成测试。

### 2.3 已吸纳的包

| 包 | 一行用法 |
| --- | --- |
| `log` | `log.Init(log.Options{Level: lvl, JSON: true, Out: w, AddSource: true})` 装配全局 slog；`log.WithContext(ctx)` 自动带 `trace_id`/`span_id`/`parent_span_id`；`log.JSON(v)` 安全序列化（`[]byte` 折成 `<N bytes>`，深度/长度封顶）。级别过滤走 `Handler.Enabled`，`log.Level()` 运行期可改。 |
| `webstatic` | `webstatic.Handler(webstatic.Options{FS: siteFS, SPA: true})` 服务一个站点产物目录：html `no-store`、`/assets/**` immutable、其余 `must-revalidate` + ETag/304；路径穿越 404 且不回落；SPA 回落只对无扩展名、非 asset 的路径。 |
| `lock` | `lock.New(client, key, ttl)` + `TryLock`/`Lock`/`Unlock`/`Extend`，`lock.Once` 是抢占一次的形式；token + Lua 校验释放、ttl/3 续期；未持有 `ErrNotHeld`，持有期间丢了 `ErrLostLock`。 |
| `sqlx` | 只用于日志与排障：`sqlx.Inline(query, args)` 把 `?` 换成可复制的参数，`sqlx.Truncate(s, max)` 头尾保留截断。它不执行 SQL，也不做 ORM。 |
| `scripts/build-host.{ps1,sh}` | 发布链的宿主层：项目脚本 dot-source 它，只给四个事实（镜像变量、bake 文件、bake target、是否多站点 staging）再调用一次；attestation、builder 镜像、三个固定挂载、load/push 在壳里（细节见 §8）。 |

包一旦吸进来，项目不再各写一份；驱动/供应商相关的部分留在项目侧适配器里。

### 2.4 目录与命名的落点

| 路径 | 谁写 | 规则 |
| --- | --- | --- |
| `proto/<域>/<域>.proto` | 你写 | service + `Req`/`Resp`；一个文件一个 service |
| `proto/<域>/<域>_core_model.proto` | 你写 | 实体/数据结构；**每个 proto 目录必须有这个文件**（service proto 可选） |
| `proto/common/common.proto` | 你写 | `common/` 只能有这一个文件，放 `Empty`、分页那类跨域消息 |
| `proto_third/**` | 你带 | protoc 的 include 依赖（`google/api/*`），升级 grpc-gateway 时一起换 |
| `codegen.yaml` | 你写 | 只描述生成物位置与形状；没有布局开关 |
| `gen/**` | 生成 | 可整体删除重建，永不手改 |
| `gen/local_service_center/<域>.go` | 生成 | `Register<X>Service(ctx, svc)` / `Get<X>Service()` |
| `gen/wiring/wiring_gen.go` | 生成 | `BootAll` / `Gate` / `RegisterGateways` / `StreamingGatewayMethods` |
| `microservice/<域>/rpc.go` | 生成一次 | `var Service` / `Main(ctx, start)` / `ServiceServer`，之后归你 |
| `microservice/<域>/<rpc_snake>.go` | 生成一次 | 一 rpc 一个文件，文件名 = rpc 名的 snake_case |
| `microservice/<域>/<子包>/` | 你写 | 服务内实现子包：不注册服务、无 `Main`；服务可以 import 自己的子包 |
| `proto/<命名空间>/<子域>/` | 你写 | 仅限登记过的命名空间（如 `open_api`）；子目录各自是服务 |
| `gen/frontends.json` | 生成 | 前端站点清单；构建脚本读它，别在项目脚本里另写一套探测 |
| `web/<站点>/` | 你写 | 目录名 = 子域名 = 产物目录名 |
| `mobile/<产品键>/` | 你写 | 目录名 = 产品键（与 `web/<子域名>` 同键），不是技术栈名 |

- 实现目录由 `go_package` 推导：`<module>/gen/proto/<相对路径>` → `microservice/<相对路径>`；骨架按目录名写包名。
- 框架自带的名字是契约，不改：`gen/`、`gen/wiring`、`Register<X>Service`/`Get<X>Service`、`<x>Api`、`grpcGatewayHelper`、`pageReq`/`asEntityId`/`i64`。
- 不造分层目录（`handler/ service/ repository/ dto/`）：一个域一个包，能力按文件切。

## 3. 怎么加

### 3.1 一个 RPC

1. 在 `proto/<域>/<域>.proto` 的 service 里加 `rpc X(XReq) returns (XResp)`；对外接口写 `google.api.http`，路径必须 `/grpc-gateway/<Service>/<X>`（内部接口**不加**注解，只走进程内调用）。
2. 跑一次生成并补骨架：`scripts/gen.ps1 -Scaffold`（等价 `generate.sh --scaffold`）。缺实现时默认是**失败**，只有显式 `--scaffold` 才补文件。
3. 生成器补出 `microservice/<域>/<x_snake>.go`（签名 + `codes.Unimplemented`）。填函数体，一行说明注释删掉。
4. 前端：`frontend_api.targets` 里的目录会多出 `<serviceLower>Api` 的一个方法；类型由项目的 ts-proto 生成（不在本仓）。
5. 同步文档：该域 `docs/<域>/README.md` 的流程/字段；对外 API 面（字段、分页、错误语义）变了，同步客户看得见的那份 API 文档（项目侧）。
6. 验证见 §9。

### 3.2 一张表 / 一个字段

1. 实体先落 proto：`<域>_core_model.proto`。ID `int64`、金额 `string`（定点两位）、时间 `int64` 毫秒、状态用本域 `string` 常量；字段编号只增不改、不重用。
2. `models/`（项目侧）加结构体与完整标签；**主键雪花**：建表不带自增，插入前显式赋值（用 xorm 时 `pk` 不带 `autoincr`）。
3. 新字段怎么放：要查询/排序/索引 → 独立列；纯展示 → 复用本域已有的 JSON 扩展位；有独立生命周期 → 新表；只是加速读 → 缓存，且写清回退路径。
4. DDL **人工执行**（交 DBA，全文贴工单或交付说明），仓库里不留可执行 `.sql`。禁止 `Sync2`/`AutoMigrate`。
5. 守卫：项目 `contracts` 里加主键/ID 口径测试；凡例外（自增主键）登记理由，新表不得照抄别人的例外。

### 3.3 一个页面 / 一个站点

1. 页面属于站点：`web/<站点>/src/...`。数据只经生成的客户端（`<serviceLower>Api` 的方法）与生成的类型。
2. 新站点：`web/<name>/`，`package.json` + vite 配置（`outDir` = 站点名）+ `src/api/request.ts`；要登录就在项目后端注册 SSO client（白名单是项目事实）。
3. 生成器只往 `codegen.yaml` 的 `frontend_api.targets` 目录写客户端，**目录必须已存在**（生成器不造目录，也不写到 targets 之外）。
4. 路由与菜单是站点自己的：新页面接 router；管理后台通常还有菜单分组与表格分页组件——那是站点约定，不是框架机制。
5. 一个页面一个 URL；旧链接别名只为已发布链接保留。
6. 移动端同一产品的目录键与 `web/<子域名>` 同键；页面经一层 `services/*` 薄封装调 RPC，不直接 import 生成客户端（项目约定）。

### 3.4 一个域服务

1. `proto/<域>/<域>.proto`（service + Req/Resp）+ `proto/<域>/<域>_core_model.proto`（实体）。
2. `go_package = "<module>/gen/proto/<域>"`；生成器从它推导 module 根与实现目录。
3. `--scaffold` 生成：`gen/local_service_center/<域>.go`、`gen/wiring` 里多一行 `Main` 与网关注册、`microservice/<域>/rpc.go` + 每个 rpc 一个文件。
4. `Main(ctx, start)` 只做同步注册与挂载：注册服务、`DeclareExchanges` 一类拓扑声明、`start("name", loop)` 挂后台循环。**不阻塞、不调别的服务**（`wiring.BootAll` 放行前调用会 panic 杀进程）。
5. 该域 `docs/<域>/README.md`（职责与边界 / 数据归属 / 关键流程与不变量 / 验证入口），新域进 `docs/README.md`。
6. 生成器强制实现目录有 `rpc.go` 与 `type ServiceServer`，且每个 rpc 住在自己名字的文件里。

### 3.5 一个 MQ 事件

1. 事件消息定义在**生产方**的 proto；exchange/routing key 常量集中一处，不散落在 handler 里。
2. 入队与业务写**同一事务**（outbox 行）；禁止在 handler 里直接 publish。
3. relay 负责投递，语义是「至少一次」；消费端必须幂等（业务键去重或状态机条件更新）。
4. 毒消息显式丢弃（返回项目里的 `ErrDropEvent` 一类哨兵）；可重试错误才重投，重投到上限进死信。
5. exchange 在启动早期集中声明；消费循环经 `start(...)` 挂载，由 `wiring.BootAll` 在全部服务注册后放行。
6. 事件只加字段不改语义；消费方要能吃旧事件（回放、积压）。
7. 框架**不带** MQ/outbox 实现：可靠性规则固定，实现（outbox 表、relay、消费封装）归项目，参考实现见使用方项目仓。

### 3.6 落点决策表

| 需求 | 落点 | 关键约束 |
| --- | --- | --- |
| 新增/修改接口 | `proto/<域>/<域>.proto` | 路径 `/grpc-gateway/<Service>/<Rpc>`；内部接口无注解 |
| 新增字段/模型 | `<域>_core_model.proto` + `models/` | 编号只增；ID int64、金额 string、时间 int64 ms |
| 业务实现 | `microservice/<域>/<capability>.go` | 方法挂在 `*ServiceServer`；鉴权第一行 |
| 纯规则/状态机 | 同包 `decision.go`/`protocol.go` | 不碰 DB，表驱动单测 |
| 列表 | 该域 proto 的 `common.PageInfoReq/Resp` + 项目 `utils` 归一 | PAGE 模式必须回 `total` |
| 游标列表 | 该消息自己的锚点字段 + `hasMore`/`next*` | 显式例外，须在项目 contracts 的 allowlist 登记 |
| 跨域读/写 | `local_service_center.Get<X>Service()` + 对方 proto | 事务参与接口显式传 session |
| 服务内实现子包（无 service） | `microservice/<域>/<subpkg>/` | 不注册服务、没有 `Main`；被直接 import 的子包注册服务会被生成器判失败 |
| 异步可靠事件 | 生产方 proto 事件 + outbox + 集中 topic 常量 | 同事务入队；消费幂等 |
| 管理后台页 | 站点 `views/` + router 菜单 | 后台 RPC 用 `Admin` 前缀 + 项目鉴权 |
| 新前端站点 | `web/<name>/` | 目录名 = 子域名 = 产物目录名；要登录就注册 SSO client |
| 新域服务 | `proto/<域>/` 两文件 + `microservice/<域>/` + docs | 导出 `var Service` 与 `Main(ctx, start)` |
| 回调 / WS / SSE | 项目 HTTP 层（网关之外） | 必须注释为什么不能走 RPC；不得借此读写别的域 |
| 改表 | `models/` + DBA 手工 SQL | 无自动迁移；SQL 不进仓 |
| 构建/发布 | 项目构建脚本 / bake / Dockerfile | 只走脚本；用项目自己的镜像变量 |

## 4. proto 契约

生成器在**生成之前**判这些，不满足就是构建失败，没有开关、没有逐目录例外：

| 规则 | 说明 |
| --- | --- |
| 目录布局 | 一个目录只允许 `{dir}.proto` 与 `{dir}_core_model.proto`；`common/` 只能有 `common.proto` |
| core model 必填 | 只有 core model 的目录合法；service proto 可以不写 |
| service 声明 | service proto 必须声明且只声明一个 service；core model、`common/` 不得声明 service |
| import 方向 | service proto 不得 import 另一个 service proto；core model 不得 import 声明了 service 的 proto；`common/` 只能 import `common/` |
| HTTP 路由 | 有注解就必须等于 `<前缀>/{Service}/{Rpc}`；前缀由 proto 位置推导：`proto/<d>/x.proto` → `/grpc-gateway`，`proto/<a>/<b>/x.proto` → `/grpc-gateway/<a>`；不支持 `additional_bindings` |
| 整数宽度 | 任何整数字段只能是 `int64`（`int32`/`uint*`/`sint*`/`fixed*` 以及 map key 一律失败） |

约定（生成器不查，靠 review 与项目守卫）：

- 消息名 `<Rpc>Req` / `<Rpc>Resp`；空请求响应用 `common.Empty`；列表用 `common.PageInfoReq` / `common.PageInfoResp`（字段名、枚举值与注释以模板的 `proto/common/common.proto` 为准，新项目直接复制；框架只规定名字与形状，不生成这两个消息）。
- HTTP 注解一律 `post:` + `body: "*"`（生成器只校验路径；方法与 body 是约定）。
- 后台接口用 `Admin` 前缀；SSE 仅限 `server-streaming` + `google.api.http`。
- `go_package` 必须是 `<module>/gen/proto/<相对路径>`，否则生成器无法推导实现目录。
- 字段编号只增不改、不重用、不改字段名。
- 被不可升级客户端消费的契约（原生插件、已发布固件、冻结协议）**只能加字段**；有冻结测试的，改它等于改法律。

## 5. 数据与接口

**ID 端到端**

| 层 | 口径 |
| --- | --- |
| proto | 实体 ID 及其外键/引用一律 `int64` |
| Go | `int64` 直接赋值，不 `FormatInt` |
| JSON | protojson 自动编成十进制字符串 `"854471262536205312"` |
| TS/JS | ts-proto 生成 `string`；`id/*Id/ids/*Ids` brand 成 `EntityId` |

- 主键用雪花：建表不带自增，`Insert` 前显式赋值（用 xorm 时 `pk` 不带 `autoincr`）。ID 生成若是一次 RPC，必须在 `Begin()` **之前**取。
- 例外只有两类理由站得住：**产品语义**（`id` 本身就是对外编号，代价是插入后才有 id）与**写入路径**（append-only 日志要单调 id、热路径不能多一次 RPC）。例外写进模型注释与架构文档，最好配一条 contracts 守卫把未登记的自增直接判失败。
- 前端收发都是 string：比较用 `i64`/`i64Compare`，展示用 `i64String`，只有分页总数/图表/布局才 `i64ToNumber`；输入用 `asEntityId`。禁止 `Number(id)`/`parseInt(id)`/`+id`（雪花会掉精度）。`EntityId` 的 brand 是可选属性，TypeScript **拦不住**普通字符串，纪律靠人。
- 其它类型：金额是定点两位小数字符串（`"99.00"`，整数运算用分）；时间是 `int64` epoch 毫秒，展示时再转 `Date`；`bytes` 在业务代码里是 `Uint8Array`，base64 编解码由生成的 helper 负责。
- 所有项目自己的整数字段都是 `int64` → JSON 里都是 string，**不要**假设计数字段（`matchCount`/`totalMatches` 一类）是 number 而去做数值运算。

**分页**

- 列表统一走 `common.PageInfoReq/Resp`，禁止自造 `page`/`pageNum`/`pageSize` 字段；游标分页是显式例外（锚点 + `hasMore`/`next*`，登记在项目 allowlist）。
- 归一化只在项目 `utils` 里做一份：`PageParams`（`pageNum<=0→1`、`size<=0→20`）、`PageOffset`、`PageResp`。PAGE 模式必须回 `total`（未分页前的总条数）。
- 前端 `pageReq` 统一构造请求（`pageNum`/`size` 都是 string）；`pageInfo` 可能为 `null`，用可选链兜底。

**错误**

- 用户可读的中文消息，不返回裸 DB 错误、不打敏感信息。
- 错误码只有一套：gRPC status code（网关按 `WriteError` 映射成 HTTP 状态或统一信封）；不自造数字错误码表，也不在 handler 里手写 HTTP 状态。
- 错误口径只有一份：成功/错误响应体由网关的 `WriteError`（以及需要信封时的 `RewriteResponse`）统一决定；进入 RPC 之后不各写一套。
- `return nil, err` 只用于**透传已包装过**的错误；xorm/HTTP/解析等原始错误先包一层。反过来也别把已包好的 status error 再包一次（提示会变成 `rpc error: code = ...`）。

**事务**

- `NewSession → Begin → defer Rollback → Commit`；或 `defer Close` + 逐分支显式 Rollback。不允许既不 defer 也不在错误分支回滚；会话要交给调用方时不得加 defer。
- 不跨服务；**事务内不调 RPC**（最常见的踩法是开事务后再取雪花 ID）。跨域事务用显式参与接口：对方收 session 且不提交。

**跨域**

- 只经 `local_service_center.Get<X>Service()` + 对方 proto；禁止 import 别的服务根包，禁止读写别的域的 `models` 与表。生成器扫 `microservice/` 的 import 直接判失败。
- 可以 import 对方**不注册服务**的内部子包（子包一旦注册服务就违规）；失败语义（报错还是降级）写进领域文档。

**事件**：见 §3.5。

**身份**：只信拦截器 `Decode` 写回 ctx 的主体（`core.Propagator`），不从 body/query 取。

## 6. 运行时契约

| 包 | 负责 | 项目不要做 |
| --- | --- | --- |
| `core` | 进程内注册表 `GlobalRegistry`（`RegisterAddr`/`GetClient`/`Discover`/`MustDiscover`）、`MaxMsgSize`（32MiB）、四个拦截器（身份传播 + span + panic 恢复 + 观察点）、`SetPropagator`/`AddObserver`/`SetDefaultTimeout`、`ShutdownContext` | 自己写注册表、拦截器、信号处理、trace 传播 |
| `gateway` | mux 装配与唯一 JSON 口径（camelCase + EmitUnpopulated + 枚举用名字 + 反序列化忽略未知字段）；纯 server-streaming 按 SSE 直出；三个钩子 `RewriteResponse`/`WriteError`/`OutgoingHeader` | 另建 mux、另解一次信封、另定 JSON 口径 |
| `observe` | 默认访问日志（每次 RPC 一条 slog，带 `trace_id`/`span_id`；请求 payload 经 `log.JSON` 折叠，大二进制不会整段进日志）+ HTTP 入口根 span + `X-Trace-Id` 回写 | 再写一套访问日志中间件 |
| `log` | 全局 slog 门面：`Init(Options{Level, JSON, Out, AddSource})`、`WithContext` 自动带 `trace_id`/`span_id`/`parent_span_id`、`JSON` 安全序列化（`[]byte` 折成 `<N bytes>`、深度/长度封顶）、`Level()` 运行期改级别 | 另引第三方 logger、自己拼 trace 字段、把 payload 原样 `fmt`/`json.Marshal` 进日志 |
| `tracing` | W3C `traceparent`/`baggage` 传播 + TracerProvider；设 `OTEL_EXPORTER_OTLP_ENDPOINT` 才导出，`OTEL_SERVICE_NAME` 定服务名 | 自定义头名/编码，自造 trace id |
| `metrics` | RED 指标 `szrw_rpc_requests_total`/`szrw_rpc_duration_seconds`（`method`/`kind`/`code` 标签）+ Go 运行时指标 + `Handler()` | 自己统计 RPC 次数/耗时 |
| `health` | `Register(name, check)` + `Handler()`：不注册=存活，注册了=就绪（3s 超时，失败 503 并列出失败项） | 自己写探针形状 |
| `obsdb` | 本次运行的 span + 日志写本地 SQLite；启动删旧库，单库 500MB × 最多 3 个轮转，异步批、队列满即丢 | 拿它当业务存储或长期保留 |

- **契约**（不可替换）：注册表、`MaxMsgSize`、`TraceKey`、四个拦截器、`StartSpan`/`ExtractTraceHTTP`、`ShutdownContext`、`gateway.New` 的 JSON 口径与流式直出、`health.Handler` 的响应形状。
- **扩展点**（内容由项目给）：`Propagator`、`Observer`（可叠加）、`SetDefaultTimeout`、`gateway.Options` 的三个函数、`health.Register`。
- **参考实现**（可整体替换）：`observe`、`metrics`。它们只是「新项目什么都不写也能跑」的默认内容；换成自己的就忽略它们，不是去配置它们。`log` 是门面不是契约：换第三方 logger 时整体替换，但日志里的 trace 字段必须来自当前 span。
- 默认超时只补一元调用（0 = 不补）；流的生命周期由业务决定，流不封顶。
- 进程内调用（loopback）caller 侧不起 span、不记日志：没有网络跳，服务端那条已经说完了同一件事。跨进程调用两侧都有。
- `tracing.Install(obs.Processor())` 必须在 `obsdb.Open` 之后、任何 span 之前：provider 按 tracer 名缓存 processor 列表，晚注册的链路 span 全丢。
- `obsdb` 只活一次运行（重启换新库），所以**没有 schema 迁移**：加字段直接改 schema 常量。

最小接入（示例代码，HTTP 组装归项目）：除了 `obsdb` 必须建在 `tracing.Install` 之前，其余调用都是「项目要什么就装什么」，不装也能跑。

```go
ctx, stop := core.ShutdownContext()
defer stop()
wiring.BootAll(ctx)

mux := gateway.New(gateway.Options{
    Register:  func(m *runtime.ServeMux) { wiring.RegisterGateways(ctx, m) },
    Streaming: wiring.StreamingGatewayMethods,
})

observe.Install()
obs, err := obsdb.Open(obsdb.Options{Dir: "logs"}) // 必须在 tracing.Install 之前
if err != nil { tracing.Install() } else { defer obs.Close(); tracing.Install(obs.Processor()) }
metrics.Install()
core.SetDefaultTimeout(10 * time.Second)            // 0 = 不补；流不封顶

root := http.NewServeMux()                          // HTTP 组装归项目
root.Handle("/healthz", health.Handler())           // 探针与指标挂在网关外
root.Handle("/metrics", metrics.Handler())          // 只给内网或反代白名单
```

## 7. 前端接法

| 生成物 | 内容 | 手改 |
| --- | --- | --- |
| `<target>/<proto 相对目录>/<serviceLower>Api.ts` | 每个带 HTTP 注解的 rpc 一个方法，类型来自 `types_from` 指向的 ts-proto 输出 | 否 |
| `<target>/<proto 相对目录>/<serviceLower>Api.js` | 同上，无类型（`lang: js`） | 否 |
| `<target>/grpcGatewayHelper.ts` / `.js` | 请求封装：`service.post` / `service.get` / `service.stream(path, data, opts)`、`JsonReq`、bytes↔base64、`configureGrpcWeb` | 否 |
| 项目侧 ts-proto 类型 | `<types_from>/**`，int64→string，`id/*Id` brand `EntityId` | 否 |

- 目录由 `codegen.yaml` 的 `frontend_api.targets` 声明，每条四件事：`dir`、`lang`（`ts`|`js`）、`service_import`（该目录的请求层）、`types_from`（ts 必填、js 不许写）。新增站点/移动端只加一条，不改插件、不改生成脚本。
- 生成物整体替换目标目录：只保留本次生成的文件，且目录必须已存在。
- 请求层归项目（参考实现：使用方项目仓的共享库 `web/kit`）：统一 token、签名、超时、去重、解信封、错误提示、401 清会话；页面不再包 axios/fetch，不再判断 `res.code`，也不重复 toast（静默请求单独声明）。
- 页面只调生成的客户端与生成的类型；禁止手写 URL、手写请求/响应 interface、二次解信封。
- 签名密钥必须等于服务端配置项；轮换时两端同步（项目事实，写在项目 `AGENTS.md`）。
- ID/金额/时间按 §5；`int64` 字段在 TS 一律 string。
- 流式：`stream` 走 SSE，需要项目侧的 `configureGrpcWeb` 适配（客户端得支持分块响应）；没接入适配就不要假设流式可用。
- 一个页面一个 URL；路由与菜单由站点接（管理后台的分组/表格组件是站点约定）。改共享请求层等于全站改动：至少验证受影响站点能构建。

## 8. 构建与发布

**生成入口只有一个**：框架仓的 `scripts/generate.sh`（cwd = 项目根，必须有 `proto/` 与 `codegen.yaml`）。

| 用法 | 行为 |
| --- | --- |
| `generate.sh` | 生成；缺实现即失败（check） |
| `generate.sh --check` | 只校验：跑一遍再丢弃产物，`gen/` 原样还原 |
| `generate.sh --scaffold` | 补出缺失的骨架文件（只补不覆盖） |

- 它做全部固定动作：五个 protoc 插件（`go`/`go-grpc`/`go-json`/`grpc-gateway` 四个公开插件 + 框架自己的 `protoc-gen-szrw-codegen`）与固定选项、`gen/` 原子替换（失败先还原，坏 proto 不会把仓删空）、proto 契约校验。项目脚本里**只留一行调用**。
- 本项目插件从挂载的框架源码 `go install`，所以改生成器不必先发布。
- 宿主只需要 Docker：没有 protoc 就在 `szrw-builder` 镜像里跑（镜像由 `Dockerfile.builder` + `scripts/build-builder.sh` 构建，Go/Node/protoc 版本是它的 `ARG`）。项目不自带工具链镜像。
- **发布链的机制在框架的 `scripts/build-host.ps1` / `build-host.sh`**：项目脚本 dot-source 它，只给四个事实（镜像变量、bake 文件、bake target、是否多站点 staging）再调用一次 `Invoke-SzrwBuild` / `szrw_invoke_build`。壳负责关 attestation、确保 builder 镜像、三个固定挂载（repo / szrw / cache）与 `--load`/`--push`；新项目不要把这几件事再抄进 `build.ps1`。
- 前端站点清单是生成物 `gen/frontends.json`（生成器按「`web/<name>` 有 `package.json` 且根目录或 `apps/*/` 下有 `vite.config.{ts,js}`」写入）。脚本侧的唯实现是 `scripts/frontends.sh`：读清单，清单缺失才回退探测；清单与磁盘不一致只说明忘了跑生成，不是配置项。
- 项目侧脚本只是一层壳，开关语义固定：生成（`-ProtoOnly`）、只校验（`-ValidateOnly`，即 `--check`，不动前端）、补骨架（`-Scaffold`）、不推送（`-SkipPush`）。模板里生成壳叫 `scripts/gen.ps1`（只需要生成与 `-Scaffold`），发布壳叫 `build.ps1` / `build.sh`（同名同参，dot-source `scripts/build-host.*`）。`-ProtoOnly` 不等于跑完契约测试：项目自己的 `contracts` 测试要在改 proto/模型的提交前单独跑。
- `codegen.yaml` 的边界：**只描述生成物在哪、长什么样，不描述业务语义**。未知键、版本不符、越界路径一律失败；加一个业务开关就等于把业务写进生成器。
- 生成物入库（`gen/`、前端客户端、ts-proto 类型），编译产物不提交（`main`、站点产物目录）。

**版本与本地差异**

- 模块无 tag：`go.mod` 用伪版本 `v0.0.0-<时间>-<提交>`。破坏性改动直接改，下游重新生成。
- 本地有 `go.work` 时用的是工作副本（改框架立刻生效，不需要 `replace`）；容器与 CI 不看 `go.work`，从 GitHub 拉伪版本。所以「本地过、CI 挂」先看是不是忘了补伪版本。

**发布**

- 二进制 + 生产配置 + 前端产物打一个镜像；镜像变量用项目自己的（如 `*_IMAGE`），不要通用 `IMAGE`。
- 镜像推到自建私有仓 `registry.lmcl.xyz`（默认 `registry.lmcl.xyz/lhdshr/<name>:latest`），推/拉前都要 `docker login` 一次，CI 可用 `REGISTRY_USER` / `REGISTRY_PASSWORD`（框架层的 `Confirm-SzrwRegistryLogin` / `szrw_registry_login` 负责自动登录）。脚本仍固定关 `--provenance/--sbom` 并设 `BUILDX_NO_DEFAULT_ATTESTATIONS=1`：不需要 provenance/SBOM，少一层 manifest，也不依赖目标 registry 的 OCI artifact 支持。
- 契约变更的发布顺序：**服务端 + web 同镜像原子生效，再推移动端整包/更新包**。网关对未知字段是静默丢弃，删/改字段后旧客户端是「降级」而不是报错，上线说明要写清窗口期内的可见行为。
- 迁移 SQL 不进仓（见 §3.2）。

## 9. 验收

```powershell
.\build.ps1 -ProtoOnly          # 生成 + proto 契约校验（模板：./scripts/gen.ps1；或 -ValidateOnly 只校验）
go build ./...
go vet ./...
go test ./contracts             # 项目自己的边界 / 字段口径 / 冻结契约
go test ./...
```

```powershell
cd web\<站点> ; npm ci ; npm run build     # 有 verify/typecheck 脚本的站点优先用它们
```

静态体检（工具随 `szrw-builder` 镜像走）：

```powershell
$pkgs = go list ./... | Where-Object { $_ -notmatch 'node_modules' }
staticcheck $pkgs        # SA/S/U/QF 全量
deadcode -test $pkgs     # 官方 x/tools：不可达函数
```

**格式检查的 CRLF 陷阱**：工作区是 CRLF，`gofmt -l` 会把几乎每个文件都列成「未格式化」（假象）。用忽略行尾的检查：

```bash
for f in $(find . -name '*.go' -not -path './web/*'); do
  diff -q <(gofmt "$f" | tr -d '\r') <(tr -d '\r' < "$f") >/dev/null || echo "UNFORMATTED $f"
done
```

对已格式化的 CRLF 文件跑 `gofmt -w` 是零内容差异的，可以放心对改动文件跑。

**`govulncheck` 必须对着发布工具链跑**（本地 Go 的标准库版本会造成误报）：

```bash
# Git Bash 下加 MSYS_NO_PATHCONV=1，否则 -w /src 会被改写成 Windows 路径
MSYS_NO_PATHCONV=1 docker run --rm -v "$PWD:/src" -w /src szrw-builder:latest \
  govulncheck ./...
```

**基线**（低于它算退化）：`gofmt` / `go vet` / `staticcheck` 干净；`govulncheck` 0 可达漏洞；模板与使用方项目构建通过。

- `deadcode` 的**固定残留项**记在该仓 `AGENTS.md`，不要为了清零去删。
- 死代码与有意保留的分界：注释写了用途（扩展点、回填脚本的口径真相源、审计口径）→ 保留；没注释、没调用 → 删。
- `//nolint:staticcheck` 对 staticcheck 无效（那是 golangci-lint 的），要么真修，要么 `//lint:ignore <检查项> <理由>`。
- 文档也是代码：项目可以用一条窄守卫把文档里的仓库路径逐个 `os.Stat`，不存在就 fail。
- 本机存在 `web/*/node_modules` 时 `go test ./...` 会走进 npm 包自带的 `go.mod`；CI 没有，忽略即可。

交付前：

- [ ] 入口是 proto RPC，路径正确；内部接口无注解；生成物没手改。
- [ ] 跨域只经 `local_service_center`；没写别人的表。
- [ ] 事务完整；事件同事务入 outbox；循环经 `start` 挂载。
- [ ] ID：proto/model `int64`，前端收发 string，无 `Number(id)`；`pageInfo.total` 走 `i64ToNumber`。
- [ ] 改契约时同步到了：生成物、每个站点的类型检查、移动端服务层、客户可见的 API 文档、该域 `docs`。
- [ ] 改表/改主键时：models 改了、DDL 已交 DBA（不进仓）、docs 写清数据归属。
- [ ] 前端只用生成客户端/类型；新页面已接路由/菜单。
- [ ] 框架仓自身改动：`go build ./... && go vet ./...`，并用 `template/`（`go build ./... && go vet ./... && go test ./...`）与一个真实项目重跑生成。

## 10. 存量体检

判断「某个/全部服务是否还合规」时按机械检查跑，不靠读代码猜：

```powershell
# 项目脚本的 -ValidateOnly（内部就是 generate.sh --check；模板用 ./scripts/gen.ps1）
.\build.ps1 -ValidateOnly
go test ./contracts                                       # 边界 + 字段口径 + 冻结契约

# 服务骨架：每个 proto 服务目录都要有实现、var Service、Main；Main 里不许 go/跨域调用
rg -n 'var Service|func Main\(' microservice -g '!*_test.go'

# 鉴权：后台 rpc 数 vs 项目鉴权调用数 应一一对得上（登录类接口是登记的例外）
rg -c '^\s*rpc Admin' proto/*/*.proto
rg -c '<项目的鉴权函数>' microservice/<域> -g '!*_test.go'

# 错误口径
rg -n 'return nil, err$' microservice        # 只允许透传已包装错误
rg -n 'fmt\.Errorf|status\.Error' microservice

# 写表归属：每个域只能写自己的 models.X
rg -oN '(Insert|Update|Delete)\(&?models\.\w+' microservice

# 前端 ID 纪律
rg -n 'Number\([^)]*[Ii]d\)|parseInt\([^)]*[Ii]d' web mobile
```

结论按「证据 + 文件:行」记录，不写「感觉不符合」。修完按性质落地：**业务口径类**（ID/分页/边界）加进项目 `contracts` 守卫；**风格/静态类**进 §9 基线；**确实不能改的**写进架构文档的例外清单。修完不落地，等于下次再查一遍。

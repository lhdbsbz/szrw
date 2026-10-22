# proto_third

protoc 编译时需要的第三方 proto（`import` 的解析目标，不参与生成逻辑）。

`google/api/{annotations,http}.proto` 来自
[googleapis](https://github.com/googleapis/googleapis/tree/master/google/api)，
提供 `google.api.http` 注解（`extend google.protobuf.MethodOptions`，编号 72295728）。
grpc-gateway 从 descriptor 里读它来生成 HTTP 路由。

**为什么放仓里而不是放进工具链镜像**：IDE 只索引磁盘上的文件。镜像里的 include 对
`protoc` 有效、对 GoLand/VSCode 无效——`import "google/api/annotations.proto"` 会变成
"构建能过、编辑期爆红"的长期噪音。401 行换 IDE 绿，值得。

升级 grpc-gateway 时：取同名文件覆盖 → 确认扩展编号仍是 `72295728` → 跑一次生成看路由 diff。

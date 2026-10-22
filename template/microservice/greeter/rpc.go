// Code scaffolded by protoc-gen-szrw-codegen from proto/greeter/greeter.proto:GreeterService
// 归你所有：生成器只在文件缺失时创建它，之后不再覆盖。

package greeter

import (
	"context"

	"github.com/lhdbsbz/szrw-template/gen/local_service_center"
	"github.com/lhdbsbz/szrw-template/gen/proto/greeter"
	"github.com/lhdbsbz/szrw-template/gen/startup"
)

var Service = &ServiceServer{}

// Main 注册本服务。后台循环用 start(...) 挂载，由 wiring.BootAll 统一放行。
func Main(ctx context.Context, start startup.Func) {
	local_service_center.RegisterGreeterService(ctx, Service)
}

type ServiceServer struct {
	greeter.UnimplementedGreeterServiceServer
}

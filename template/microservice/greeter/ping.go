// Code scaffolded by protoc-gen-szrw-codegen from proto/greeter/greeter.proto:GreeterService/Ping
// 归你所有：生成器只在文件缺失时创建它，之后不再覆盖。

package greeter

import (
	"context"

	"github.com/lhdbsbz/szrw-template/gen/proto/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Ping 是生成的骨架，实现后请删掉这一行说明。
func (s *ServiceServer) Ping(ctx context.Context, req *common.Empty) (*common.Empty, error) {
	return nil, status.Error(codes.Unimplemented, "未实现: GreeterService/Ping")
}

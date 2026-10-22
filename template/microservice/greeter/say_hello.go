// Code scaffolded by protoc-gen-szrw-codegen from proto/greeter/greeter.proto:GreeterService/SayHello
// 归你所有：生成器只在文件缺失时创建它，之后不再覆盖。

package greeter

import (
	"context"

	"github.com/lhdbsbz/szrw-template/gen/proto/greeter"
)

// SayHello 是模板里唯一一段业务代码：把骨架的 Unimplemented 换掉即可。
func (s *ServiceServer) SayHello(_ context.Context, req *greeter.SayHelloReq) (*greeter.SayHelloResp, error) {
	name := req.GetName()
	if name == "" {
		name = "world"
	}
	return &greeter.SayHelloResp{Message: "hello, " + name}, nil
}

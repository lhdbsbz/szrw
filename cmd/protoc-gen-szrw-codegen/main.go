// protoc-gen-szrw-codegen 是 szrw-codegen 唯一的 protoc 插件入口。
//
// 两段都由同一份 codegen.yaml 决定，所以不需要两个插件二进制：
//
//   - registry 段存在：进程内服务注册、Gate/BootAll、启动契约
//   - frontend_api 段存在：TS/JS 客户端与 runtime helper
//
// 参数只有两个：config=<path>、scaffold=write（本地加 RPC 时补骨架；
// 缺省等价 check，CI 缺实现就是失败）。
package main

import (
	"fmt"

	"github.com/lhdbsbz/szrw/internal/config"
	"github.com/lhdbsbz/szrw/internal/frontendgen"
	"github.com/lhdbsbz/szrw/internal/registrygen"
	"google.golang.org/protobuf/compiler/protogen"
)

func main() {
	protogen.Options{}.Run(func(gen *protogen.Plugin) error {
		param := ""
		if gen.Request.Parameter != nil {
			param = *gen.Request.Parameter
		}

		loaded, err := config.Load(param)
		if err != nil {
			return err
		}

		if loaded.Enabled() {
			if err := registrygen.Generate(gen, loaded.Registry); err != nil {
				return err
			}
		}

		if loaded.FrontendEnabled() {
			if err := loaded.FrontendAPI.Normalize(); err != nil {
				return fmt.Errorf("frontend_api: %w", err)
			}
			if err := frontendgen.Generate(gen, loaded.FrontendAPI); err != nil {
				return err
			}
		}

		return nil
	})
}

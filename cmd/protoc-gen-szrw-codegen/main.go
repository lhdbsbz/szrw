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

		// 前端站点清单：脚本据此挂 node_modules 卷、打包产物，不再自己探测 vite.config.*。
		// 与 registry/frontend_api 两段无关：后端-only 项目也会拿到 sites: []。
		manifest, err := frontendgen.Manifest(".")
		if err != nil {
			return fmt.Errorf("前端站点清单: %w", err)
		}
		if _, err := gen.NewGeneratedFile(frontendgen.ManifestPath, "").Write(manifest); err != nil {
			return fmt.Errorf("写入 %s: %w", frontendgen.ManifestPath, err)
		}

		return nil
	})
}

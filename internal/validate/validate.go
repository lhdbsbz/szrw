// Package validate 把「proto 契约规则」从使用方的构建脚本里搬进生成器。
//
// 规则只看 descriptor + proto 目录就能判，全部是框架常量，没有任何开关或
// 逐目录例外：不满足就是构建失败。需要项目命名约定（*Id 命名、分页 DTO、
// 某个客户端字段冻结）的判断留在项目自己的测试里。
package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lhdbsbz/szrw/internal/config"
	"github.com/lhdbsbz/szrw/internal/httpx"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Run 检查全部 proto 契约规则，返回按文件排序的违规列表。
func Run(gen *protogen.Plugin) []string {
	v := &validator{}
	v.checkLayout()
	v.checkDescriptors(gen)
	sort.Strings(v.violations)
	return v.violations
}

type validator struct {
	violations []string
}

func (v *validator) fail(path, msg string) {
	v.violations = append(v.violations, path+": "+msg)
}

// ---- 布局：目录里只能有 {dir}.proto 与 {dir}_core_model.proto ----

func (v *validator) checkLayout() {
	root := config.ProtoRoot
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		var protos []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".proto") {
				protos = append(protos, e.Name())
			}
		}
		if len(protos) == 0 {
			return nil // 容器目录（如 open_api/）
		}

		dir := filepath.ToSlash(path)
		base := filepath.Base(dir)
		rel := strings.TrimPrefix(dir, root+"/")

		if base == "common" {
			for _, name := range protos {
				if name != "common.proto" {
					v.fail(dir, "common/ 只允许 common.proto，发现 "+name)
				}
			}
			sort.Strings(protos)
			if len(protos) == 0 || protos[0] != "common.proto" {
				v.fail(dir, "common/ 缺少 common.proto")
			}
			return nil
		}

		serviceFile := base + ".proto"
		coreFile := base + "_core_model.proto"
		has := map[string]bool{}
		for _, name := range protos {
			has[name] = true
			if name != serviceFile && name != coreFile {
				v.fail(dir, fmt.Sprintf("%s/ 只允许 %s 与 %s，发现 %s", rel, serviceFile, coreFile, name))
			}
		}
		// 服务 proto 可选（只有 core 模型的目录也合法，历史上有 websocket），
		// core model 必填：实体定义是这一层唯一的信息来源。
		if !has[coreFile] {
			v.fail(dir, "缺少 "+coreFile)
		}
		return nil
	})
	if err != nil {
		v.fail(root, "读取 proto 根失败: "+err.Error())
	}
}

// ---- descriptor 规则：服务声明、import 方向、HTTP 路由、整数宽度 ----

func (v *validator) checkDescriptors(gen *protogen.Plugin) {
	for _, file := range gen.Files {
		if !file.Generate {
			continue // 只检查本次输入，不检查 import 进来的第三方
		}
		path := file.Desc.Path()
		if strings.HasPrefix(path, "google/") {
			continue
		}

		v.checkServiceDeclarations(path, file)
		v.checkImports(path, file)
		v.checkHTTPRoutes(path, file)
		v.checkIntegerWidth(path, file)
	}
}

func (v *validator) checkServiceDeclarations(path string, file *protogen.File) {
	count := len(file.Services)
	isCore := strings.HasSuffix(path, "_core_model.proto")
	if strings.Contains(path, "/common/") {
		if count > 0 {
			v.fail(path, "common/ 里不声明 service")
		}
		return
	}
	switch {
	case isCore && count > 0:
		v.fail(path, "core model 不得声明 service（service 只写在 {dir}.proto 里）")
	case !isCore && count == 0:
		v.fail(path, "service proto 必须声明且只声明一个 service")
	case count > 1:
		v.fail(path, fmt.Sprintf("一个 proto 文件只允许一个 service，发现 %d 个（注册名会打架）", count))
	}
}

func (v *validator) checkImports(path string, file *protogen.File) {
	if file.Desc.Package() == "" {
		return
	}
	isCommon := strings.Contains(path, "/common/")
	isCore := strings.HasSuffix(path, "_core_model.proto")

	imports := file.Desc.Imports()
	for i := 0; i < imports.Len(); i++ {
		target := imports.Get(i).FileDescriptor
		targetPath := target.Path()
		if strings.HasPrefix(targetPath, "google/") {
			continue
		}
		targetIsCommon := strings.Contains(targetPath, "/common/")
		targetHasService := target.Services().Len() > 0

		switch {
		case isCommon && !targetIsCommon:
			v.fail(path, "common/ 只能 import 其他 common proto，发现 "+targetPath)
		case isCore && targetHasService:
			v.fail(path, "core model 不得 import 声明了 service 的 proto（"+targetPath+"）")
		case !isCore && !isCommon && targetHasService:
			v.fail(path, "service proto 不得 import 另一个 service proto（"+targetPath+"）；只能 import common 或 *_core_model.proto")
		}
	}
}

func (v *validator) checkHTTPRoutes(path string, file *protogen.File) {
	prefix := routePrefix(path)
	for _, service := range file.Services {
		for _, method := range service.Methods {
			if n := httpx.AdditionalBindingCount(method); n > 0 {
				v.fail(path, fmt.Sprintf("%s.%s 用了 additional_bindings（%d 条）：框架只认主绑定路径，网关与前端会分叉",
					service.Desc.Name(), method.Desc.Name(), n))
			}
			rule := httpx.Extract(method)
			if rule == nil {
				// 没有注解 = 只走进程内调用，合法。但注解存在却解析不出路径要报。
				if httpx.HasAnnotation(method) {
					v.fail(path, fmt.Sprintf("%s.%s 的 google.api.http 注解无法解析（只支持 get/post/put/delete/patch + 路径）",
						service.Desc.Name(), method.Desc.Name()))
				}
				continue
			}
			want := prefix + "/" + string(service.Desc.Name()) + "/" + string(method.Desc.Name())
			if rule.Path != want {
				v.fail(path, fmt.Sprintf("%s.%s 的 HTTP 路由是 %q，应为 %q",
					service.Desc.Name(), method.Desc.Name(), rule.Path, want))
			}
		}
	}
}

// routePrefix 由 proto 文件位置唯一推导，没有逐目录例外表：
//
//	proto/<d>/x.proto        → /grpc-gateway
//	proto/<d1>/<d2>/x.proto  → /grpc-gateway/<d1>
//
// 即「路由前缀 + proto 命名空间的第一段（仅当 proto 本身分了子目录）」。
// 现有的 open_api 域因此落在 /grpc-gateway/open_api 下，不需要任何配置。
func routePrefix(path string) string {
	rel := strings.TrimPrefix(filepath.ToSlash(path), config.ProtoRoot+"/")
	parts := strings.Split(rel, "/")
	// parts 至少是 [<d>, <file>.proto]；多于两段说明文件就在子目录里。
	if len(parts) > 2 {
		return config.HTTPRoutePrefix + "/" + parts[0]
	}
	return config.HTTPRoutePrefix
}

func (v *validator) checkIntegerWidth(path string, file *protogen.File) {
	var walk func(list protoreflect.MessageDescriptors, prefix string)
	walk = func(list protoreflect.MessageDescriptors, prefix string) {
		for i := 0; i < list.Len(); i++ {
			md := list.Get(i)
			name := prefix + string(md.Name())
			fields := md.Fields()
			for j := 0; j < fields.Len(); j++ {
				fd := fields.Get(j)
				if fd.IsMap() {
					if isInteger(fd.MapKey().Kind()) && fd.MapKey().Kind() != protoreflect.Int64Kind {
						v.fail(path, fmt.Sprintf("%s.%s 的 map key 是 %s，只能是 int64", name, fd.Name(), fd.MapKey().Kind()))
					}
					continue
				}
				if isInteger(fd.Kind()) && fd.Kind() != protoreflect.Int64Kind {
					v.fail(path, fmt.Sprintf("%s.%s 是 %s，只能是 int64（JSON 里才能统一是字符串）", name, fd.Name(), fd.Kind()))
				}
			}
			walk(md.Messages(), name+".")
		}
	}
	walk(file.Desc.Messages(), "")
}

func isInteger(k protoreflect.Kind) bool {
	switch k {
	case protoreflect.Int32Kind, protoreflect.Int64Kind,
		protoreflect.Uint32Kind, protoreflect.Uint64Kind,
		protoreflect.Sint32Kind, protoreflect.Sint64Kind,
		protoreflect.Fixed32Kind, protoreflect.Fixed64Kind,
		protoreflect.Sfixed32Kind, protoreflect.Sfixed64Kind:
		return true
	}
	return false
}

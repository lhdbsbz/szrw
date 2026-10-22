// Package config 是 codegen.yaml 的唯一 schema 与校验入口。
//
// 设计原则：框架说一不二。
//
//   - **不提供布局开关**。生成物放哪、实现包叫什么、proto 根在哪、路由前缀是什么——
//     全部是常量。项目能配的只有「我这几个前端目录在哪」。
//   - 未知键一律报错（KnownFields）：配置拼错、插件升级都立刻暴露。
//   - **没有 enabled 开关**：段存在就是开启，段不存在就是不生成。
//   - 没有版本兼容：version 不匹配直接失败。
//   - 想不按框架来 ⇒ 生成器失败，你改 proto 或改目录，不改配置。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version 是当前 schema 版本。
const Version = 2

// DefaultPath 是未显式指定时读取的配置文件名（相对 protoc 的工作目录）。
const DefaultPath = "codegen.yaml"

// 框架布局常量。这些不是「默认值」，是唯一取值。
const (
	// GenProtoSeg 是 go_package 必须包含的段，用于从导入路径反推 module 根。
	GenProtoSeg = "/gen/proto/"
	// ProtoRoot 是 protoc 的 proto 根（相对仓库根）。
	ProtoRoot = "proto"
	// RegistryDir 是每个服务一个注册文件的输出目录；包名取目录名。
	RegistryDir = "gen/local_service_center"
	// WiringOut 是聚合接线文件（Gate/BootAll）。
	WiringOut = "gen/wiring/wiring_gen.go"
	// StartupOut 是启动钩子契约（只有类型，无实现）。
	StartupOut = "gen/startup/startup.go"
	// CorePackage 是生成代码依赖的运行时包：**框架仓的完整导入路径**。
	// 它是生成物契约（GlobalRegistry / MaxMsgSize / 四个拦截器），每个项目都用同一份，
	// 不允许项目自带实现——两份实现必然分叉（模板曾经就是 service 的简化版，跨进程丢 trace）。
	CorePackage = "github.com/lhdbsbz/szrw/core"
	// ImplRoot 是实现包根目录。
	ImplRoot = "microservice"
	// ImplSuffix 是实现包 import 别名的后缀：别名不能和 gen/proto 的同名包冲突。
	ImplSuffix = "_service"
	// HTTPRoutePrefix 是 google.api.http 注解必须遵守的路径前缀。
	HTTPRoutePrefix = "/grpc-gateway"
)

// Config 是 codegen.yaml 的顶层结构：只有两段，段存在即开启。
type Config struct {
	Version int `yaml:"version"`
	// Registry 存在则生成注册与接线。该段没有任何可调项。
	Registry *Registry `yaml:"registry"`
	// FrontendAPI 存在则生成前端客户端。
	FrontendAPI *FrontendAPI `yaml:"frontend_api"`
}

// Registry 是「生成注册与接线」这一段。这里刻意没有任何字段：
// 布局是常量，缺实现的处理方式由命令行参数 scaffold=write 决定，不由仓库决定。
type Registry struct {
	// WriteScaffold 来自 protoc 参数 scaffold=write（默认 false = check）。
	// 只给本地加 RPC 时用；CI 永远走 check，缺实现就是构建失败。
	WriteScaffold bool `yaml:"-"`
}

// Load 解析插件参数并严格解码。参数形如 `config=<path>[,scaffold=write]`。
// 参数为空时读 DefaultPath；文件不存在时返回零值配置（= 什么都不生成）。
func Load(param string) (*Config, error) {
	path := DefaultPath
	writeScaffold := false
	for _, item := range strings.Split(param, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		key, value, ok := strings.Cut(item, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok {
			return nil, fmt.Errorf("参数 %q 缺少 =，只接受 config=<path> 与 scaffold=write", item)
		}
		switch key {
		case "config":
			if value == "" {
				return nil, errors.New("config= 的值为空")
			}
			path = value
		case "scaffold":
			if value != "write" {
				return nil, fmt.Errorf("scaffold 只接受 write（缺省即 check），收到 %q", value)
			}
			writeScaffold = true
		default:
			return nil, fmt.Errorf("未知参数 %q：只接受 config=<path> 与 scaffold=write", key)
		}
	}

	loaded := &Config{Version: Version}
	f, err := os.Open(path)
	switch {
	case err == nil:
		defer f.Close()
		dec := yaml.NewDecoder(f)
		dec.KnownFields(true)
		if err := dec.Decode(loaded); err != nil {
			return nil, fmt.Errorf("解析 %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		return loaded, nil
	default:
		return nil, fmt.Errorf("打开 %s: %w", path, err)
	}

	if loaded.Version != Version {
		return nil, fmt.Errorf("%s 的 version=%d，本插件只支持 %d", path, loaded.Version, Version)
	}
	if loaded.Registry != nil {
		loaded.Registry.WriteScaffold = writeScaffold
	}
	return loaded, nil
}

// Enabled 报告是否生成注册与接线。
func (c *Config) Enabled() bool { return c != nil && c.Registry != nil }

// FrontendEnabled 报告是否生成前端客户端。
func (c *Config) FrontendEnabled() bool { return c != nil && c.FrontendAPI != nil }

// RegistryPackage / WiringPackage 由目录名推导包名：常量里少一个键，少一处不一致。
func RegistryPackage() string { return PackageNameOf(RegistryDir) }
func WiringPackage() string   { return PackageNameOf(filepath.Dir(WiringOut)) }

// CoreImport 是生成代码引用的运行时包导入路径。恒为完整路径，不拼 module 根。
func CoreImport() string { return CorePackage }

// StartupImport 是启动钩子契约的完整导入路径。
func StartupImport(moduleRoot string) string {
	return moduleRoot + "/" + filepath.ToSlash(filepath.Dir(StartupOut))
}

// PackageNameOf 取目录名并统一成合法包名。
func PackageNameOf(dir string) string {
	return strings.ReplaceAll(filepath.Base(filepath.Clean(dir)), "-", "_")
}

// ---- frontend_api 段 ----

const (
	LangTS = "ts"
	LangJS = "js"
)

// FrontendAPI 是前端客户端生成段：只有「输出到哪」是项目事实，其余全是规则。
type FrontendAPI struct {
	Targets []Target `yaml:"targets"`
}

// Target 是一个输出目标。请求层路径与类型根都必须显式写明，
// 不提供全局默认值再逐目标覆盖——那只会让「哪个生效」变成谜题。
type Target struct {
	Dir string `yaml:"dir"`
	// Lang 决定生成 *.ts 还是 *.js。
	Lang string `yaml:"lang"`
	// ServiceImport 是该目录下请求层（fetch 封装）的导入路径。
	ServiceImport string `yaml:"service_import"`
	// TypesFrom 是 ts-proto 类型根；lang: js 时忽略。
	TypesFrom string `yaml:"types_from"`
}

// Normalize 校验 frontend_api 段。这里不给任何默认值：缺字段就是失败。
func (f *FrontendAPI) Normalize() error {
	if len(f.Targets) == 0 {
		return errors.New("frontend_api.targets 为空（不想生成前端客户端就不要写这一段）")
	}
	seen := map[string]bool{}
	for i := range f.Targets {
		t := &f.Targets[i]
		t.Dir, t.Lang = strings.TrimSpace(t.Dir), strings.TrimSpace(t.Lang)
		t.ServiceImport, t.TypesFrom = strings.TrimSpace(t.ServiceImport), strings.TrimSpace(t.TypesFrom)
		if t.Dir == "" {
			return errors.New("targets 里有空的 dir")
		}
		if err := checkRelPath("targets[].dir", t.Dir); err != nil {
			return err
		}
		key := t.Lang + "|" + t.Dir
		if seen[key] {
			return fmt.Errorf("重复的 target（%s, %s）", t.Dir, t.Lang)
		}
		seen[key] = true
		switch t.Lang {
		case LangTS:
			if t.TypesFrom == "" {
				return fmt.Errorf("target %q 是 ts，必须写 types_from", t.Dir)
			}
		case LangJS:
			if t.TypesFrom != "" {
				return fmt.Errorf("target %q 是 js，types_from 无意义，删掉", t.Dir)
			}
		default:
			return fmt.Errorf("target %q 的 lang 只能是 %q 或 %q，收到 %q", t.Dir, LangTS, LangJS, t.Lang)
		}
		if t.ServiceImport == "" {
			return fmt.Errorf("target %q 必须写 service_import", t.Dir)
		}
	}
	return nil
}

func checkRelPath(key, value string) error {
	if filepath.IsAbs(value) {
		return fmt.Errorf("%s 必须是相对 protoc 工作目录的路径，收到 %q", key, value)
	}
	if strings.HasPrefix(filepath.Clean(value), "..") {
		return fmt.Errorf("%s 不能指到工作目录之外，收到 %q", key, value)
	}
	return nil
}

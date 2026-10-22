package registrygen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/lhdbsbz/szrw/internal/config"
)

// 域边界：一个服务只能 import 自己的子包，或者「不注册服务」的内部模块。
// 调用另一个服务必须经 local_service_center 与它的 proto 契约——现在是进程内调用，
// 将来换成远程 RPC 时，调用点与请求边界不用重写。
//
// 判据只需要「proto 声明了哪些服务」+「Go 源码 import 了什么」，与项目命名无关，
// 所以归生成器。规则来自 docs/architecture/service-boundaries.md。
func checkBoundaries(serviceRoots map[string]bool) ([]string, error) {
	if len(serviceRoots) == 0 {
		return nil, nil
	}

	var violations []string
	fset := token.NewFileSet()
	prefix := config.ImplRoot + "/"

	err := filepath.WalkDir(config.ImplRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(filepath.Dir(path))
		rel = strings.TrimPrefix(rel, prefix)
		source := owningService(rel, serviceRoots)
		if source == "" {
			return nil // 不是服务目录，也不在任何服务目录之下：内部模块，放行
		}

		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("解析 %s 失败: %w", path, err)
		}
		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return fmt.Errorf("解析 %s 的 import 失败: %w", path, err)
			}
			idx := strings.LastIndex(importPath, "/"+config.ImplRoot+"/")
			if idx < 0 {
				continue
			}
			target := importPath[idx+len("/"+config.ImplRoot+"/"):]
			targetService := owningService(target, serviceRoots)
			if targetService == "" || targetService == source {
				continue
			}
			if strings.HasPrefix(target, targetService+"/") {
				// 自己的子包可以 import，但它不能反过来注册服务。
				if reg := subpackageRegisters(filepath.Join(config.ImplRoot, target)); reg != "" {
					violations = append(violations, fmt.Sprintf(
						"%s 被 %s 直接 import，却注册了服务 %s；只能经它自己的 proto 契约调用",
						filepath.ToSlash(filepath.Join(config.ImplRoot, target)), path, reg))
				}
				continue
			}
			violations = append(violations, fmt.Sprintf(
				"%s（属服务 %s）直接 import 了服务 %s；跨域只能经 local_service_center 与它的 proto 契约",
				filepath.ToSlash(path), source, target))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(violations)
	return violations, nil
}

// owningService 找 dir 所属的服务根：dir 本身或最近的祖先。
func owningService(dir string, serviceRoots map[string]bool) string {
	for candidate := dir; candidate != "." && candidate != ""; candidate = filepath.ToSlash(filepath.Dir(candidate)) {
		if serviceRoots[candidate] {
			return candidate
		}
	}
	return ""
}

// subpackageRegisters 报告一个子包是否在注册服务（import 了注册包并调用 Register*）。
func subpackageRegisters(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			continue
		}
		alias := ""
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !strings.HasSuffix(path, "/"+config.RegistryDir) {
				continue
			}
			alias = config.RegistryPackage()
			if imp.Name != nil {
				alias = imp.Name.Name
			}
		}
		if alias == "" {
			continue
		}
		var found string
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || found != "" {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if ok && id.Name == alias && strings.HasPrefix(sel.Sel.Name, "Register") {
				found = sel.Sel.Name
			}
			return true
		})
		if found != "" {
			return found
		}
	}
	return ""
}

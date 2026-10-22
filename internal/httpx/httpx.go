// Package httpx 是 google.api.http 注解的**唯一**解析实现。
//
// 为什么必须唯一：registry 用它判断某个服务要不要生成网关注册，frontend-api 用它
// 生成客户端方法。两边口径一旦不一致，就会出现「前端生成了调用、网关没注册路由」
// 的运行时 404——生成物 diff 里看不出来。收敛前它靠一句注释维持一致，见旧仓
// protoc-gen-service-registry 的 methodHasHTTP。
package httpx

import (
	"reflect"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Rule 是一条可用的 HTTP 路由。只认 post/get/put/delete/patch 五种 pattern：
// 其它（custom）不是 mux 能直接挂的形式，生成器不为它产出任何东西。
type Rule struct {
	Method string // post/get/put/delete/patch
	Path   string
}

// Extract 取出方法的 HTTP 路由；没有注解或没有受支持的 pattern 时返回 nil。
func Extract(method *protogen.Method) *Rule {
	options, ok := method.Desc.Options().(*descriptorpb.MethodOptions)
	if !ok || options == nil {
		return nil
	}
	ext := proto.GetExtension(options, annotations.E_Http)
	if ext == nil {
		return nil
	}
	rule, ok := ext.(*annotations.HttpRule)
	if !ok || rule == nil {
		return nil
	}

	// Pattern 是 oneof，用反射安全访问（不同 protobuf 版本下具体类型可能不同）。
	field := reflect.ValueOf(rule).Elem().FieldByName("Pattern")
	if !field.IsValid() || field.IsNil() {
		return nil
	}
	switch v := field.Interface().(type) {
	case *annotations.HttpRule_Post:
		if v.Post != "" {
			return &Rule{Method: "post", Path: v.Post}
		}
	case *annotations.HttpRule_Get:
		if v.Get != "" {
			return &Rule{Method: "get", Path: v.Get}
		}
	case *annotations.HttpRule_Put:
		if v.Put != "" {
			return &Rule{Method: "put", Path: v.Put}
		}
	case *annotations.HttpRule_Delete:
		if v.Delete != "" {
			return &Rule{Method: "delete", Path: v.Delete}
		}
	case *annotations.HttpRule_Patch:
		if v.Patch != "" {
			return &Rule{Method: "patch", Path: v.Patch}
		}
	}
	return nil
}

// HasAnnotation 报告方法上是否写了 google.api.http 注解（不论 pattern 是否受支持）。
// 用来区分「没注解」（合法，只走进程内调用）与「注解写错」（必须报错）。
func HasAnnotation(method *protogen.Method) bool {
	options, ok := method.Desc.Options().(*descriptorpb.MethodOptions)
	if !ok || options == nil {
		return false
	}
	ext := proto.GetExtension(options, annotations.E_Http)
	rule, ok := ext.(*annotations.HttpRule)
	return ok && rule != nil
}

// AdditionalBindingCount 返回 additional_bindings 的条数。
// 框架只认主绑定：前端客户端与网关路由都按主路径生成，额外绑定会让两边口径分叉。
func AdditionalBindingCount(method *protogen.Method) int {
	options, ok := method.Desc.Options().(*descriptorpb.MethodOptions)
	if !ok || options == nil {
		return 0
	}
	ext := proto.GetExtension(options, annotations.E_Http)
	rule, ok := ext.(*annotations.HttpRule)
	if !ok || rule == nil {
		return 0
	}
	return len(rule.GetAdditionalBindings())
}

// MethodHasHTTP 该方法是否对外暴露 HTTP 路由。
func MethodHasHTTP(method *protogen.Method) bool { return Extract(method) != nil }

// ServiceHasHTTP 该服务是否有任意方法暴露 HTTP 路由（决定是否生成网关注册）。
func ServiceHasHTTP(service *protogen.Service) bool {
	for _, m := range service.Methods {
		if MethodHasHTTP(m) {
			return true
		}
	}
	return false
}

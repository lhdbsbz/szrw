// Package naming 收敛生成物的标识符风格。
//
// 两个插件都要从 proto 名字推导 Go/文件标识符，规则必须唯一：
// 收敛之前它们各自有一份同名但实现不同的 toCamelCase（一个是首字母小写，
// 一个是 snake->UpperCamel），名字一样、行为不一样，是最难发现的那类 bug。
package naming

import (
	"strings"
	"unicode"
)

// LowerFirst 把首字母小写：User -> user。用于生成前端文件名（userApi.ts）。
func LowerFirst(s string) string {
	if s == "" {
		return s
	}
	if c := s[0]; c >= 'A' && c <= 'Z' {
		return string(c+32) + s[1:]
	}
	return s
}

// Snake 把 UpperCamel 转成 snake_case：UpdateNickName -> update_nick_name，AIGateway -> ai_gateway。
// 用于从 rpc 名推导骨架文件名（一 rpc 一文件）。
func Snake(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsUpper(r) {
			b.WriteRune(r)
			continue
		}
		if i > 0 {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// UpperCamel 把 snake_case / kebab-case 转成 UpperCamel：open_api_memory -> OpenApiMemory。
// 用于从 proto 相对路径推导 Go 侧注册函数名。
func UpperCamel(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' })
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
		}
	}
	return strings.Join(parts, "")
}

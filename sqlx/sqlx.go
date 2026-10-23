// Package sqlx 提供两个 ORM 都不管、但日志与排障每次都要的小工具。
//
// Inline 只用于「把实际执行的 SQL 打进日志」，不用于真正执行：参数值会被
// 加引号/转义后填进 ? 的位置。扫描时跳过字符串字面量、标识符引号、注释和
// \?，所以 URL、JSON、注释里的问号不会被误当成占位符。
//
// Truncate 从中间截断，保留头尾，避免一条超长 SQL 或大字段把日志刷爆。
//
// 本包只依赖标准库，不引入任何驱动或 ORM。
package sqlx

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// maxInlineArgChars 是单个字符串参数在日志里保留的原文 rune 数（头尾合计）。
// 超过这个长度的值用 Truncate 折叠，否则一次批量写入就能刷出几 MB 日志。
const maxInlineArgChars = 512

// sqlStringEscaper 按 MySQL 单引号字符串的规则转义。
// 单引号用两个单引号（不是反斜杠转义）：NO_BACKSLASH_ESCAPES 模式下也能保持语法正确；
// 反斜杠单独加倍；控制字符转成可见形式，保证一条 SQL 只占一行。
var sqlStringEscaper = strings.NewReplacer(
	`\`, `\\`,
	`'`, `''`,
	"\x00", `\0`,
	"\n", `\n`,
	"\r", `\r`,
	"\t", `\t`,
)

// Inline 把 query 里的 ? 用 args 就地替换。
//
// 规则：
//   - 只替换字符串/标识符引号、注释、\? 之外的 ?；
//   - 参数少于占位符时，多出来的 ? 原样保留；参数多余时忽略；
//   - nil → NULL，time.Time → RFC3339，[]byte → <N bytes>，bool → 1/0；
//   - string 加单引号并转义，过长时走 Truncate。
func Inline(query string, args []any) string {
	if len(args) == 0 {
		return query
	}

	var b strings.Builder
	b.Grow(len(query) + len(args)*8)

	next := 0
	for i := 0; i < len(query); {
		switch query[i] {
		case '\'', '"', '`':
			// 字符串字面量与标识符里的 ? 不是占位符；单/双引号支持反斜杠转义。
			end := skipQuoted(query, i, query[i], query[i] != '`')
			b.WriteString(query[i:end])
			i = end

		case '-':
			// MySQL 的 -- 注释要求后面跟空白；否则 1--2 是减法，? 仍是占位符。
			if i+1 < len(query) && query[i+1] == '-' && (i+2 == len(query) || isSpace(query[i+2])) {
				end := skipLine(query, i)
				b.WriteString(query[i:end])
				i = end
			} else {
				b.WriteByte(query[i])
				i++
			}

		case '#':
			end := skipLine(query, i)
			b.WriteString(query[i:end])
			i = end

		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				end := skipBlockComment(query, i)
				b.WriteString(query[i:end])
				i = end
			} else {
				b.WriteByte(query[i])
				i++
			}

		case '\\':
			// \? 是字面问号：连同反斜杠原样保留，不消耗参数。
			b.WriteByte(query[i])
			i++
			if i < len(query) {
				b.WriteByte(query[i])
				i++
			}

		case '?':
			if next < len(args) {
				b.WriteString(inlineArg(args[next]))
				next++
			} else {
				b.WriteByte('?')
			}
			i++

		default:
			b.WriteByte(query[i])
			i++
		}
	}
	return b.String()
}

// Truncate 保留头尾、截掉中间，并插入 "…(N chars)"，N 是原文的 rune 数。
//
// max 是保留的原文 rune 数（头尾合计），不包含标记本身；max<=0 时只返回标记。
// 按 rune 切分，不会把 UTF-8 字符截成半个。
func Truncate(s string, max int) string {
	if max < 0 {
		max = 0
	}
	// 字节数已经不超过 max，rune 数只会更少，省一次分配。
	if len(s) <= max {
		return s
	}

	r := []rune(s)
	if len(r) <= max {
		return s
	}

	marker := fmt.Sprintf("…(%d chars)", len(r))
	if max == 0 {
		return marker
	}

	head := max / 2
	tail := max - head

	var b strings.Builder
	b.Grow(head*4 + len(marker) + tail*4)
	b.WriteString(string(r[:head]))
	b.WriteString(marker)
	b.WriteString(string(r[len(r)-tail:]))
	return b.String()
}

func inlineArg(v any) string {
	if v == nil {
		return "NULL"
	}

	switch x := v.(type) {
	case string:
		return quoteSQL(Truncate(x, maxInlineArgChars))
	case []byte:
		if x == nil {
			return "NULL"
		}
		return fmt.Sprintf("<%d bytes>", len(x))
	case time.Time:
		return quoteSQL(x.Format(time.RFC3339))
	case *time.Time:
		if x == nil {
			return "NULL"
		}
		return quoteSQL(x.Format(time.RFC3339))
	case bool:
		if x {
			return "1"
		}
		return "0"
	}

	// 自定义类型（type UserID string、type Blob []byte…）走 reflect，避免 Go 语法
	// 漏进 SQL（比如把数字类型打印成 '123'）。
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return "NULL"
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.String:
		return quoteSQL(Truncate(rv.String(), maxInlineArgChars))
	case reflect.Bool:
		if rv.Bool() {
			return "1"
		}
		return "0"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float32:
		return strconv.FormatFloat(rv.Float(), 'g', -1, 32)
	case reflect.Float64:
		return strconv.FormatFloat(rv.Float(), 'g', -1, 64)
	case reflect.Slice:
		if rv.IsNil() {
			return "NULL"
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return fmt.Sprintf("<%d bytes>", rv.Len())
		}
	case reflect.Map, reflect.Chan, reflect.Func:
		if rv.IsNil() {
			return "NULL"
		}
	}

	if s, ok := v.(fmt.Stringer); ok {
		return quoteSQL(Truncate(s.String(), maxInlineArgChars))
	}
	return quoteSQL(Truncate(fmt.Sprint(v), maxInlineArgChars))
}

func quoteSQL(s string) string {
	return "'" + sqlStringEscaper.Replace(s) + "'"
}

func skipQuoted(s string, start int, quote byte, backslash bool) int {
	i := start + 1
	for i < len(s) {
		switch s[i] {
		case '\\':
			if backslash {
				i += 2
				continue
			}
			i++
		case quote:
			// 双写引号是字面量，不是结束。
			if i+1 < len(s) && s[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		default:
			i++
		}
	}
	return len(s)
}

func skipLine(s string, start int) int {
	if j := strings.IndexByte(s[start:], '\n'); j >= 0 {
		return start + j + 1
	}
	return len(s)
}

func skipBlockComment(s string, start int) int {
	if j := strings.Index(s[start+2:], "*/"); j >= 0 {
		return start + 2 + j + 2
	}
	return len(s)
}

func isSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

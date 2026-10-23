package log

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// 日志是给人看/给采集器索引的，不是数据通道。这些上限保证单个字段
// 有界：超长字符串截断，容器只留前 N 项，整棵树按节点预算封顶。
const (
	maxDepth  = 8        // 嵌套深度
	maxItems  = 64       // 单个 slice/map/struct 保留的元素数
	maxString = 2048     // 单个字符串保留的字节数
	maxBudget = 32 << 10 // 整棵树的总字符预算
	nodeCost  = 8        // 每个节点占用的预算，防「海量小标量」
)

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// JSON 把任意值序列化成适合写进日志的一行 JSON。
//
// 与 encoding/json 的差别只有「安全」：
//   - []byte（图片、音频、proto bytes 字段）折叠成 "<N bytes>"，8MiB 图片不会变成
//     十几 MiB base64；
//   - 深度、单值长度、元素个数、总预算全部封顶，超出部分标 <truncated>/<max depth>；
//   - nil 返回 "null"；NaN/Inf 退化成字符串；无法序列化时返回 <unserializable>。
//
// 永远不 panic，可以直接把线上对象丢进来。
func JSON(v any) (out string) {
	out = "<unserializable>"
	defer func() {
		if recover() != nil {
			out = "<unserializable>"
		}
	}()

	if v == nil {
		return "null"
	}
	s := &sanitizer{budget: maxBudget}
	safe := s.value(reflect.ValueOf(v), 0)
	if safe == nil {
		return "null"
	}
	// 日志不是 HTML，关掉 < > & 的转义，<N bytes> 才看得见。
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(safe); err != nil {
		return "<unserializable>"
	}
	b := bytes.TrimRight(buf.Bytes(), "\n")
	if len(b) > maxBudget {
		return fmt.Sprintf("%q", fmt.Sprintf("<%d bytes json>", len(b)))
	}
	return string(b)
}

type sanitizer struct{ budget int }

func (s *sanitizer) value(v reflect.Value, depth int) any {
	if !v.IsValid() || !v.CanInterface() {
		return nil
	}
	if depth > maxDepth {
		return "<max depth>"
	}
	if s.budget -= nodeCost; s.budget <= 0 {
		return "<truncated>"
	}

	// 接口先拆开。
	for v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}

	// error 在解指针前判断：错误消息比结构体字段有用。
	// 同时避免 nil 指针实现 error 时调用 Error() 造成 panic。
	if err, ok := v.Interface().(error); ok && !isNilable(v) {
		return s.str(err.Error())
	}

	// 指针按 JSON 语义解引用；深度上限顺手挡住环形结构。
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		if s.budget -= nodeCost; s.budget <= 0 {
			return "<truncated>"
		}
		v = v.Elem()
		depth++
	}

	// 二进制先折叠，先于任何 Marshaler：哪怕类型自带 base64 编码，也不许整段进日志。
	if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
		if v.IsNil() {
			return nil
		}
		return fmt.Sprintf("<%d bytes>", v.Len())
	}

	// 自定义序列化（time.Time、net.IP 等）优先，但输出同样受长度约束。
	if safe, ok := s.marshaled(v); ok {
		return safe
	}

	switch v.Kind() {
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint()
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Sprintf("%v", f)
		}
		return f
	case reflect.String:
		return s.str(v.String())
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		return s.list(v, depth)
	case reflect.Array:
		return s.list(v, depth)
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		return s.mapping(v, depth)
	case reflect.Struct:
		return s.structure(v, depth)
	default:
		// chan / func / complex / unsafe.Pointer 之类无法 JSON 化。
		return "<unsupported>"
	}
}

func (s *sanitizer) list(v reflect.Value, depth int) []any {
	n := v.Len()
	keep := n
	if keep > maxItems {
		keep = maxItems
	}
	out := make([]any, 0, keep+1)
	for i := 0; i < keep; i++ {
		out = append(out, s.value(v.Index(i), depth+1))
	}
	if n > keep {
		out = append(out, fmt.Sprintf("<%d more items>", n-keep))
	}
	return out
}

func (s *sanitizer) mapping(v reflect.Value, depth int) map[string]any {
	out := make(map[string]any, maxItems)
	iter := v.MapRange()
	n := 0
	for iter.Next() && n < maxItems {
		out[s.mapKey(iter.Key())] = s.value(iter.Value(), depth+1)
		n++
	}
	if total := v.Len(); total > n {
		out["<truncated>"] = fmt.Sprintf("<%d more keys>", total-n)
	}
	return out
}

// structure 按 encoding/json 的 tag 规则展开结构体；未导出字段一律跳过。
func (s *sanitizer) structure(v reflect.Value, depth int) map[string]any {
	t := v.Type()
	out := make(map[string]any, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // 未导出
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		fv := v.Field(i)
		if fv.IsZero() && strings.Contains(opts, "omitempty") {
			continue
		}
		// 匿名嵌入且未改名：按 encoding/json 的规则摊平。
		if f.Anonymous && name == "" && fv.Kind() == reflect.Struct {
			if inner, ok := s.value(fv, depth+1).(map[string]any); ok {
				for k, val := range inner {
					out[k] = val
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		out[name] = s.value(fv, depth+1)
	}
	return out
}

func (s *sanitizer) str(x string) string {
	keep := len(x)
	if keep > maxString {
		keep = maxString
	}
	// 只按真正写进日志的长度扣预算：截断后的字符串不该继续吃后面的额度。
	s.budget -= keep
	if len(x) > maxString {
		return x[:maxString] + fmt.Sprintf("<%d bytes total>", len(x))
	}
	return x
}

func (s *sanitizer) mapKey(v reflect.Value) string {
	if v.Kind() == reflect.String {
		return s.str(v.String())
	}
	if m, ok := textMarshaler(v); ok {
		if b, err := m.MarshalText(); err == nil {
			return s.str(string(b))
		}
	}
	return s.str(fmt.Sprint(v.Interface()))
}

// marshaled 尊重 json.Marshaler / encoding.TextMarshaler；输出超长就折叠，
// 非法 JSON 退化成普通字符串——总之不让一个字段把整条日志毁掉。
func (s *sanitizer) marshaled(v reflect.Value) (any, bool) {
	if m, ok := jsonMarshaler(v); ok {
		b, err := m.MarshalJSON()
		if err != nil {
			return "<marshal error>", true
		}
		if !json.Valid(b) {
			return s.str(string(b)), true
		}
		if len(b) > maxString {
			return fmt.Sprintf("<%d bytes json>", len(b)), true
		}
		return json.RawMessage(b), true
	}
	if m, ok := textMarshaler(v); ok {
		b, err := m.MarshalText()
		if err != nil {
			return "<marshal error>", true
		}
		return s.str(string(b)), true
	}
	return nil, false
}

func jsonMarshaler(v reflect.Value) (json.Marshaler, bool) {
	if v.Type().Implements(jsonMarshalerType) {
		return v.Interface().(json.Marshaler), true
	}
	if v.CanAddr() && v.Addr().Type().Implements(jsonMarshalerType) {
		return v.Addr().Interface().(json.Marshaler), true
	}
	return nil, false
}

func textMarshaler(v reflect.Value) (encoding.TextMarshaler, bool) {
	if v.Type().Implements(textMarshalerType) {
		return v.Interface().(encoding.TextMarshaler), true
	}
	if v.CanAddr() && v.Addr().Type().Implements(textMarshalerType) {
		return v.Addr().Interface().(encoding.TextMarshaler), true
	}
	return nil, false
}

func isNilable(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	}
	return false
}

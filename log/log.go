// Package log 是 szrw 的默认日志门面：标准库 log/slog 之上的一层薄包装。
//
// 它只解决四件事：
//
//  1. Init 按 Options 建 handler 并设为全局默认，可重复调用；
//  2. WithContext 把当前 span 的 trace_id / span_id / parent_span_id 挂到日志上；
//  3. Entry 的链式写法（WithField / WithFields / WithError）与级别过滤；
//  4. JSON 把任意值安全地压成一行 JSON（大二进制折叠、深度/长度封顶）。
//
// 不引第三方、不自定义格式、不做采集：stdout 交给部署侧采集器。
// 级别过滤走 Handler.Enabled，AddSource 指向真实业务调用行。
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/lhdbsbz/szrw/core"
)

// Options 是 Init 的入参。零值即「Info 级别、Text、stdout、不带 source」。
type Options struct {
	Level     slog.Level // 默认 Info（slog.Level 零值）
	JSON      bool       // true → JSON handler；false → Text
	Out       io.Writer  // nil → os.Stdout
	AddSource bool
}

// levelVar 是所有 Init 建出的 handler 共用的级别开关，运行期可热改。
var levelVar = new(slog.LevelVar)

// Level 返回全局级别开关：Level().Set(slog.LevelDebug) 立刻生效。
func Level() *slog.LevelVar { return levelVar }

// Init 按 Options 建 handler 并设为 slog 默认 logger。可重复调用，后一次覆盖前一次。
func Init(o Options) {
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	levelVar.Set(o.Level)
	opts := &slog.HandlerOptions{Level: levelVar, AddSource: o.AddSource}

	var h slog.Handler
	if o.JSON {
		h = slog.NewJSONHandler(out, opts)
	} else {
		h = slog.NewTextHandler(out, opts)
	}
	slog.SetDefault(slog.New(h))
}

// Entry 是不可变的日志入口：每次 With* 都返回新 Entry，可并发使用。
type Entry struct{ l *slog.Logger }

func defaultEntry() *Entry { return &Entry{l: slog.Default()} }

// WithContext 返回带链路字段的 Entry。ctx 为 nil 或没有 span 时就是普通 Entry。
func WithContext(ctx context.Context) *Entry {
	e := defaultEntry()
	if ctx == nil {
		return e
	}
	if id := core.TraceID(ctx); id != "" {
		e = e.WithField("trace_id", id)
	}
	if id := core.SpanID(ctx); id != "" {
		e = e.WithField("span_id", id)
	}
	if id := core.ParentSpanID(ctx); id != "" {
		e = e.WithField("parent_span_id", id)
	}
	return e
}

// 包级链式入口：没有 ctx 可用时（启动期、后台协程）直接用。
func WithField(key string, value any) *Entry { return defaultEntry().WithField(key, value) }
func WithFields(fields map[string]any) *Entry {
	return defaultEntry().WithFields(fields)
}
func WithError(err error) *Entry { return defaultEntry().WithError(err) }

// WithField 追加一个字段。
func (e *Entry) WithField(key string, value any) *Entry {
	return &Entry{l: e.l.With(key, value)}
}

// WithFields 追加一组字段。按 key 排序，保证不同次运行形状一致。
func (e *Entry) WithFields(fields map[string]any) *Entry {
	if len(fields) == 0 {
		return e
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		attrs = append(attrs, k, fields[k])
	}
	return &Entry{l: e.l.With(attrs...)}
}

// WithError 追加 error 字段；nil 不产生字段。
func (e *Entry) WithError(err error) *Entry {
	if err == nil {
		return e
	}
	return &Entry{l: e.l.With("error", err.Error())}
}

func (e *Entry) Info(msg string)  { e.log(slog.LevelInfo, msg) }
func (e *Entry) Debug(msg string) { e.log(slog.LevelDebug, msg) }
func (e *Entry) Warn(msg string)  { e.log(slog.LevelWarn, msg) }
func (e *Entry) Error(msg string) { e.log(slog.LevelError, msg) }

func (e *Entry) Infof(format string, args ...any) {
	e.log(slog.LevelInfo, fmt.Sprintf(format, args...))
}
func (e *Entry) Debugf(format string, args ...any) {
	e.log(slog.LevelDebug, fmt.Sprintf(format, args...))
}
func (e *Entry) Warnf(format string, args ...any) {
	e.log(slog.LevelWarn, fmt.Sprintf(format, args...))
}
func (e *Entry) Errorf(format string, args ...any) {
	e.log(slog.LevelError, fmt.Sprintf(format, args...))
}

// 包级快捷方式。这里直接调 e.log（而不是 e.Info），
// 保证栈上永远只有一层包装，AddSource 的跳数才不用为每条路径各算一遍。
func Info(msg string)  { defaultEntry().log(slog.LevelInfo, msg) }
func Debug(msg string) { defaultEntry().log(slog.LevelDebug, msg) }
func Warn(msg string)  { defaultEntry().log(slog.LevelWarn, msg) }
func Error(msg string) { defaultEntry().log(slog.LevelError, msg) }

func Infof(format string, args ...any) {
	defaultEntry().log(slog.LevelInfo, fmt.Sprintf(format, args...))
}
func Debugf(format string, args ...any) {
	defaultEntry().log(slog.LevelDebug, fmt.Sprintf(format, args...))
}
func Warnf(format string, args ...any) {
	defaultEntry().log(slog.LevelWarn, fmt.Sprintf(format, args...))
}
func Errorf(format string, args ...any) {
	defaultEntry().log(slog.LevelError, fmt.Sprintf(format, args...))
}

// log 手动构 record，把 PC 换成真正的调用点，而不是这个包装层。
//
// 先问 Handler.Enabled：直接 Handle 会绕过级别过滤，info 级别下 Debug 也会打出来。
// 调用栈：0=runtime.Callers 1=(*Entry).log 2=Info/Infof/包级同名函数 3=业务代码。
// 所以所有公开入口都必须只隔一层就调到 log，不能中间再加 helper。
func (e *Entry) log(level slog.Level, msg string) {
	ctx := context.Background()
	h := e.l.Handler()
	if !h.Enabled(ctx, level) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])
	rec := slog.NewRecord(time.Now(), level, msg, pcs[0])
	_ = h.Handle(ctx, rec)
}

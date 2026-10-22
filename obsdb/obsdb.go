// Package obsdb 把「本次运行」的可观测数据（span + 日志）写进本地 SQLite 文件。
//
// 生命周期（刻意的）：
//   - 进程启动：删掉上次运行留下的所有 obs-*.db，新建一个库开始写。
//   - 运行中：单库写到上限（默认 500MB）就轮转到新文件，本轮最多保留 3 个，
//     建第 4 个时删掉第 1 个。
//   - 因此永远没有"跨进程累积"，也就不需要任何 schema/DDL 升级。
//
// 为什么不放内存：内存只能留最近几分钟、窗口随流量浮动，而排障要"这次运行发生的都能查"。
// 为什么不自己写追加文件：那是一个没有索引的数据库；SQLite 是它带索引的版本。
// 字段/表结构随时可以改——每轮都是新库，建表语句就是唯一事实。
package obsdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，可交叉编译（cgo 版不行）

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const schema = `
CREATE TABLE IF NOT EXISTS span(
 trace_id TEXT NOT NULL, span_id TEXT NOT NULL PRIMARY KEY, parent_span_id TEXT NOT NULL DEFAULT '',
 name TEXT NOT NULL DEFAULT '', service TEXT NOT NULL DEFAULT '', protocol TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT '', status_code TEXT NOT NULL DEFAULT '', error_msg TEXT NOT NULL DEFAULT '',
 start_ns INTEGER NOT NULL, end_ns INTEGER NOT NULL, attrs TEXT NOT NULL DEFAULT '{}');
CREATE INDEX IF NOT EXISTS idx_span_trace ON span(trace_id, start_ns);
CREATE INDEX IF NOT EXISTS idx_span_start ON span(start_ns);
CREATE INDEX IF NOT EXISTS idx_span_root ON span(parent_span_id, start_ns);

CREATE TABLE IF NOT EXISTS log(
 id INTEGER PRIMARY KEY AUTOINCREMENT, ts_ns INTEGER NOT NULL, level TEXT NOT NULL DEFAULT '',
 msg TEXT NOT NULL DEFAULT '', caller TEXT NOT NULL DEFAULT '',
 trace_id TEXT NOT NULL DEFAULT '', span_id TEXT NOT NULL DEFAULT '', parent_span_id TEXT NOT NULL DEFAULT '',
 attrs TEXT NOT NULL DEFAULT '{}');
CREATE INDEX IF NOT EXISTS idx_log_ts ON log(ts_ns);
CREATE INDEX IF NOT EXISTS idx_log_trace ON log(trace_id);
`

// Options 控制文件位置与轮转阈值。
type Options struct {
	Dir       string // 存放目录（一般就是日志目录）
	MaxBytes  int64  // 单库上限，默认 500MB
	KeepFiles int    // 本轮最多保留几个库文件，默认 3
}

// Store 是一个运行期观测库：写走异步批处理，读跨本轮所有文件。
type Store struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	keep     int
	seq      int
	dbs      []*dbFile // 老的在前
	closed   bool

	queue chan entry
	wg    sync.WaitGroup

	// 已写入的总条数（只增不减：轮转删掉旧文件也不回退），查询页的「本轮数据」用它，
	// 避免每次刷新都对大表做 COUNT(*)。
	writtenLogs  atomic.Int64
	writtenSpans atomic.Int64
}

// dbFile 是本次运行里的一个库文件。
type dbFile struct {
	db   *sql.DB
	path string
}

type entry struct {
	span *Span
	log  *LogRecord
}

// Span 是写入/查询用的中立结构（不依赖任何 proto）。
type Span struct {
	TraceID, SpanID, ParentSpanID string
	Name, Service, Protocol       string
	Status, StatusCode, ErrorMsg  string
	StartNS, EndNS                int64
	Attrs                         map[string]any
}

// LogRecord 是一条日志。
type LogRecord struct {
	TimeNS                        int64
	Level, Msg, Caller            string
	TraceID, SpanID, ParentSpanID string
	Attrs                         map[string]any
}

// Filter 是日志查询条件。
type Filter struct {
	SinceNS, UntilNS int64
	Level, Keyword   string
	TraceID          string
	Limit, Offset    int
}

// Open 删除上次运行的库，建本次运行的新库。
func Open(opts Options) (*Store, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 500 << 20
	}
	if opts.KeepFiles <= 0 {
		opts.KeepFiles = 3
	}
	if opts.Dir == "" {
		opts.Dir = "./logs"
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	removeAllRuns(opts.Dir)

	s := &Store{dir: opts.Dir, maxBytes: opts.MaxBytes, keep: opts.KeepFiles, queue: make(chan entry, 8192)}
	if err := s.rotate(); err != nil {
		return nil, err
	}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

// Paths 返回本轮当前可查的库文件（老的在前）。
func (s *Store) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.dbs))
	for _, f := range s.dbs {
		out = append(out, f.path)
	}
	return out
}

// rotate 新建一个库文件；超过 keep 个就关掉并删掉最老的（它是本轮更早的部分）。
func (s *Store) rotate() error {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()

	path := filepath.Join(s.dir, fmt.Sprintf("obs-%s-%02d.db", time.Now().Format("20060102-150405"), seq))
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return fmt.Errorf("建表失败: %w", err)
	}

	s.mu.Lock()
	s.dbs = append(s.dbs, &dbFile{db: db, path: path})
	var evicted *dbFile
	if over := len(s.dbs) - s.keep; over > 0 {
		evicted = s.dbs[0]
		s.dbs = append(s.dbs[:0], s.dbs[over:]...)
	}
	s.mu.Unlock()

	if evicted != nil {
		_ = evicted.db.Close()
		removeDBFiles(evicted.path)
	}
	return nil
}

func (s *Store) current() *dbFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dbs) == 0 {
		return nil
	}
	return s.dbs[len(s.dbs)-1]
}

func (s *Store) all() []*dbFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*dbFile(nil), s.dbs...)
}

// Processor 返回写入本库的 span 处理器。必须在 tracing.Install 时一起传入：
// provider 按 tracer 名缓存 processor 列表，注册晚了这条链路的 span 会全丢。
func (s *Store) Processor() sdktrace.SpanProcessor {
	return sdktrace.NewBatchSpanProcessor(s, sdktrace.WithBatchTimeout(time.Second))
}

// ExportSpans 实现 sdktrace.SpanExporter。
func (s *Store) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, sp := range spans {
		converted := fromOTelSpan(sp)
		if !s.push(entry{span: &converted}) {
			return nil
		}
	}
	return nil
}

// Shutdown 实现 sdktrace.SpanExporter（真正关闭在 Close）。
func (s *Store) Shutdown(context.Context) error { return nil }

// push 非阻塞入队：队列满说明磁盘跟不上，绝不阻塞业务。
func (s *Store) push(e entry) bool {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return false
	}
	select {
	case s.queue <- e:
		return true
	default:
		return false
	}
}

// Close 停止写入并把缓冲刷完。
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.queue)
	s.mu.Unlock()

	s.wg.Wait()
	for _, f := range s.all() {
		_ = f.db.Close()
	}
	return nil
}

// writer 批量提交：每 200ms 或 500 条落一次盘。
func (s *Store) writer() {
	defer s.wg.Done()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	batch := make([]entry, 0, 500)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.flush(batch)
		batch = batch[:0]
	}
	for {
		select {
		case e, ok := <-s.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= 500 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (s *Store) flush(batch []entry) {
	cur := s.current()
	if cur == nil {
		return
	}
	tx, err := cur.db.Begin()
	if err != nil {
		return
	}
	spanStmt, err := tx.Prepare(`INSERT OR REPLACE INTO span
		(trace_id, span_id, parent_span_id, name, service, protocol, status, status_code, error_msg, start_ns, end_ns, attrs)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return
	}
	logStmt, err := tx.Prepare(`INSERT INTO log
		(ts_ns, level, msg, caller, trace_id, span_id, parent_span_id, attrs) VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return
	}
	var nSpans, nLogs int64
	for _, e := range batch {
		if e.span != nil {
			nSpans++
			attrs, _ := json.Marshal(e.span.Attrs)
			_, _ = spanStmt.Exec(e.span.TraceID, e.span.SpanID, e.span.ParentSpanID, e.span.Name,
				e.span.Service, e.span.Protocol, e.span.Status, e.span.StatusCode, e.span.ErrorMsg,
				e.span.StartNS, e.span.EndNS, string(attrs))
		}
		if e.log != nil {
			nLogs++
			attrs, _ := json.Marshal(e.log.Attrs)
			_, _ = logStmt.Exec(e.log.TimeNS, e.log.Level, e.log.Msg, e.log.Caller,
				e.log.TraceID, e.log.SpanID, e.log.ParentSpanID, string(attrs))
		}
	}
	_ = spanStmt.Close()
	_ = logStmt.Close()
	if err := tx.Commit(); err != nil {
		return
	}
	s.writtenSpans.Add(nSpans)
	s.writtenLogs.Add(nLogs)

	// 写完再看体积：到顶就换新库（本轮最多 keep 个，第 4 个删第 1 个）。
	if info, err := os.Stat(cur.path); err == nil && info.Size() >= s.maxBytes {
		if err := s.rotate(); err != nil {
			slog.Warn("观测库轮转失败，继续写当前文件", "error", err)
		}
	}
}

// Traces 查询一条链路的所有 span（跨本轮所有库文件，按开始时间升序）。
func (s *Store) Traces(ctx context.Context, traceID string) ([]Span, error) {
	var out []Span
	for _, f := range s.all() {
		rows, err := f.db.QueryContext(ctx, `SELECT trace_id, span_id, parent_span_id, name, service, protocol,
			status, status_code, error_msg, start_ns, end_ns, attrs
			FROM span WHERE trace_id = ? ORDER BY start_ns`, traceID)
		if err != nil {
			return nil, err
		}
		spans, err := scanSpans(rows)
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, spans...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartNS < out[j].StartNS })
	return out, nil
}

// Logs 按条件查询日志，返回命中总数与当前页（跨本轮所有库文件合并）。
func (s *Store) Logs(ctx context.Context, f Filter) ([]LogRecord, int64, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	where, args := f.where()

	var all []LogRecord
	var total int64
	for _, file := range s.all() {
		var n int64
		if err := file.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM log "+where, args...).Scan(&n); err != nil {
			return nil, 0, err
		}
		total += n

		// 每个文件都取到 offset+limit，合并后再切片，保证全局排序正确。
		rows, err := file.db.QueryContext(ctx,
			`SELECT ts_ns, level, msg, caller, trace_id, span_id, parent_span_id, attrs FROM log `+where+
				` ORDER BY ts_ns DESC LIMIT ? OFFSET ?`, append(args, f.Offset+f.Limit, 0)...)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var rec LogRecord
			var attrs string
			if err := rows.Scan(&rec.TimeNS, &rec.Level, &rec.Msg, &rec.Caller, &rec.TraceID,
				&rec.SpanID, &rec.ParentSpanID, &attrs); err != nil {
				_ = rows.Close()
				return nil, 0, err
			}
			_ = json.Unmarshal([]byte(attrs), &rec.Attrs)
			all = append(all, rec)
		}
		_ = rows.Close()
	}

	sort.Slice(all, func(i, j int) bool { return all[i].TimeNS > all[j].TimeNS })
	if f.Offset >= len(all) {
		return []LogRecord{}, total, nil
	}
	end := f.Offset + f.Limit
	if end > len(all) {
		end = len(all)
	}
	return all[f.Offset:end], total, nil
}

func (f Filter) where() (string, []any) {
	conds := []string{"1=1"}
	args := []any{}
	if f.SinceNS > 0 {
		conds = append(conds, "ts_ns >= ?")
		args = append(args, f.SinceNS)
	}
	if f.UntilNS > 0 {
		conds = append(conds, "ts_ns <= ?")
		args = append(args, f.UntilNS)
	}
	if f.Level != "" {
		conds = append(conds, "level = ?")
		args = append(args, strings.ToUpper(f.Level))
	}
	if f.TraceID != "" {
		conds = append(conds, "trace_id = ?")
		args = append(args, f.TraceID)
	}
	if f.Keyword != "" {
		conds = append(conds, "(msg LIKE ? OR attrs LIKE ?)")
		like := "%" + f.Keyword + "%"
		args = append(args, like, like)
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

func scanSpans(rows *sql.Rows) ([]Span, error) {
	var out []Span
	for rows.Next() {
		var s Span
		var attrs string
		if err := rows.Scan(&s.TraceID, &s.SpanID, &s.ParentSpanID, &s.Name, &s.Service, &s.Protocol,
			&s.Status, &s.StatusCode, &s.ErrorMsg, &s.StartNS, &s.EndNS, &attrs); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(attrs), &s.Attrs)
		out = append(out, s)
	}
	return out, rows.Err()
}

// removeAllRuns 清掉上次运行留下的库（含 WAL/SHM）。
func removeAllRuns(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "obs-*.db"))
	for _, m := range matches {
		removeDBFiles(m)
	}
}

func removeDBFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

// Counts 返回本轮已写入的日志与 span 总数。
//
// 用计数器而不是 COUNT(*)：概览页 5s 刷一次，对 1.5GB 的观测库反复数全表
// 会把「看板」变成负载来源。轮转删掉旧文件后计数不回退，它表示「这次运行写了多少」。
func (s *Store) Counts(_ context.Context) (logs, spans int64, err error) {
	return s.writtenLogs.Load(), s.writtenSpans.Load(), nil
}

// Dir 返回观测库目录。
func (s *Store) Dir() string { return s.dir }

// Limits 返回单库上限与保留个数（admin 展示用）。
func (s *Store) Limits() (maxBytes int64, keep int) { return s.maxBytes, s.keep }

package obsdb

// 读侧查询：请求列表（按 trace 聚合）与 RED 指标（按入口名字聚合）。
//
// 两条纪律贯穿本文件：
//  1. 只读。观测库里的东西丢了业务照跑，查询再慢也只是页面慢，所以不需要缓存/物化表。
//  2. 跨本轮所有库文件合并。轮转（单库 500MB）对使用者透明：每个文件各查一遍，
//     在 Go 里按全局顺序合并。分页/排序的正确性靠「每个文件取 offset+limit 条候选」保证。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Window 是观测查询统一的时间窗：Unix 纳秒，闭区间 [FromNS, UntilNS]。
type Window struct {
	FromNS  int64
	UntilNS int64
}

// isEntryExpr 判断一条 span 是不是这次请求的入口（根）：
//
//   - 没有父 span：浏览器/App/插件直连，本进程就是链路的根；
//   - 或者是 HTTP 网关那条：上游透传了 traceparent 时根 span 会带一个外部 parent，
//     但它仍然是这次请求的入口。
//
// 进程内 gRPC 子 span 两条都不满足（有本地父 span；HTTP 只由 ingress 产生）。
func isEntryExpr(alias string) string {
	p := alias + "."
	return fmt.Sprintf(`(%[1]sparent_span_id = '' OR (%[1]sprotocol = 'HTTP' AND %[1]sservice = 'gateway'))`, p)
}

// httpEntryPrefixes 是「非 grpc-gateway 的 HTTP 业务入口」前缀。
//
// 新增一个这样的入口时只改这里：requestEntryScope 的 SQL 与 FoldUnmatchedEntry
// （网关把未匹配路由折叠成 <unmatched> 的判定）都从这里取，两边不会各写一份。
var httpEntryPrefixes = []string{"/grpc-gateway/", "/service/api/", "/api/"}

// requestEntryScope 是「一次业务请求」的入口口径：只认 gRPC 与 HTTP 业务接口。
//
// 排除静态资源、SPA 页面与 WS/SSE 长连接：
//   - 浏览器加载一个页面会产生几十条静态请求，它们会稀释真正的 QPS；
//   - 长连接的 span 时长是整个连接的生命周期（小时级），会把 P99 直接带偏。
//
// 未匹配路由的探测（入口名折叠为 UnmatchedEntry，见 FoldUnmatchedEntry）不管原始路径是什么都算进来：
// 它们也是打进来的请求，扫描量本身就是要看的信号；不算会让「被扫」完全隐形。
//
// 漏加前缀只会让该入口不出现在请求/RED 页面（日志与调用链仍可查），不会算错别的口径。
func requestEntryScope(alias string) string {
	p := alias + "."
	conds := []string{fmt.Sprintf("%sname LIKE '/%%'", p)} // gRPC 直连入口（名字以 / 开头）
	for _, prefix := range httpEntryPrefixes {
		conds = append(conds, fmt.Sprintf("%sname LIKE '%% %s%%'", p, prefix))
	}
	// 折叠后的探测行：名字已不是原始路径，靠固定值认。
	conds = append(conds, p+"name = "+unmatchedEntryLiteral())
	return fmt.Sprintf(`(%s AND (%s))`, isEntryExpr(alias), strings.Join(conds, " OR "))
}

// serverStatusCodeExpr 是「HTTP 状态码落在 5xx」这一条事实，serverErrorExpr 与
// clientErrorExpr 都从它派生，保证两个口径互斥且覆盖 4xx/5xx 全集。
func serverStatusCodeExpr(alias string) string {
	return fmt.Sprintf(`%[1]s.protocol = 'HTTP' AND CAST(%[1]s.status_code AS INTEGER) >= 500`, alias)
}

func clientStatusCodeExpr(alias string) string {
	return fmt.Sprintf(`%[1]s.protocol = 'HTTP' AND CAST(%[1]s.status_code AS INTEGER) >= 400
		AND CAST(%[1]s.status_code AS INTEGER) < 500`, alias)
}

// errorExpr 判断一条 span 是否算「服务端错误」——看板上的错误率只认这一条。
//
// 网关的 5xx 不一定把 span 标成 ERROR（错误是写响应头，不是 return err），
// 所以只看 status 会把「接口 500」算成成功；这里按状态码兜底：
// HTTP >= 500，gRPC 非 0（gRPC 只在失败时才写 status_code）。
//
// span status=ERROR 但不属于上面两条的（典型是网关把客户端错误翻成 err 后 RecordError，
// 也可能是 4xx 之外的业务拒绝），如果不排除，404 会被算成服务端错误；
// 所以「ERROR 且不是 HTTP 4xx」才算，HTTP 4xx 一律归 clientErrorExpr。
func errorExpr(alias string) string {
	return fmt.Sprintf(`(%[1]s.status = 'ERROR' AND NOT (%[2]s)
		OR (%[3]s)
		OR (%[1]s.protocol = 'GRPC' AND %[1]s.status_code <> '' AND %[1]s.status_code <> '0'))`,
		alias, clientStatusCodeExpr(alias), serverStatusCodeExpr(alias))
}

// clientErrorExpr 判断一条 span 是否算「客户端错误」（HTTP 4xx）：扫描器探测、鉴权失败、
// 请求体不合法都在这里。它是独立指标，不参与 errorExpr——把 404 算进错误率
// 会让「被扫」看起来像「服务坏了」。
func clientErrorExpr(alias string) string {
	return fmt.Sprintf(`(%s)`, clientStatusCodeExpr(alias))
}

// unmatchedPathExpr 取折叠后保留的真实路径（见 UnmatchedPathAttr）。
//
// attrs 是 span 属性的 JSON 字面量，用 json_extract 取一个键（现代 SQLite 编译带了 JSON1）；
// 键名带点，所以用 $."key" 的引号形式，不然会被当成两层路径。
func unmatchedPathExpr(alias string) string {
	return fmt.Sprintf(`COALESCE(json_extract(NULLIF(%[1]s.attrs, ''), '$."%[2]s"'), '')`,
		alias, UnmatchedPathAttr)
}

// UnmatchedEntry 是「没匹配到任何 HTTP 路由」的入口名。
//
// 公网扫描器每天打出几千个不存在的路径，每个都会在请求列表/RED 里占一行；
// 网关在请求结束时把这类 span 的名字改成它、真实路径写进 UnmatchedPathAttr，
// 于是所有探测折叠成一行，计数还在，真实接口不再被淹掉。
const UnmatchedEntry = "<unmatched>"

// UnmatchedPathAttr 是折叠后保留真实路径的 span 属性名（截断见网关）。
const UnmatchedPathAttr = "http.unmatched_path"

// FoldUnmatchedEntry 决定一个「没匹配到任何 gin 路由」的请求要不要把入口名折叠成 UnmatchedEntry，
// 要折叠时返回折叠后的名字与 true。
//
// 只折叠落在业务入口前缀下的路径（httpEntryPrefixes）：只有它们会进请求/RED 口径，也正好是扫描噪声的来源。
// SPA 页面路由与静态资源不折叠——它们本来就被 requestEntryScope 排除，折叠了反而会把页面加载/资源请求
// 塞进 <unmatched> 行，把 QPS 带偏。真实路径由网关写进 UnmatchedPathAttr，库里的 name 仍是原始路径（审计需要）。
func FoldUnmatchedEntry(urlPath string) (string, bool) {
	for _, prefix := range httpEntryPrefixes {
		if strings.HasPrefix(urlPath, prefix) {
			return UnmatchedEntry, true
		}
	}
	return "", false
}

// unmatchedEntryLiteral 是 UnmatchedEntry 的 SQL 字面量（不可参数化：它在聚合子查询里）。
func unmatchedEntryLiteral() string { return "'" + UnmatchedEntry + "'" }

// TraceFilter 是「请求列表」的查询条件。筛选在 SQL 里做，分页在合并后做。
type TraceFilter struct {
	Window        Window
	Service       string // 主服务（链路里最早 GRPC span 的 service）
	Keyword       string // 入口名称模糊匹配（含未匹配路由的真实路径）
	Status        string // SUCCESS / ERROR / CLIENT_ERROR
	StatusCode    string // 精确状态码
	MinDurationNS int64
	SortBy        string // time | duration | status
	ExcludeProbes bool   // 排除未匹配路由的探测请求（网关折叠后的 <unmatched>）
	Limit, Offset int
	SkipStats     bool // 只要列表/总数时跳过按服务的分布（概览页的慢请求计数）
	CountOnly     bool // 只要 total，不取列表（概览页的慢请求计数）
}

// TraceRow 是请求列表的一行（按 trace 聚合）。
type TraceRow struct {
	TraceID        string
	Entry          string // 根 span 名称（未匹配路由时为 UnmatchedEntry）
	Path           string // 根 span 的真实路径（未匹配路由时取 span 属性，其余同 Entry 的路径段）
	Service        string // 主服务
	Protocol       string
	Status         string // SUCCESS / ERROR / CLIENT_ERROR（按状态码兜底后的口径）
	StatusCode     string
	StartNS        int64
	EndNS          int64
	SpanCount      int64
	ErrorSpans     int64 // 只含服务端错误
	ClientErrSpans int64
	LogCount       int64
	RootError      int64
}

// TraceServiceStat 是窗口内按主服务的分布（服务筛选下拉 + 一眼看慢在哪）。
type TraceServiceStat struct {
	Service          string
	Count            int64
	ErrorCount       int64 // 服务端错误（5xx / gRPC 非 0）
	ClientErrorCount int64 // 客户端错误（4xx）
	AvgMS            float64
	MaxMS            float64
}

// TracePage 是请求列表的一页 + 按服务的分布。
type TracePage struct {
	Rows     []TraceRow
	Total    int64
	Services []TraceServiceStat
}

// candidateCap 限制每个库文件返回的候选行数。
//
// 深翻页本来就不该是排障手段（缩小时间窗 / 加筛选更快），这里给一个上界，
// 防止一次查询把几 MB 的行搬进内存。
const candidateCap = 5000

// aggregateSQL 生成「按 trace 聚合」的子查询。withService=false 时省掉
// main_service 的关联子查询（只算 total 时不需要它）。
func (f TraceFilter) aggregateSQL(withService bool) (string, []any) {
	mainService := "''"
	if withService {
		// 链路里最早出现的 GRPC span 就是这次请求进入的业务服务：
		// 网关那条是 HTTP，真正的域调用从 GRPC 开始；没有 GRPC 子 span 时为空。
		mainService = `COALESCE((SELECT g.service FROM span g
			WHERE g.trace_id = s.trace_id AND g.protocol = 'GRPC'
			ORDER BY g.start_ns LIMIT 1), '')`
	}
	sql := fmt.Sprintf(`SELECT s.trace_id AS trace_id,
			MIN(s.start_ns) AS start_ns,
			MAX(s.end_ns) AS end_ns,
			COUNT(DISTINCT s.span_id) AS span_count,
			SUM(CASE WHEN %[1]s THEN 1 ELSE 0 END) AS error_spans,
			SUM(CASE WHEN %[5]s THEN 1 ELSE 0 END) AS client_error_spans,
			COALESCE(MAX(CASE WHEN %[4]s THEN s.name END), '') AS entry,
			COALESCE(MAX(CASE WHEN %[4]s AND s.name = %[6]s THEN %[7]s END), '') AS unmatched_path,
			COALESCE(MAX(CASE WHEN %[4]s THEN s.service END), '') AS root_service,
			COALESCE(MAX(CASE WHEN %[4]s THEN s.protocol END), '') AS protocol,
			COALESCE(MAX(CASE WHEN %[4]s THEN s.status_code END), '') AS status_code,
			MAX(CASE WHEN %[4]s AND %[1]s THEN 1 ELSE 0 END) AS root_error,
			MAX(CASE WHEN %[4]s AND %[5]s THEN 1 ELSE 0 END) AS root_client_error,
			%[2]s AS main_service
		FROM span s
		WHERE s.trace_id IN (
			SELECT DISTINCT r.trace_id FROM span r
			WHERE %[3]s AND r.start_ns >= ? AND r.start_ns <= ?
		)
		GROUP BY s.trace_id`,
		errorExpr("s"), mainService, requestEntryScope("r"), isEntryExpr("s"),
		clientErrorExpr("s"), unmatchedEntryLiteral(), unmatchedPathExpr("s"))
	return sql, []any{f.Window.FromNS, f.Window.UntilNS}
}

// whereSQL 在聚合结果上套筛选；返回值已按 SQL 文本顺序排好参数。
func (f TraceFilter) whereSQL() (string, []any) {
	conds := []string{"1=1"}
	var args []any
	// 三态：服务端错误 / 客户端错误 / 成功（两者都不是）。互斥且覆盖全集。
	switch strings.ToUpper(f.Status) {
	case "ERROR":
		conds = append(conds, "root_error = 1")
	case "CLIENT_ERROR":
		conds = append(conds, "root_error = 0 AND root_client_error = 1")
	case "SUCCESS":
		conds = append(conds, "root_error = 0 AND root_client_error = 0")
	}
	if f.ExcludeProbes {
		conds = append(conds, "entry <> "+unmatchedEntryLiteral())
	}
	if f.StatusCode != "" {
		conds = append(conds, "status_code = ?")
		args = append(args, f.StatusCode)
	}
	if f.Service != "" {
		conds = append(conds, "main_service = ?")
		args = append(args, f.Service)
	}
	if f.Keyword != "" {
		conds = append(conds, `(entry LIKE ? ESCAPE '\' OR unmatched_path LIKE ? ESCAPE '\')`)
		pattern := likePattern(f.Keyword)
		args = append(args, pattern, pattern)
	}
	if f.MinDurationNS > 0 {
		conds = append(conds, "end_ns - start_ns >= ?")
		args = append(args, f.MinDurationNS)
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

func (f TraceFilter) orderSQL() string {
	switch f.SortBy {
	case "duration":
		return "(end_ns - start_ns) DESC, start_ns DESC, trace_id"
	case "status":
		// 服务端错误优先，其次客户端错误，最后成功（“错误优先”仍然成立，但 4xx 不再冒充故障）。
		return "root_error DESC, root_client_error DESC, start_ns DESC, trace_id"
	default:
		return "start_ns DESC, trace_id"
	}
}

// RequestTraces 返回一页请求（按 trace 聚合）+ 按主服务的分布，跨本轮所有库文件合并。
//
// 分页口径：每个库文件各取 (offset+limit) 条候选，合并去重后再截取 offset..offset+limit。
// 这是分布式 top-K 的标准做法：全局第 K 名一定出现在某个文件的局部前 K 名里。
func (s *Store) RequestTraces(ctx context.Context, f TraceFilter) (TracePage, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	want := f.Offset + f.Limit
	if want > candidateCap {
		want = candidateCap
	}

	agg, base := f.aggregateSQL(true)
	where, filterArgs := f.whereSQL()
	files := s.all()

	// total 不需要 main_service 列：只有按服务筛选时才把关联子查询带上。
	countAgg, countBase := f.aggregateSQL(f.Service != "")

	var total int64
	rows := make(map[string]*TraceRow)
	stats := make(map[string]*TraceServiceStat)

	for _, file := range files {
		var n int64
		countSQL := "SELECT COUNT(*) FROM (" + countAgg + ") " + where
		if err := file.db.QueryRowContext(ctx, countSQL, append(append([]any{}, countBase...), filterArgs...)...).Scan(&n); err != nil {
			return TracePage{}, err
		}
		total += n

		if !f.CountOnly {
			listSQL := "SELECT trace_id, start_ns, end_ns, span_count, error_spans, client_error_spans, entry, unmatched_path, root_service, protocol, status_code, root_error, root_client_error, main_service FROM (" + agg + ") " + where +
				" ORDER BY " + f.orderSQL() + " LIMIT ?"
			listArgs := append(append(append([]any{}, base...), filterArgs...), want)
			rs, err := file.db.QueryContext(ctx, listSQL, listArgs...)
			if err != nil {
				return TracePage{}, err
			}
			for rs.Next() {
				var r TraceRow
				var rootService, unmatchedPath string
				var rootClientErr int64
				if err := rs.Scan(&r.TraceID, &r.StartNS, &r.EndNS, &r.SpanCount, &r.ErrorSpans, &r.ClientErrSpans,
					&r.Entry, &unmatchedPath, &rootService, &r.Protocol, &r.StatusCode, &r.RootError, &rootClientErr, &r.Service); err != nil {
					_ = rs.Close()
					return TracePage{}, err
				}
				if r.Service == "" {
					r.Service = rootService
				}
				// 真实路径：折叠行用 span 属性里存的原始路径，其余行就是入口名的路径段。
				r.Path = unmatchedPath
				if r.Path == "" {
					if _, p := splitEntryName(r.Entry); p != "" {
						r.Path = p
					}
				}
				switch {
				case r.RootError != 0:
					r.Status = "ERROR"
				case rootClientErr != 0:
					r.Status = "CLIENT_ERROR"
				default:
					r.Status = "SUCCESS"
				}
				if kept, ok := rows[r.TraceID]; ok {
					keepTraceRow(kept, r)
					continue
				}
				copied := r
				rows[r.TraceID] = &copied
			}
			if err := rs.Err(); err != nil {
				_ = rs.Close()
				return TracePage{}, err
			}
			_ = rs.Close()
		}

		// 服务分布：同一份聚合 + 同样的筛选，按主服务分组。
		if f.SkipStats {
			continue
		}
		statSQL := "SELECT main_service, COUNT(*), SUM(root_error), SUM(CASE WHEN root_error = 0 AND root_client_error = 1 THEN 1 ELSE 0 END), AVG(end_ns - start_ns), MAX(end_ns - start_ns) FROM (" + agg + ") " + where +
			" AND main_service <> '' GROUP BY main_service ORDER BY 2 DESC LIMIT 30"
		statArgs := append(append([]any{}, base...), filterArgs...)
		srs, err := file.db.QueryContext(ctx, statSQL, statArgs...)
		if err != nil {
			return TracePage{}, err
		}
		for srs.Next() {
			var st TraceServiceStat
			var avgNS, maxNS float64
			if err := srs.Scan(&st.Service, &st.Count, &st.ErrorCount, &st.ClientErrorCount, &avgNS, &maxNS); err != nil {
				_ = srs.Close()
				return TracePage{}, err
			}
			st.AvgMS = avgNS / 1e6
			st.MaxMS = maxNS / 1e6
			if kept, ok := stats[st.Service]; ok {
				kept.Count += st.Count
				kept.ErrorCount += st.ErrorCount
				kept.ClientErrorCount += st.ClientErrorCount
				// 平均值按条数加权；最大值取大。
				kept.AvgMS = (kept.AvgMS*float64(kept.Count-st.Count) + st.AvgMS*float64(st.Count)) / float64(kept.Count)
				if st.MaxMS > kept.MaxMS {
					kept.MaxMS = st.MaxMS
				}
				continue
			}
			copied := st
			stats[st.Service] = &copied
		}
		if err := srs.Err(); err != nil {
			_ = srs.Close()
			return TracePage{}, err
		}
		_ = srs.Close()
	}

	out := make([]TraceRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sortTraceRows(out, f.SortBy)
	if f.Offset >= len(out) {
		out = []TraceRow{}
	} else {
		end := f.Offset + f.Limit
		if end > len(out) {
			end = len(out)
		}
		out = out[f.Offset:end]
	}

	if len(out) > 0 {
		ids := make([]any, len(out))
		counts := make(map[string]int64, len(out))
		for i := range out {
			ids[i] = out[i].TraceID
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		for _, file := range files {
			lrs, err := file.db.QueryContext(ctx,
				"SELECT trace_id, COUNT(*) FROM log WHERE trace_id IN ("+marks+") GROUP BY trace_id", ids...)
			if err != nil {
				return TracePage{}, err
			}
			for lrs.Next() {
				var id string
				var n int64
				if err := lrs.Scan(&id, &n); err != nil {
					_ = lrs.Close()
					return TracePage{}, err
				}
				counts[id] += n
			}
			if err := lrs.Err(); err != nil {
				_ = lrs.Close()
				return TracePage{}, err
			}
			_ = lrs.Close()
		}
		for i := range out {
			out[i].LogCount = counts[out[i].TraceID]
		}
	}

	page := TracePage{Rows: out, Total: total}
	for _, st := range stats {
		page.Services = append(page.Services, *st)
	}
	sort.Slice(page.Services, func(i, j int) bool {
		if page.Services[i].Count != page.Services[j].Count {
			return page.Services[i].Count > page.Services[j].Count
		}
		return page.Services[i].Service < page.Services[j].Service
	})
	return page, nil
}

// keepTraceRow 合并同一条链路在两个库文件里的片段（轮转刚好切在请求中间）。
func keepTraceRow(dst *TraceRow, src TraceRow) {
	if src.StartNS < dst.StartNS {
		dst.StartNS = src.StartNS
	}
	if src.EndNS > dst.EndNS {
		dst.EndNS = src.EndNS
	}
	dst.SpanCount += src.SpanCount
	dst.ErrorSpans += src.ErrorSpans
	dst.ClientErrSpans += src.ClientErrSpans
	dst.RootError += src.RootError
	if src.Status == "CLIENT_ERROR" && dst.Status == "SUCCESS" {
		dst.Status = "CLIENT_ERROR"
	}
	if dst.RootError != 0 {
		dst.Status = "ERROR"
	}
	if dst.Entry == "" {
		dst.Entry = src.Entry
	}
	if dst.Path == "" {
		dst.Path = src.Path
	}
	if dst.Service == "" {
		dst.Service = src.Service
	}
	if dst.Protocol == "" {
		dst.Protocol = src.Protocol
	}
	if dst.StatusCode == "" {
		dst.StatusCode = src.StatusCode
	}
}

// splitEntryName 把入口名拆成「方法 + 路径」：HTTP 是 "POST /grpc-gateway/..."，
// gRPC 直连是 "/proto.x.Y/Z"（没有方法段）。obsdb 只用于回填 TraceRow.Path。
func splitEntryName(entry string) (method, path string) {
	if strings.HasPrefix(entry, "/") {
		return "", entry
	}
	if i := strings.IndexByte(entry, ' '); i > 0 {
		return entry[:i], entry[i+1:]
	}
	return "", entry
}

// sortTraceRows 排序。status 口径：服务端错误 > 客户端错误 > 成功（4xx 不再冒充 5xx 抬到最前）。
// 未匹配路由的行由前端标识，不在这里改排序（时间轴仍按时间倒序）。
func sortTraceRows(rows []TraceRow, sortBy string) {
	rank := func(r TraceRow) int {
		switch {
		case r.RootError != 0:
			return 2
		case r.Status == "CLIENT_ERROR":
			return 1
		}
		return 0
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch sortBy {
		case "duration":
			da, db := a.EndNS-a.StartNS, b.EndNS-b.StartNS
			if da != db {
				return da > db
			}
		case "status":
			if ra, rb := rank(a), rank(b); ra != rb {
				return ra > rb
			}
		}
		if a.StartNS != b.StartNS {
			return a.StartNS > b.StartNS
		}
		return a.TraceID < b.TraceID
	})
}

// RedQuery 是 RED 指标的查询条件。
type RedQuery struct {
	Window  Window
	Buckets int // 趋势分桶数，<=0 时按窗口长度取默认值
}

// RedPoint 是趋势里的一个桶。
//
// ErrorCount 只含服务端错误（5xx / gRPC 非 0），ClientErrorCount 只含 HTTP 4xx；
// 两者只影响指标语义，分位数/计数的口径不变（4xx 仍算一次请求）。
type RedPoint struct {
	StartNS          int64
	Count            int64
	ErrorCount       int64
	ClientErrorCount int64
	P95MS            float64
	P99MS            float64
}

// RedSeries 是一个入口名字的 RED 汇总 + 趋势。
type RedSeries struct {
	Name             string
	Service          string
	Protocol         string
	Count            int64
	ErrorCount       int64 // 只含服务端错误（5xx / gRPC 非 0）
	ClientErrorCount int64 // 只含客户端错误（HTTP 4xx）
	AvgMS            float64
	MaxMS            float64
	P50MS            float64
	P95MS            float64
	P99MS            float64
	Points           []RedPoint
}

// RedReport 是 RED 页面/概览的一屏数据。
type RedReport struct {
	Series   []RedSeries
	Total    RedSeries
	BucketNS int64
	Dropped  int // 因基数上限被折叠的系列数
}

// redAcc 是一个系列（或全局）的累加器：窗口汇总 + 每桶耗时直方图。
//
// minNS/maxNS 是这一组的精确极值，bucketMinNS/bucketMaxNS 是每个时间桶的精确极值；
// 分位数靠它们把插值区间收紧到实测范围（见 histogramQuantile）。
type redAcc struct {
	count, errs, clientErrs, sumNS int64
	minNS, maxNS                   int64
	hist                           [][]int64 // [时间桶][耗时桶]
	bucketCount                    []int64
	bucketMinNS                    []int64
	bucketMaxNS                    []int64
	errByBucket                    []int64
	clientErrByBucket              []int64
}

func newRedAcc(buckets int) *redAcc {
	a := &redAcc{
		hist:              make([][]int64, buckets),
		bucketCount:       make([]int64, buckets),
		bucketMinNS:       make([]int64, buckets),
		bucketMaxNS:       make([]int64, buckets),
		errByBucket:       make([]int64, buckets),
		clientErrByBucket: make([]int64, buckets),
	}
	for i := range a.hist {
		a.hist[i] = make([]int64, len(redBoundsMS)+1)
	}
	return a
}

// add 合并一批 SQL 聚合行（同一个 [时间桶, 耗时桶]）。count 恒 >= 1，
// 所以「此前计数为 0」就是可直接取本行极值的第一行（全局看 a.count，每个时间桶看 bucketCount）。
func (a *redAcc) add(tb, bucket, count, errs, clientErrs, sumNS, minNS, maxNS int64) {
	if a.count == 0 {
		a.minNS, a.maxNS = minNS, maxNS
	} else {
		if minNS < a.minNS {
			a.minNS = minNS
		}
		if maxNS > a.maxNS {
			a.maxNS = maxNS
		}
	}
	a.count += count
	a.errs += errs
	a.clientErrs += clientErrs
	a.sumNS += sumNS

	if a.bucketCount[tb] == 0 {
		a.bucketMinNS[tb], a.bucketMaxNS[tb] = minNS, maxNS
	} else {
		if minNS < a.bucketMinNS[tb] {
			a.bucketMinNS[tb] = minNS
		}
		if maxNS > a.bucketMaxNS[tb] {
			a.bucketMaxNS[tb] = maxNS
		}
	}
	a.bucketCount[tb] += count
	a.hist[tb][bucket] += count
	a.errByBucket[tb] += errs
	a.clientErrByBucket[tb] += clientErrs
}

// redBoundsMS 是耗时直方图的桶上界（毫秒，le 语义），末桶为 +Inf。
//
// 与 Prometheus 默认桶（szrw_rpc_duration_seconds）同一套刻度，只是换成毫秒：
// 分位数由这张直方图插值（histogram_quantile 同法），但插值区间收紧到实测 [min, max]，
// 所以误差不超过一个桶宽，且永远不会越过该组的 max。
var redBoundsMS = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// redSeriesCap 防止高基数入口名（攻击者构造 /grpc-gateway/<随机串>）把页面撑爆。
const redSeriesCap = 200

func durationBucketExpr(alias string) string {
	var b strings.Builder
	b.WriteString("CASE")
	for i, le := range redBoundsMS {
		fmt.Fprintf(&b, " WHEN (%s.end_ns - %s.start_ns) <= %d THEN %d", alias, alias, int64(le)*int64(time.Millisecond), i)
	}
	fmt.Fprintf(&b, " ELSE %d END", len(redBoundsMS))
	return b.String()
}

// RED 返回时间窗内的 Rate / Error / Duration 指标，按入口名字（根 span 的 name）分组。
//
// 口径：
//   - 只统计「请求入口」的根 span（见 requestEntryScope），一次请求一条，QPS 不被内部 RPC 放大；
//   - count/avg/max 为精确值；p50/p95/p99 由耗时直方图在实测 [min, max] 内
//     插值估算（见 histogramQuantile），小样本因此退化成「区间内插值」而不是桶边界外推；
//   - ErrorCount 只含服务端错误（HTTP 5xx / gRPC 非 0），ClientErrorCount 只含 HTTP 4xx；
//     4xx 仍是一次请求（进 Count 与分位数），只是不算故障；
//   - 趋势的桶宽由窗口长度决定，桶内 p95/p99 用的是该时间桶自己的 [min, max]。
func (s *Store) RED(ctx context.Context, q RedQuery) (RedReport, error) {
	from, until := q.Window.FromNS, q.Window.UntilNS
	if until <= from {
		return RedReport{}, nil
	}
	buckets := q.Buckets
	if buckets <= 0 {
		switch span := until - from; {
		case span <= int64(15*time.Minute):
			buckets = 15
		case span <= int64(time.Hour):
			buckets = 12
		case span <= int64(6*time.Hour):
			buckets = 24
		case span <= int64(24*time.Hour):
			buckets = 48
		default:
			buckets = 56
		}
	}
	if buckets > 240 {
		buckets = 240
	}
	bucketNS := (until - from) / int64(buckets)
	if bucketNS <= 0 {
		bucketNS = 1
	}

	type accKey struct{ name, service, protocol string }
	series := make(map[accKey]*redAcc)
	total := newRedAcc(buckets)

	sql := fmt.Sprintf(`SELECT s.name, s.service, s.protocol,
			CAST((s.start_ns - ?) / ? AS INTEGER) AS tb,
			%s AS bucket,
			COUNT(*) AS c,
			SUM(CASE WHEN %s THEN 1 ELSE 0 END) AS e,
			SUM(CASE WHEN %s THEN 1 ELSE 0 END) AS ce,
			SUM(s.end_ns - s.start_ns) AS sum_ns,
			MIN(s.end_ns - s.start_ns) AS min_ns,
			MAX(s.end_ns - s.start_ns) AS max_ns
		FROM span s
		WHERE %s AND s.start_ns >= ? AND s.start_ns <= ?
		GROUP BY s.name, s.service, s.protocol, tb, bucket`,
		durationBucketExpr("s"), errorExpr("s"), clientErrorExpr("s"), requestEntryScope("s"))

	for _, file := range s.all() {
		rows, err := file.db.QueryContext(ctx, sql, from, bucketNS, from, until)
		if err != nil {
			return RedReport{}, err
		}
		for rows.Next() {
			var name, service, protocol string
			var tb, bucket, count, errs, clientErrs, sumNS, minNS, maxNS int64
			if err := rows.Scan(&name, &service, &protocol, &tb, &bucket, &count, &errs, &clientErrs, &sumNS, &minNS, &maxNS); err != nil {
				_ = rows.Close()
				return RedReport{}, err
			}
			if tb < 0 || tb >= int64(buckets) || bucket < 0 || bucket > int64(len(redBoundsMS)) {
				continue
			}
			key := accKey{name, service, protocol}
			a := series[key]
			if a == nil {
				a = newRedAcc(buckets)
				series[key] = a
			}
			a.add(tb, bucket, count, errs, clientErrs, sumNS, minNS, maxNS)
			total.add(tb, bucket, count, errs, clientErrs, sumNS, minNS, maxNS)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return RedReport{}, err
		}
		_ = rows.Close()
	}

	build := func(a *redAcc, name, service, protocol string) RedSeries {
		out := RedSeries{Name: name, Service: service, Protocol: protocol,
			Count: a.count, ErrorCount: a.errs, ClientErrorCount: a.clientErrs}
		if a.count > 0 {
			out.AvgMS = float64(a.sumNS) / float64(a.count) / 1e6
		}
		out.MaxMS = float64(a.maxNS) / 1e6
		all := make([]int64, len(redBoundsMS)+1)
		for i := range a.hist {
			for j, c := range a.hist[i] {
				all[j] += c
			}
		}
		out.P50MS, out.P95MS, out.P99MS = redQuantiles(all, float64(a.minNS)/1e6, out.MaxMS)
		out.Points = make([]RedPoint, buckets)
		for i := range a.hist {
			_, p95, p99 := redQuantiles(a.hist[i], float64(a.bucketMinNS[i])/1e6, float64(a.bucketMaxNS[i])/1e6)
			out.Points[i] = RedPoint{
				StartNS:          from + int64(i)*bucketNS,
				Count:            a.bucketCount[i],
				ErrorCount:       a.errByBucket[i],
				ClientErrorCount: a.clientErrByBucket[i],
				P95MS:            p95,
				P99MS:            p99,
			}
		}
		return out
	}

	report := RedReport{BucketNS: bucketNS, Total: build(total, "ALL", "", "")}
	keys := make([]accKey, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ci, cj := series[keys[i]].count, series[keys[j]].count
		if ci != cj {
			return ci > cj
		}
		if keys[i].name != keys[j].name {
			return keys[i].name < keys[j].name
		}
		return keys[i].service < keys[j].service
	})
	if len(keys) > redSeriesCap {
		report.Dropped = len(keys) - redSeriesCap
		keys = keys[:redSeriesCap]
	}
	for _, k := range keys {
		report.Series = append(report.Series, build(series[k], k.name, k.service, k.protocol))
	}
	return report, nil
}

// redQuantiles 返回一组的三条分位线，并保证 0 <= p50 <= p95 <= p99 <= max。
func redQuantiles(counts []int64, minMS, maxMS float64) (p50, p95, p99 float64) {
	p50 = histogramQuantile(0.50, counts, minMS, maxMS)
	p95 = histogramQuantile(0.95, counts, minMS, maxMS)
	p99 = histogramQuantile(0.99, counts, minMS, maxMS)
	if p95 < p50 {
		p95 = p50
	}
	if p99 < p95 {
		p99 = p95
	}
	return p50, p95, p99
}

// histogramQuantile 按 Prometheus histogram_quantile 的方式在桶内线性插值，
// 但插值区间不是桶边界，而是桶边界与该组实测 [minMS, maxMS] 的交集。
//
// 为什么：桶只记 le 上界，(100,250] 里的样本可能全都贴着 250，也可能全都贴着 100。
// 1/2/3 条样本落在这个桶里时，按桶边界插值会把这个不确定性放大成「P95 > Max」
// （实测：3 条都在 (100,250]、max=160.08 → p95=242.5、p99=248.5）。
// 用实测极值收紧区间后：样本全在一个桶里时区间就退化成真实支撑 [min, max]，
// count=1 时三条分位线精确等于 max；跨桶时与 histogram_quantile 一致，只是被 max 封顶。
//
// counts 是各耗时桶的计数（末桶 = 大于最后一个上界），minMS/maxMS 是该组实测最小/最大值。
func histogramQuantile(q float64, counts []int64, minMS, maxMS float64) float64 {
	var total int64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	if minMS < 0 {
		minMS = 0
	}
	if maxMS < minMS {
		maxMS = minMS
	}

	rank := q * float64(total)
	var cum int64
	lower := 0.0
	for i, c := range counts {
		if float64(cum)+float64(c) >= rank {
			// 桶 i 覆盖 (lower, redBoundsMS[i]]，末桶没有上界。
			upper := maxMS
			if i < len(redBoundsMS) {
				upper = min(redBoundsMS[i], maxMS)
			}
			lower = max(lower, minMS)
			if upper < lower {
				upper = lower
			}
			if c <= 0 {
				return upper
			}
			frac := (rank - float64(cum)) / float64(c)
			frac = min(max(frac, 0), 1)
			return min(lower+(upper-lower)*frac, maxMS)
		}
		cum += c
		if i < len(redBoundsMS) {
			lower = redBoundsMS[i]
		}
	}
	return maxMS
}

// likePattern 把用户输入变成 LIKE 模式（转义 %、_ 与转义符本身）。
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

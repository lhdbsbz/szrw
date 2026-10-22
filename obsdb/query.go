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

// requestEntryScope 是「一次业务请求」的入口口径：只认 gRPC 与 HTTP 业务接口。
//
// 排除静态资源、SPA 页面与 WS/SSE 长连接：
//   - 浏览器加载一个页面会产生几十条静态请求，它们会稀释真正的 QPS；
//   - 长连接的 span 时长是整个连接的生命周期（小时级），会把 P99 直接带偏。
//
// 新增「非 grpc-gateway 的 HTTP 入口」时，把前缀加到这里；漏加只会让该入口
// 不出现在请求/RED 页面（日志与调用链仍可查），不会算错别的口径。
func requestEntryScope(alias string) string {
	p := alias + "."
	return fmt.Sprintf(`(%[1]s
		AND (%[2]sname LIKE '/%%'
			OR %[2]sname LIKE '%% /grpc-gateway/%%'
			OR %[2]sname LIKE '%% /service/api/%%'
			OR %[2]sname LIKE '%% /api/%%'))`, isEntryExpr(alias), p)
}

// errorExpr 判断一条 span 是否算失败。
//
// 网关的 5xx 不一定把 span 标成 ERROR（错误是写响应头，不是 return err），
// 所以只看 status 会把「接口 500」算成成功；这里按状态码兜底：
// HTTP >= 400，gRPC 非 0（gRPC 只在失败时才写 status_code）。
func errorExpr(alias string) string {
	return fmt.Sprintf(`(%[1]s.status = 'ERROR'
		OR (%[1]s.protocol = 'HTTP' AND CAST(%[1]s.status_code AS INTEGER) >= 400)
		OR (%[1]s.protocol = 'GRPC' AND %[1]s.status_code <> '' AND %[1]s.status_code <> '0'))`,
		alias)
}

// TraceFilter 是「请求列表」的查询条件。筛选在 SQL 里做，分页在合并后做。
type TraceFilter struct {
	Window        Window
	Service       string // 主服务（链路里最早 GRPC span 的 service）
	Keyword       string // 入口名称模糊匹配
	Status        string // SUCCESS / ERROR
	StatusCode    string // 精确状态码
	MinDurationNS int64
	SortBy        string // time | duration | status
	Limit, Offset int
	SkipStats     bool // 只要列表/总数时跳过按服务的分布（概览页的慢请求计数）
	CountOnly     bool // 只要 total，不取列表（概览页的慢请求计数）
}

// TraceRow 是请求列表的一行（按 trace 聚合）。
type TraceRow struct {
	TraceID    string
	Entry      string // 根 span 名称
	Service    string // 主服务
	Protocol   string
	Status     string // SUCCESS / ERROR（按状态码兜底后的口径）
	StatusCode string
	StartNS    int64
	EndNS      int64
	SpanCount  int64
	ErrorSpans int64
	LogCount   int64
	RootError  int64
}

// TraceServiceStat 是窗口内按主服务的分布（服务筛选下拉 + 一眼看慢在哪）。
type TraceServiceStat struct {
	Service    string
	Count      int64
	ErrorCount int64
	AvgMS      float64
	MaxMS      float64
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
			COALESCE(MAX(CASE WHEN %[4]s THEN s.name END), '') AS entry,
			COALESCE(MAX(CASE WHEN %[4]s THEN s.service END), '') AS root_service,
			COALESCE(MAX(CASE WHEN %[4]s THEN s.protocol END), '') AS protocol,
			COALESCE(MAX(CASE WHEN %[4]s THEN s.status_code END), '') AS status_code,
			MAX(CASE WHEN %[4]s AND %[1]s THEN 1 ELSE 0 END) AS root_error,
			%[2]s AS main_service
		FROM span s
		WHERE s.trace_id IN (
			SELECT DISTINCT r.trace_id FROM span r
			WHERE %[3]s AND r.start_ns >= ? AND r.start_ns <= ?
		)
		GROUP BY s.trace_id`, errorExpr("s"), mainService, requestEntryScope("r"), isEntryExpr("s"))
	return sql, []any{f.Window.FromNS, f.Window.UntilNS}
}

// whereSQL 在聚合结果上套筛选；返回值已按 SQL 文本顺序排好参数。
func (f TraceFilter) whereSQL() (string, []any) {
	conds := []string{"1=1"}
	var args []any
	switch strings.ToUpper(f.Status) {
	case "ERROR":
		conds = append(conds, "root_error = 1")
	case "SUCCESS":
		conds = append(conds, "root_error = 0")
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
		conds = append(conds, `entry LIKE ? ESCAPE '\'`)
		args = append(args, likePattern(f.Keyword))
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
		return "root_error DESC, start_ns DESC, trace_id"
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
			listSQL := "SELECT trace_id, start_ns, end_ns, span_count, error_spans, entry, root_service, protocol, status_code, root_error, main_service FROM (" + agg + ") " + where +
				" ORDER BY " + f.orderSQL() + " LIMIT ?"
			listArgs := append(append(append([]any{}, base...), filterArgs...), want)
			rs, err := file.db.QueryContext(ctx, listSQL, listArgs...)
			if err != nil {
				return TracePage{}, err
			}
			for rs.Next() {
				var r TraceRow
				var rootService string
				if err := rs.Scan(&r.TraceID, &r.StartNS, &r.EndNS, &r.SpanCount, &r.ErrorSpans,
					&r.Entry, &rootService, &r.Protocol, &r.StatusCode, &r.RootError, &r.Service); err != nil {
					_ = rs.Close()
					return TracePage{}, err
				}
				if r.Service == "" {
					r.Service = rootService
				}
				r.Status = "SUCCESS"
				if r.RootError != 0 {
					r.Status = "ERROR"
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
		statSQL := "SELECT main_service, COUNT(*), SUM(root_error), AVG(end_ns - start_ns), MAX(end_ns - start_ns) FROM (" + agg + ") " + where +
			" AND main_service <> '' GROUP BY main_service ORDER BY 2 DESC LIMIT 30"
		statArgs := append(append([]any{}, base...), filterArgs...)
		srs, err := file.db.QueryContext(ctx, statSQL, statArgs...)
		if err != nil {
			return TracePage{}, err
		}
		for srs.Next() {
			var st TraceServiceStat
			var avgNS, maxNS float64
			if err := srs.Scan(&st.Service, &st.Count, &st.ErrorCount, &avgNS, &maxNS); err != nil {
				_ = srs.Close()
				return TracePage{}, err
			}
			st.AvgMS = avgNS / 1e6
			st.MaxMS = maxNS / 1e6
			if kept, ok := stats[st.Service]; ok {
				kept.Count += st.Count
				kept.ErrorCount += st.ErrorCount
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
	dst.RootError += src.RootError
	if dst.RootError != 0 {
		dst.Status = "ERROR"
	}
	if dst.Entry == "" {
		dst.Entry = src.Entry
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

func sortTraceRows(rows []TraceRow, sortBy string) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch sortBy {
		case "duration":
			da, db := a.EndNS-a.StartNS, b.EndNS-b.StartNS
			if da != db {
				return da > db
			}
		case "status":
			if a.RootError != b.RootError {
				return a.RootError > b.RootError
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
type RedPoint struct {
	StartNS    int64
	Count      int64
	ErrorCount int64
	P95MS      float64
	P99MS      float64
}

// RedSeries 是一个入口名字的 RED 汇总 + 趋势。
type RedSeries struct {
	Name       string
	Service    string
	Protocol   string
	Count      int64
	ErrorCount int64
	AvgMS      float64
	MaxMS      float64
	P50MS      float64
	P95MS      float64
	P99MS      float64
	Points     []RedPoint
}

// RedReport 是 RED 页面/概览的一屏数据。
type RedReport struct {
	Series   []RedSeries
	Total    RedSeries
	BucketNS int64
	Dropped  int // 因基数上限被折叠的系列数
}

// redAcc 是一个系列（或全局）的累加器：窗口汇总 + 每桶耗时直方图。
type redAcc struct {
	count, errs, sumNS, maxNS int64
	hist                      [][]int64 // [时间桶][耗时桶]
	errByBucket               []int64
}

func newRedAcc(buckets int) *redAcc {
	a := &redAcc{hist: make([][]int64, buckets), errByBucket: make([]int64, buckets)}
	for i := range a.hist {
		a.hist[i] = make([]int64, len(redBoundsMS)+1)
	}
	return a
}

func (a *redAcc) add(tb, bucket, count, errs, sumNS, maxNS int64) {
	a.count += count
	a.errs += errs
	a.sumNS += sumNS
	if maxNS > a.maxNS {
		a.maxNS = maxNS
	}
	a.hist[tb][bucket] += count
	a.errByBucket[tb] += errs
}

// redBoundsMS 是耗时直方图的桶上界（毫秒，le 语义），末桶为 +Inf。
//
// 与 Prometheus 默认桶（szrw_rpc_duration_seconds）同一套刻度，只是换成毫秒：
// 分位数由这张直方图按 histogram_quantile 同法线性插值，误差不超过一个桶宽。
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
//   - count/avg/max 为精确值；p50/p95/p99 由耗时直方图插值估算（见 redBoundsMS）；
//   - 趋势的桶宽由窗口长度决定，桶内 p95 同样来自直方图。
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
			SUM(s.end_ns - s.start_ns) AS sum_ns,
			MAX(s.end_ns - s.start_ns) AS max_ns
		FROM span s
		WHERE %s AND s.start_ns >= ? AND s.start_ns <= ?
		GROUP BY s.name, s.service, s.protocol, tb, bucket`,
		durationBucketExpr("s"), errorExpr("s"), requestEntryScope("s"))

	for _, file := range s.all() {
		rows, err := file.db.QueryContext(ctx, sql, from, bucketNS, from, until)
		if err != nil {
			return RedReport{}, err
		}
		for rows.Next() {
			var name, service, protocol string
			var tb, bucket, count, errs, sumNS, maxNS int64
			if err := rows.Scan(&name, &service, &protocol, &tb, &bucket, &count, &errs, &sumNS, &maxNS); err != nil {
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
			a.add(tb, bucket, count, errs, sumNS, maxNS)
			total.add(tb, bucket, count, errs, sumNS, maxNS)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return RedReport{}, err
		}
		_ = rows.Close()
	}

	build := func(a *redAcc, name, service, protocol string) RedSeries {
		out := RedSeries{Name: name, Service: service, Protocol: protocol, Count: a.count, ErrorCount: a.errs}
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
		out.P50MS = histogramQuantile(0.50, all, out.MaxMS)
		out.P95MS = histogramQuantile(0.95, all, out.MaxMS)
		out.P99MS = histogramQuantile(0.99, all, out.MaxMS)
		out.Points = make([]RedPoint, buckets)
		for i := range a.hist {
			var count int64
			for _, c := range a.hist[i] {
				count += c
			}
			out.Points[i] = RedPoint{
				StartNS:    from + int64(i)*bucketNS,
				Count:      count,
				ErrorCount: a.errByBucket[i],
				P95MS:      histogramQuantile(0.95, a.hist[i], out.MaxMS),
				P99MS:      histogramQuantile(0.99, a.hist[i], out.MaxMS),
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

// histogramQuantile 按 Prometheus histogram_quantile 的方式在桶内线性插值。
// counts 是各耗时桶的计数（末桶 = 大于最后一个上界），maxMS 是观测到的最大值。
func histogramQuantile(q float64, counts []int64, maxMS float64) float64 {
	var total int64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	rank := q * float64(total)
	var cum int64
	lower := 0.0
	for i, c := range counts {
		if float64(cum)+float64(c) >= rank {
			if i >= len(redBoundsMS) {
				return maxMS
			}
			upper := redBoundsMS[i]
			if c <= 0 {
				return upper
			}
			frac := (rank - float64(cum)) / float64(c)
			if frac < 0 {
				frac = 0
			}
			if frac > 1 {
				frac = 1
			}
			return lower + (upper-lower)*frac
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

package obsdb

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

// pushSpan / pushLog 是测试里造数据的快捷方式：写入队列由后台批量落盘。
func pushSpan(s *Store, sp Span) {
	if sp.Attrs == nil {
		sp.Attrs = map[string]any{}
	}
	s.push(entry{span: &sp})
}

func pushLog(s *Store, l LogRecord) {
	if l.Attrs == nil {
		l.Attrs = map[string]any{}
	}
	s.push(entry{log: &l})
}

func TestRequestTracesAggregatesAndFilters(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Now().Add(-time.Minute).UnixNano()
	window := Window{FromNS: base - int64(time.Second), UntilNS: base + int64(time.Minute)}

	// 一次成功请求：HTTP 根 span + 一个 gRPC 子 span。
	pushSpan(s, Span{TraceID: "t-ok", SpanID: "s-root", Name: "POST /grpc-gateway/TradeService/CreateOrder",
		Service: "gateway", Protocol: "HTTP", Status: "SUCCESS", StatusCode: "200",
		StartNS: base, EndNS: base + int64(10*time.Millisecond)})
	pushSpan(s, Span{TraceID: "t-ok", SpanID: "s-grpc", ParentSpanID: "s-root",
		Name: "/proto.trade.TradeService/CreateOrder", Service: "proto.trade.TradeService",
		Protocol: "GRPC", Status: "SUCCESS", StatusCode: "0",
		StartNS: base + int64(time.Millisecond), EndNS: base + int64(9*time.Millisecond)})
	pushLog(s, LogRecord{TimeNS: base + int64(2*time.Millisecond), Level: "info",
		Msg: "created", Caller: "D:/workspace/service/microservice/trade/create.go:42", TraceID: "t-ok", SpanID: "s-grpc"})

	// 一次失败请求：gRPC 状态码非 0，HTTP 也返回 500。
	pushSpan(s, Span{TraceID: "t-bad", SpanID: "s-root2", Name: "POST /grpc-gateway/TradeService/CreateOrder",
		Service: "gateway", Protocol: "HTTP", Status: "SUCCESS", StatusCode: "500",
		StartNS: base + int64(20*time.Millisecond), EndNS: base + int64(120*time.Millisecond)})
	pushSpan(s, Span{TraceID: "t-bad", SpanID: "s-grpc2", ParentSpanID: "s-root2",
		Name: "/proto.trade.TradeService/CreateOrder", Service: "proto.trade.TradeService",
		Protocol: "GRPC", Status: "ERROR", StatusCode: "13",
		StartNS: base + int64(21*time.Millisecond), EndNS: base + int64(118*time.Millisecond)})

	// 静态资源请求必须被排除在请求/RED 口径之外。
	pushSpan(s, Span{TraceID: "t-asset", SpanID: "s-asset", Name: "GET /assets/index-abc.js",
		Service: "gateway", Protocol: "HTTP", Status: "SUCCESS", StatusCode: "200",
		StartNS: base + int64(30*time.Millisecond), EndNS: base + int64(31*time.Millisecond)})

	time.Sleep(400 * time.Millisecond) // 等 200ms 批量落盘

	page, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Rows) != 2 {
		t.Fatalf("want 2 requests (asset excluded), got total=%d rows=%d", page.Total, len(page.Rows))
	}
	// 默认按时间倒序：t-bad 在后。
	newest, oldest := page.Rows[0], page.Rows[1]
	if newest.TraceID != "t-bad" {
		t.Fatalf("want newest trace first, got %+v", page.Rows)
	}
	if newest.Status != "ERROR" {
		t.Fatalf("HTTP 500 应算 ERROR，got %q", newest.Status)
	}
	if newest.Service != "proto.trade.TradeService" {
		t.Fatalf("主服务应取最早 GRPC span 的 service，got %q", newest.Service)
	}
	if newest.SpanCount != 2 || newest.ErrorSpans != 2 { // 根 500 + 子 span 13 都算失败
		t.Fatalf("span 聚合不对: %+v", newest)
	}
	if oldest.Status != "SUCCESS" || oldest.LogCount != 1 {
		t.Fatalf("成功请求聚合不对: %+v", oldest)
	}

	byDuration, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, SortBy: "duration", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(byDuration.Rows) != 1 || byDuration.Rows[0].TraceID != "t-bad" {
		t.Fatalf("按时长排序应先出慢请求: %+v", byDuration.Rows)
	}
	if byDuration.Total != 2 {
		t.Fatalf("total 不受分页影响，got %d", byDuration.Total)
	}

	onlyBad, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, Status: "ERROR", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if onlyBad.Total != 1 || len(onlyBad.Rows) != 1 || onlyBad.Rows[0].TraceID != "t-bad" {
		t.Fatalf("错误筛选不对: %+v", onlyBad.Rows)
	}

	byService, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, Service: "proto.trade.TradeService", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if byService.Total != 2 {
		t.Fatalf("服务筛选不对: total=%d", byService.Total)
	}
	if len(byService.Services) != 1 || byService.Services[0].Service != "proto.trade.TradeService" || byService.Services[0].ErrorCount != 1 {
		t.Fatalf("服务分布不对: %+v", byService.Services)
	}
}

func TestREDHistogramAndTrend(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Now().Add(-time.Minute).UnixNano()
	// 10 条 10ms 的成功 + 1 条 2s 的失败。
	for i := 0; i < 10; i++ {
		start := base + int64(i)*int64(100*time.Millisecond)
		pushSpan(s, Span{TraceID: "ok-" + string(rune('a'+i)), SpanID: "ok-" + string(rune('a'+i)),
			Name: "POST /grpc-gateway/ImService/GetMessageList", Service: "gateway", Protocol: "HTTP",
			Status: "SUCCESS", StatusCode: "200",
			StartNS: start, EndNS: start + int64(10*time.Millisecond)})
	}
	pushSpan(s, Span{TraceID: "slow", SpanID: "slow", Name: "POST /grpc-gateway/ImService/GetMessageList",
		Service: "gateway", Protocol: "HTTP", Status: "SUCCESS", StatusCode: "500",
		StartNS: base, EndNS: base + int64(2*time.Second)})

	time.Sleep(400 * time.Millisecond)

	window := Window{FromNS: base - int64(time.Second), UntilNS: base + int64(time.Minute)}
	report, err := s.RED(context.Background(), RedQuery{Window: window, Buckets: 4})
	if err != nil {
		t.Fatal(err)
	}
	if report.Total.Count != 11 || report.Total.ErrorCount != 1 {
		t.Fatalf("RED 汇总不对: count=%d err=%d", report.Total.Count, report.Total.ErrorCount)
	}
	if report.Total.P50MS < 5 || report.Total.P50MS > 10.001 {
		t.Fatalf("p50 应落在 (5ms,10ms] 桶里，got %.3f", report.Total.P50MS)
	}
	if report.Total.P99MS < 1000 || report.Total.P99MS > 2500 {
		t.Fatalf("p99 应落在慢请求桶 (1000ms,2500ms] 里，got %.3f", report.Total.P99MS)
	}
	if report.Total.MaxMS < 1999 {
		t.Fatalf("max 应是精确值 2000ms，got %.3f", report.Total.MaxMS)
	}
	if len(report.Total.Points) != 4 {
		t.Fatalf("趋势应有 4 个桶，got %d", len(report.Total.Points))
	}
	if len(report.Series) != 1 || report.Series[0].Name != "POST /grpc-gateway/ImService/GetMessageList" {
		t.Fatalf("按入口名字分组不对: %+v", report.Series)
	}
	if report.Series[0].Service != "gateway" {
		t.Fatalf("HTTP 入口的 service 应保留 span 值，got %q", report.Series[0].Service)
	}
}

func TestRequestEntryAcceptsUpstreamTraceparent(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Now().Add(-time.Minute).UnixNano()
	// 上游透传了 traceparent：入口 HTTP span 的 parent_span_id 指向外部（本库里不存在），
	// 它仍然是这次请求的入口，不能因为「父 span 非空」被排除在 QPS/请求列表之外。
	pushSpan(s, Span{TraceID: "t-remote", SpanID: "aaaaaaaaaaaaaaaa", ParentSpanID: "bbbbbbbbbbbbbbbb",
		Name: "POST /grpc-gateway/UserService/Login", Service: "gateway", Protocol: "HTTP",
		Status: "SUCCESS", StatusCode: "200",
		StartNS: base, EndNS: base + int64(5*time.Millisecond)})
	pushSpan(s, Span{TraceID: "t-remote", SpanID: "cccccccccccccccc", ParentSpanID: "aaaaaaaaaaaaaaaa",
		Name: "/proto.user.UserService/Login", Service: "proto.user.UserService", Protocol: "GRPC",
		Status: "SUCCESS", StatusCode: "0",
		StartNS: base + int64(time.Millisecond), EndNS: base + int64(4*time.Millisecond)})

	time.Sleep(400 * time.Millisecond)

	window := Window{FromNS: base - int64(time.Second), UntilNS: base + int64(time.Minute)}
	page, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Rows) != 1 {
		t.Fatalf("带上游 traceparent 的请求应算一次请求，got total=%d rows=%d", page.Total, len(page.Rows))
	}
	row := page.Rows[0]
	if row.Entry != "POST /grpc-gateway/UserService/Login" || row.Service != "proto.user.UserService" {
		t.Fatalf("入口/主服务识别不对: %+v", row)
	}

	report, err := s.RED(context.Background(), RedQuery{Window: window, Buckets: 2})
	if err != nil {
		t.Fatal(err)
	}
	if report.Total.Count != 1 {
		t.Fatalf("RED 也应把它算进 QPS，got %d", report.Total.Count)
	}
}

// repMS 造 n 条相同的时长（毫秒）。
func repMS(ms float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = ms
	}
	return out
}

// assertQuantilesOrdered 钉住 RED 的分位不变式：0 <= p50 <= p95 <= p99 <= max。
// 趋势点没有独立的 max，只能拿系列 max 当上界，同时要求 p95 <= p99。
func assertQuantilesOrdered(t *testing.T, label string, s RedSeries) {
	t.Helper()
	if s.P50MS < 0 || s.P95MS < 0 || s.P99MS < 0 {
		t.Fatalf("%s: 分位为负: p50=%v p95=%v p99=%v", label, s.P50MS, s.P95MS, s.P99MS)
	}
	if s.P50MS > s.P95MS || s.P95MS > s.P99MS || s.P99MS > s.MaxMS {
		t.Fatalf("%s: 必须 0<=p50<=p95<=p99<=max，got p50=%v p95=%v p99=%v max=%v",
			label, s.P50MS, s.P95MS, s.P99MS, s.MaxMS)
	}
	for i, p := range s.Points {
		if p.P95MS < 0 || p.P99MS < 0 || p.P95MS > p.P99MS || p.P99MS > s.MaxMS {
			t.Fatalf("%s: 趋势第 %d 桶越界/逆序: p95=%v p99=%v seriesMax=%v",
				label, i, p.P95MS, p.P99MS, s.MaxMS)
		}
	}
}

// TestREDQuantilesBoundedByMax 是「P95 大于 Max」的回归：
//
// 生产症状：count=3、max=160.08、p95=242.5、p99=248.5——3 条样本都落在 (100,250]，
// 旧实现按桶边界 [100,250] 线性插值，把桶上界当成了数据支撑。
// 现在插值区间被实测 [min,max] 收紧，并且对 max 封顶。
func TestREDQuantilesBoundedByMax(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Now().Add(-2 * time.Minute).UnixNano()
	cases := []struct {
		name string
		ms   []float64
	}{
		{"count1", []float64{160.08}},                                                 // 单样本：三条分位线应精确等于 max
		{"count2", []float64{150, 160.08}},                                            // 同桶两条
		{"count3", []float64{158, 159.5, 160.08}},                                     // 生产症状
		{"cross", append(append(repMS(10, 90), repMS(160, 5)...), repMS(2000, 5)...)}, // 跨多桶 + 末桶外
		{"over10s", []float64{11000, 12000}},                                          // 落在 +Inf 末桶
	}
	for ci, c := range cases {
		// 每个 case 间隔 6s 起跑：让它们落进不同的趋势时间桶，覆盖按时间桶 merge 极值。
		seriesBase := base + int64(ci)*int64(6*time.Second)
		for i, ms := range c.ms {
			start := seriesBase + int64(i)*int64(time.Millisecond)
			dur := time.Duration(ms * float64(time.Millisecond))
			id := fmt.Sprintf("%s-%d", c.name, i)
			pushSpan(s, Span{TraceID: id, SpanID: id,
				Name:    "POST /grpc-gateway/Regression/" + c.name,
				Service: "gateway", Protocol: "HTTP", Status: "SUCCESS", StatusCode: "200",
				StartNS: start, EndNS: start + int64(dur)})
		}
	}
	time.Sleep(400 * time.Millisecond) // 等 200ms 批量落盘

	window := Window{FromNS: base - int64(time.Second), UntilNS: base + int64(40*time.Second)}
	report, err := s.RED(context.Background(), RedQuery{Window: window, Buckets: 4})
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]RedSeries, len(report.Series))
	for _, sr := range report.Series {
		byName[sr.Name] = sr
	}
	if len(byName) != len(cases) {
		t.Fatalf("入口分组不对: got %d series", len(byName))
	}
	for _, c := range cases {
		sr, ok := byName["POST /grpc-gateway/Regression/"+c.name]
		if !ok {
			t.Fatalf("缺少 series %q", c.name)
		}
		if sr.Count != int64(len(c.ms)) {
			t.Fatalf("%s: count 不对 got %d want %d", c.name, sr.Count, len(c.ms))
		}
		wantMax := 0.0
		for _, ms := range c.ms {
			wantMax = math.Max(wantMax, ms)
		}
		if math.Abs(sr.MaxMS-wantMax) > 0.01 {
			t.Fatalf("%s: max 应是精确值 %v，got %v", c.name, wantMax, sr.MaxMS)
		}
		assertQuantilesOrdered(t, c.name, sr)
	}

	// count=1：唯一一条样本就是 max，分位不应再被桶宽放大。
	if one := byName["POST /grpc-gateway/Regression/count1"]; math.Abs(one.P50MS-one.MaxMS) > 1e-9 ||
		math.Abs(one.P95MS-one.MaxMS) > 1e-9 || math.Abs(one.P99MS-one.MaxMS) > 1e-9 {
		t.Fatalf("count1 的三条分位线应等于 max=%v，got p50=%v p95=%v p99=%v",
			one.MaxMS, one.P50MS, one.P95MS, one.P99MS)
	}
	// 生产症状的直球断言：修复前这里是 242.5 / 248.5。
	if three := byName["POST /grpc-gateway/Regression/count3"]; three.P95MS >= 242 || three.P99MS >= 248 {
		t.Fatalf("count3 的分位应落在实测 [158,160.08] 内，got p95=%v p99=%v max=%v",
			three.P95MS, three.P99MS, three.MaxMS)
	}
	// ALL 汇总也要满足同一不变式。
	assertQuantilesOrdered(t, "ALL", report.Total)
}

// TestHistogramQuantileSupport 直接钉住插值区间的口径（不经 SQL，跑得快）：
// 样本全在同一个桶里时，区间退化成实测 [min,max]；末桶（+Inf）用 max 当上界。
func TestHistogramQuantileSupport(t *testing.T) {
	const le250 = 7 // redBoundsMS[7] == 250，(100,250] 这一桶
	all := make([]int64, len(redBoundsMS)+1)
	all[le250] = 3

	p50, p95, p99 := redQuantiles(all, 158, 160.08)
	if p50 < 158 || p99 > 160.08 || p50 > p95 || p95 > p99 {
		t.Fatalf("单桶样本应收敛到 [158,160.08] 且单调，got %v %v %v", p50, p95, p99)
	}

	single := make([]int64, len(redBoundsMS)+1)
	single[le250] = 1
	if q := histogramQuantile(0.95, single, 160.08, 160.08); math.Abs(q-160.08) > 1e-9 {
		t.Fatalf("count=1 时应精确返回 max，got %v", q)
	}

	last := make([]int64, len(redBoundsMS)+1)
	last[len(redBoundsMS)] = 2 // 都在 +Inf 末桶
	if p50, p95, p99 := redQuantiles(last, 11000, 12000); p50 < 11000 || p99 > 12000 || p99 < p95 {
		t.Fatalf("末桶应以实测 max 为上界，got %v %v %v", p50, p95, p99)
	}

	empty := make([]int64, len(redBoundsMS)+1)
	if p50, p95, p99 := redQuantiles(empty, 0, 0); p50 != 0 || p95 != 0 || p99 != 0 {
		t.Fatalf("空直方图应为 0，got %v %v %v", p50, p95, p99)
	}
}

// TestClientErrorSplitFromServerError 是「404 被算成服务端故障」的回归：
//
// 生产症状：扫描器打 /api/graphql、/api/uploads/../../.env 等不存在的路径，返回 404，
// 看板显示错误率 100%。4xx 是客户端错误（探测/鉴权失败/参数不合法），不是服务端故障；
// 5xx / gRPC 非 0 才是。这里一条 404 + 一条 500 混在一起：
// ErrorCount 只能含 500，ClientErrorCount 只能含 404，且 404 不出现在错误列表里。
func TestClientErrorSplitFromServerError(t *testing.T) {
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Now().Add(-time.Minute).UnixNano()
	// 未匹配路由：网关把 span 名折叠成 <unmatched>，真实路径留在属性里（库里仍存原文）。
	pushSpan(s, Span{TraceID: "t-404", SpanID: "s-404", Name: UnmatchedEntry,
		Service: "gateway", Protocol: "HTTP", Status: "SUCCESS", StatusCode: "404",
		StartNS: base, EndNS: base + int64(2*time.Millisecond),
		Attrs: map[string]any{UnmatchedPathAttr: "/api/uploads/../../.env"}})
	// 真实故障：网关 500，span 自己也标了 ERROR（被上面 ERROR 分支兜住）。
	pushSpan(s, Span{TraceID: "t-500", SpanID: "s-500", Name: "POST /grpc-gateway/TradeService/CreateOrder",
		Service: "gateway", Protocol: "HTTP", Status: "ERROR", StatusCode: "500",
		StartNS: base + int64(10*time.Millisecond), EndNS: base + int64(60*time.Millisecond)})

	time.Sleep(400 * time.Millisecond)

	window := Window{FromNS: base - int64(time.Second), UntilNS: base + int64(time.Minute)}
	report, err := s.RED(context.Background(), RedQuery{Window: window, Buckets: 2})
	if err != nil {
		t.Fatal(err)
	}
	if report.Total.Count != 2 || report.Total.ErrorCount != 1 || report.Total.ClientErrorCount != 1 {
		t.Fatalf("RED 汇总应为 count=2 err=1 clientErr=1，got count=%d err=%d clientErr=%d",
			report.Total.Count, report.Total.ErrorCount, report.Total.ClientErrorCount)
	}
	var sumErr, sumClientErr int64
	for _, p := range report.Total.Points {
		sumErr += p.ErrorCount
		sumClientErr += p.ClientErrorCount
	}
	if sumErr != 1 || sumClientErr != 1 {
		t.Fatalf("趋势桶的错误计数应与汇总一致，got err=%d clientErr=%d", sumErr, sumClientErr)
	}
	byName := make(map[string]RedSeries, len(report.Series))
	for _, sr := range report.Series {
		byName[sr.Name] = sr
	}
	if got := byName[UnmatchedEntry]; got.Count != 1 || got.ErrorCount != 0 || got.ClientErrorCount != 1 {
		t.Fatalf("<unmatched> 系列应只算客户端错误: %+v", got)
	}

	page, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("404 仍是一次请求（进 total），got %d", page.Total)
	}
	var row404, row500 *TraceRow
	for i := range page.Rows {
		switch page.Rows[i].TraceID {
		case "t-404":
			row404 = &page.Rows[i]
		case "t-500":
			row500 = &page.Rows[i]
		}
	}
	if row404 == nil || row500 == nil {
		t.Fatalf("两条请求都应在列表里: %+v", page.Rows)
	}
	if row404.Status != "CLIENT_ERROR" || row404.ErrorSpans != 0 {
		t.Fatalf("404 必须是 CLIENT_ERROR 且不计入 ErrorSpans: %+v", *row404)
	}
	if row404.Entry != UnmatchedEntry || row404.Path != "/api/uploads/../../.env" {
		t.Fatalf("折叠行应保留入口名与真实路径: %+v", *row404)
	}
	if row500.Status != "ERROR" || row500.ErrorSpans != 1 {
		t.Fatalf("500 必须仍是 ERROR: %+v", *row500)
	}

	// 错误筛选只出 500，404 不在里面。
	onlyErr, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, Status: "ERROR", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if onlyErr.Total != 1 || len(onlyErr.Rows) != 1 || onlyErr.Rows[0].TraceID != "t-500" {
		t.Fatalf("错误列表只应有 500: total=%d rows=%+v", onlyErr.Total, onlyErr.Rows)
	}
	onlyClient, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, Status: "CLIENT_ERROR", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if onlyClient.Total != 1 || len(onlyClient.Rows) != 1 || onlyClient.Rows[0].TraceID != "t-404" {
		t.Fatalf("客户端错误筛选只应有 404: total=%d rows=%+v", onlyClient.Total, onlyClient.Rows)
	}
	onlyOK, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, Status: "SUCCESS", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if onlyOK.Total != 0 {
		t.Fatalf("SUCCESS 不应包含 404/500，got %d", onlyOK.Total)
	}

	// 排除探测：<unmatched> 整行消失，真实接口还在。
	noProbe, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, ExcludeProbes: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if noProbe.Total != 1 || len(noProbe.Rows) != 1 || noProbe.Rows[0].TraceID != "t-500" {
		t.Fatalf("排除探测后只应有真实接口: total=%d rows=%+v", noProbe.Total, noProbe.Rows)
	}
	// 关键词能匹配到折叠掉的真实路径（证据没丢）。
	byPath, err := s.RequestTraces(context.Background(), TraceFilter{Window: window, Keyword: ".env", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if byPath.Total != 1 || len(byPath.Rows) != 1 || byPath.Rows[0].TraceID != "t-404" {
		t.Fatalf("应能按真实路径搜到折叠行: total=%d rows=%+v", byPath.Total, byPath.Rows)
	}
}

// TestFoldUnmatchedEntryOnlyBusinessPrefixes 钉住折叠范围：只有会进 RED/请求口径的业务前缀
// （/api/、/grpc-gateway/、/service/api/）才折叠；SPA 页面路由与静态资源不折叠，
// 否则页面加载会被塞进 <unmatched>，把 QPS 带偏。
func TestFoldUnmatchedEntryOnlyBusinessPrefixes(t *testing.T) {
	cases := []struct {
		path string
		want string
		fold bool
	}{
		{"/api/graphql", UnmatchedEntry, true},
		{"/api/uploads/../../.env", UnmatchedEntry, true},
		{"/grpc-gateway/DiagnosticsService/Nope", UnmatchedEntry, true},
		{"/service/api/payment/callback/nope", UnmatchedEntry, true},
		{"/admin/login", "", false},
		{"/assets/missing.js", "", false},
		{"/", "", false},
	}
	for _, c := range cases {
		got, fold := FoldUnmatchedEntry(c.path)
		if got != c.want || fold != c.fold {
			t.Fatalf("FoldUnmatchedEntry(%q) = (%q,%v)，期望 (%q,%v)", c.path, got, fold, c.want, c.fold)
		}
	}
}

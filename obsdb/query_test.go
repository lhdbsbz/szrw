package obsdb

import (
	"context"
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

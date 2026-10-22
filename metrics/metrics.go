// Package metrics 把每次 RPC 变成 Prometheus 指标（RED：请求数、错误数、延迟）。
//
// 指标名和标签是框架常量，不要改：改一次所有面板和告警就失效一次。
// 项目要加业务指标（下单数、缓存命中…）就自己注册到 prometheus.DefaultRegisterer，
// 会跟这里的指标一起从同一个 /metrics 端点出去。
package metrics

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	io_prometheus_client "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc/status"

	"github.com/lhdbsbz/szrw/core"
)

var (
	requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "szrw_rpc_requests_total",
		Help: "RPC 调用次数（method=全限定方法名, kind=client|server, code=gRPC 状态码）",
	}, []string{"method", "kind", "code"})

	duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "szrw_rpc_duration_seconds",
		Help:    "RPC 耗时（method=全限定方法名, kind=client|server）",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "kind"})

	once sync.Once
)

// Install 挂上采集器。重复调用只生效一次。
//
// 它只注册一个 Observer，不碰日志、不碰传播：与 observe.Install() 并列使用。
func Install() {
	once.Do(func() {
		prometheus.MustRegister(requests, duration)
		core.AddObserver(recorder{})
	})
}

// Handler 是 /metrics 端点。同一个进程里只有一个，挂在网关之外（不让它走鉴权）。
func Handler() http.Handler { return promhttp.Handler() }

type recorder struct{}

func (recorder) Unary(_ context.Context, method string, _ any, dur time.Duration, err error, server bool) {
	observe(method, dur, err, server)
}

func (recorder) Stream(_ context.Context, method string, dur time.Duration, err error, server bool) {
	observe(method, dur, err, server)
}

func observe(method string, dur time.Duration, err error, server bool) {
	kind := "client"
	if server {
		kind = "server"
	}
	requests.WithLabelValues(method, kind, status.Code(err).String()).Inc()
	duration.WithLabelValues(method, kind).Observe(dur.Seconds())
}

// Sample 是一次指标采样（已格式化标签，便于直接展示）。
type Sample struct {
	Name   string
	Labels string
	Value  float64
}

// Snapshot 现取一份指标快照（admin 的运维页用它，与 /metrics 同源）。
//
// 只收 Gauge/Counter/Untyped 与 Histogram 的 count/sum：直方图的桶会撑爆页面，
// 需要分布请走 /metrics 或接 Prometheus。
func Snapshot() ([]Sample, error) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return nil, err
	}
	var out []Sample
	for _, mf := range families {
		name := mf.GetName()
		for _, m := range mf.GetMetric() {
			labels := formatLabels(m)
			switch {
			case m.GetCounter() != nil:
				out = append(out, Sample{Name: name, Labels: labels, Value: m.GetCounter().GetValue()})
			case m.GetGauge() != nil:
				out = append(out, Sample{Name: name, Labels: labels, Value: m.GetGauge().GetValue()})
			case m.GetUntyped() != nil:
				out = append(out, Sample{Name: name, Labels: labels, Value: m.GetUntyped().GetValue()})
			case m.GetHistogram() != nil:
				h := m.GetHistogram()
				out = append(out, Sample{Name: name + "_count", Labels: labels, Value: float64(h.GetSampleCount())})
				out = append(out, Sample{Name: name + "_sum", Labels: labels, Value: h.GetSampleSum()})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Labels < out[j].Labels
	})
	return out, nil
}

func formatLabels(m interface {
	GetLabel() []*io_prometheus_client.LabelPair
}) string {
	pairs := m.GetLabel()
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.GetName()+"="+p.GetValue())
	}
	return strings.Join(parts, ",")
}

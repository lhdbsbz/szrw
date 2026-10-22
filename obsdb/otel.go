package obsdb

import (
	"strconv"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// fromOTelSpan 把 SDK 的 span 转成库里的中立结构。
func fromOTelSpan(sp sdktrace.ReadOnlySpan) Span {
	attrs := make(map[string]any, len(sp.Attributes()))
	for _, a := range sp.Attributes() {
		attrs[string(a.Key)] = a.Value.AsInterface()
	}

	out := Span{
		TraceID:  sp.SpanContext().TraceID().String(),
		SpanID:   sp.SpanContext().SpanID().String(),
		Name:     sp.Name(),
		Status:   "SUCCESS",
		StartNS:  sp.StartTime().UnixNano(),
		EndNS:    sp.EndTime().UnixNano(),
		Attrs:    attrs,
		ErrorMsg: sp.Status().Description,
	}
	if sp.Parent().IsValid() {
		out.ParentSpanID = sp.Parent().SpanID().String()
	}
	switch {
	case attrs["rpc.system"] != nil:
		out.Protocol = "GRPC"
		out.Service, _ = attrs["rpc.service"].(string)
		out.StatusCode = anyString(attrs["rpc.grpc.status_code"])
	case attrs["http.request.method"] != nil:
		out.Protocol = "HTTP"
		out.Service = "gateway"
		out.StatusCode = anyString(attrs["http.response.status_code"])
	}
	if sp.Status().Code == codes.Error {
		out.Status = "ERROR"
	}
	return out
}

func anyString(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case string:
		return n
	case int64:
		return strconv.FormatInt(n, 10)
	case float64:
		return strconv.FormatInt(int64(n), 10)
	}
	return ""
}

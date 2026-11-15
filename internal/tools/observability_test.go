package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBuildTimeSeriesQuery_BoundsAndDefaults(t *testing.T) {
	query, err := buildTimeSeriesQuery(TimeSeriesMetricsArgs{Type: "SYSTEM"})
	if err != nil {
		t.Fatalf("default query: %v", err)
	}
	if got := query.Get("duration_minutes"); got != "30" {
		t.Errorf("default duration = %q, want 30", got)
	}
	if _, err := buildTimeSeriesQuery(TimeSeriesMetricsArgs{Type: "unknown"}); err == nil {
		t.Error("unknown metric type should be rejected")
	}
	if _, err := buildTimeSeriesQuery(TimeSeriesMetricsArgs{Type: "api", DurationMinutes: 1441}); err == nil {
		t.Error("duration over 1440 minutes should be rejected")
	}
}

func TestBuildApplicationLogsQuery_BoundsAndDefaults(t *testing.T) {
	query, err := buildApplicationLogsQuery(ApplicationLogsArgs{Level: "WARN"})
	if err != nil {
		t.Fatalf("default query: %v", err)
	}
	if query.Get("level") != "warn" || query.Get("limit") != "100" || query.Get("since_minutes") != "60" {
		t.Errorf("unexpected default query: %v", query)
	}
	if _, err := buildApplicationLogsQuery(ApplicationLogsArgs{Level: "trace"}); err == nil {
		t.Error("unsupported log level should be rejected")
	}
	if _, err := buildApplicationLogsQuery(ApplicationLogsArgs{Limit: 1001}); err == nil {
		t.Error("limit over 1000 should be rejected")
	}
	if _, err := buildApplicationLogsQuery(ApplicationLogsArgs{SinceMinutes: 1441}); err == nil {
		t.Error("look-back over 1440 minutes should be rejected")
	}
}

func TestCompactTimeSeriesResponseKeepsNewestPointsAndValidJSON(t *testing.T) {
	points := make([]map[string]any, 360)
	for i := range points {
		points[i] = map[string]any{"timestamp": i, "value": i}
	}
	body, err := json.Marshal(map[string]any{"type": "api", "data": points})
	if err != nil {
		t.Fatal(err)
	}
	got := compactTimeSeriesResponse(body, 220)
	if !json.Valid([]byte(got)) {
		t.Fatalf("truncated response is invalid JSON: %s", got)
	}
	var decoded struct {
		Data []struct {
			Timestamp int `json:"timestamp"`
		} `json:"data"`
		Truncated int `json:"truncated_points"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Truncated == 0 {
		t.Fatal("expected truncated_points to be reported")
	}
	if len(decoded.Data) == 0 || decoded.Data[len(decoded.Data)-1].Timestamp != 359 {
		t.Fatalf("newest point missing: %#v", decoded.Data)
	}
	if decoded.Data[0].Timestamp <= 0 {
		t.Fatalf("oldest points were not discarded: %#v", decoded.Data)
	}
	if decoded.Truncated+len(decoded.Data) != len(points) {
		t.Fatalf("truncated_points = %d with %d points kept, want them to sum to %d", decoded.Truncated, len(decoded.Data), len(points))
	}
	if decoded.Data[0].Timestamp != decoded.Truncated {
		t.Fatalf("first kept point = %d, want %d (a contiguous newest suffix)", decoded.Data[0].Timestamp, decoded.Truncated)
	}
}

// TestCompactTimeSeriesResponseFillsTheCap pins both edges of the budget
// across many caps: the output never exceeds the cap, and keeping one more
// point would have.
func TestCompactTimeSeriesResponseFillsTheCap(t *testing.T) {
	points := make([]map[string]any, 360)
	for i := range points {
		points[i] = map[string]any{"timestamp": "2026-11-15T17:00:00.123456789Z", "seq": i, "latency_p99_ms": 1.25}
	}
	body, err := json.Marshal(map[string]any{"type": "api", "points_count": len(points), "data": points})
	if err != nil {
		t.Fatal(err)
	}
	for maxChars := 200; maxChars < 30000; maxChars += 97 {
		got := compactTimeSeriesResponse(body, maxChars)
		var decoded struct {
			Data      []json.RawMessage `json:"data"`
			Truncated int               `json:"truncated_points"`
		}
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("cap %d: invalid JSON: %v", maxChars, err)
		}
		if len(got) > maxChars {
			t.Fatalf("cap %d: response is %d bytes", maxChars, len(got))
		}
		if decoded.Truncated+len(decoded.Data) != len(points) {
			t.Fatalf("cap %d: truncated_points %d + kept %d != %d", maxChars, decoded.Truncated, len(decoded.Data), len(points))
		}
		if decoded.Truncated == 0 {
			continue
		}
		oneMore, err := json.Marshal(map[string]any{
			"type":             "api",
			"points_count":     len(points),
			"data":             points[decoded.Truncated-1:],
			"truncated_points": decoded.Truncated - 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(oneMore) <= maxChars {
			t.Fatalf("cap %d: kept %d points but %d fit (%d bytes)", maxChars, len(decoded.Data), len(decoded.Data)+1, len(oneMore))
		}
	}
}

func TestCompactTimeSeriesResponseUnderCapIsUntouched(t *testing.T) {
	body := []byte(`{"type": "api", "data": [{"timestamp": 1}, {"timestamp": 2}]}`)
	got := compactTimeSeriesResponse(body, 50000)
	if strings.Contains(got, "truncated_points") {
		t.Fatalf("truncated_points reported for a response under the cap: %s", got)
	}
	if strings.Contains(got, " ") {
		t.Fatalf("response not compacted: %s", got)
	}
	var decoded struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil || len(decoded.Data) != 2 {
		t.Fatalf("decoded = %#v, err = %v, want both points", decoded, err)
	}
}

func TestInvalidArgumentErrorIsNotReportedAsArcError(t *testing.T) {
	_, err := buildTimeSeriesQuery(TimeSeriesMetricsArgs{Type: "unknown"})
	if _, ok := err.(*invalidArgumentError); !ok {
		t.Fatalf("error type = %T, want invalidArgumentError", err)
	}
	result := observabilityError(err)
	if !result.IsError || len(result.Content) != 1 {
		t.Fatalf("result = %#v, want one error content", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.TextContent", result.Content[0])
	}
	if strings.Contains(text.Text, "Arc error") {
		t.Fatalf("validation error labelled as an Arc error: %q", text.Text)
	}
	want := "Invalid argument: metric type must be one of: system, application, api"
	if text.Text != want {
		t.Fatalf("message = %q, want %q", text.Text, want)
	}
}

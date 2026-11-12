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
}

func TestInvalidArgumentErrorIsNotReportedAsArcError(t *testing.T) {
	_, err := buildTimeSeriesQuery(TimeSeriesMetricsArgs{Type: "unknown"})
	if _, ok := err.(*invalidArgumentError); !ok {
		t.Fatalf("error type = %T, want invalidArgumentError", err)
	}
	if got := fmt.Sprintf("Invalid argument: %v", err); got == "" {
		t.Fatal("empty validation message")
	}
}

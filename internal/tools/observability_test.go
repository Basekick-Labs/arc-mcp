package tools

import "testing"

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

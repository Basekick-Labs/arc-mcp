package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMetricsRequestValidatesAndBoundsFamilies(t *testing.T) {
	tests := []struct {
		name, family, metricType, endpoint, query string
		minutes                                   int
		wantErr                                   bool
	}{
		{name: "aggregate", family: "aggregate", endpoint: "/api/v1/metrics"},
		{name: "memory", family: "memory", endpoint: "/api/v1/metrics/memory"},
		{name: "query pool", family: "query-pool", endpoint: "/api/v1/metrics/query-pool"},
		{name: "endpoints", family: "endpoints", endpoint: "/api/v1/metrics/endpoints"},
		{name: "timeseries default", family: "timeseries", metricType: "api", endpoint: "/api/v1/metrics/timeseries/api", query: "duration_minutes=30"},
		{name: "timeseries bound", family: "timeseries", metricType: "system", minutes: 1440, endpoint: "/api/v1/metrics/timeseries/system", query: "duration_minutes=1440"},
		{name: "unknown family", family: "logs", wantErr: true},
		{name: "unknown timeseries", family: "timeseries", metricType: "other", wantErr: true},
		{name: "too long", family: "timeseries", metricType: "api", minutes: 1441, wantErr: true},
		{name: "type on snapshot", family: "aggregate", metricType: "api", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint, query, timeseries, err := metricsRequest(MetricsArgs{Family: tt.family, Type: tt.metricType, DurationMinutes: tt.minutes})
			if (err != nil) != tt.wantErr {
				t.Fatalf("metricsRequest error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if endpoint != tt.endpoint || query.Encode() != tt.query || timeseries != (tt.family == "timeseries") {
				t.Errorf("metricsRequest = (%q, %q, %v), want (%q, %q, %v)", endpoint, query.Encode(), timeseries, tt.endpoint, tt.query, tt.family == "timeseries")
			}
		})
	}
}

func TestTrimTimeSeriesKeepsNewestAndExactNumbers(t *testing.T) {
	body := `{"type":"api","counter":9007199254740993,"data":[{"timestamp":1,"value":10},{"timestamp":2,"value":20},{"timestamp":3,"value":30},{"timestamp":4,"value":40}]}`
	got, err := trimTimeSeries([]byte(body), 120)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(got) {
		t.Fatalf("trimmed response is invalid JSON: %s", got)
	}
	if !strings.Contains(string(got), `"counter":9007199254740993`) {
		t.Fatalf("integer lost precision: %s", got)
	}
	var decoded struct {
		Data []struct {
			Timestamp int `json:"timestamp"`
		} `json:"data"`
		TruncatedPoints int `json:"truncated_points"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TruncatedPoints == 0 || len(decoded.Data) == 0 || decoded.Data[len(decoded.Data)-1].Timestamp != 4 {
		t.Fatalf("newest points/truncation not preserved: %s", got)
	}
	if len(got) > 120 {
		t.Fatalf("response has %d bytes, exceeds 120", len(got))
	}
}

func TestOpsResultAppliesSameBudgetToTextAndStructuredContent(t *testing.T) {
	body := json.RawMessage(`{"count":9007199254740993}`)
	result, structured, err := opsResult(body, 40, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if text != string(encoded) || !strings.Contains(text, "9007199254740993") {
		t.Fatalf("text %s and structured output %s differ", text, encoded)
	}
	if _, _, err := opsResult(json.RawMessage(`{"metrics":"`+strings.Repeat("x", 64)+`"}`), 24, false); err == nil {
		t.Fatal("oversized snapshot should return a bounded error")
	}
}

func TestRegisterOpsOnlyExposesTwoReadOnlyStructuredTools(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/ready":
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"not_ready","reason":"recovering"}`)
		case "/api/v1/metrics/timeseries/api":
			if r.URL.Query().Get("duration_minutes") != "30" || r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("unexpected metrics request: %s?%s auth=%q", r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"))
			}
			fmt.Fprint(w, `{"type":"api","counter":9007199254740993,"data":[{"timestamp":"now"}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	cs := newSession(t, handler, func(server *mcp.Server, client *arc.Client) { RegisterOps(server, client, 50_000) })
	listed, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 2 {
		t.Fatalf("registered %d ops tools, want 2", len(listed.Tools))
	}
	for _, tool := range listed.Tools {
		if tool.Name != "arc_health" && tool.Name != "arc_metrics" {
			t.Errorf("unexpected tool %q", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.OutputSchema == nil {
			t.Errorf("%s lacks read-only annotation or output schema", tool.Name)
		}
	}

	health, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "arc_health", Arguments: map[string]any{}})
	if err != nil || health.IsError {
		t.Fatalf("health result = %#v, %v", health, err)
	}
	healthJSON, err := json.Marshal(health.StructuredContent)
	if err != nil || !strings.Contains(string(healthJSON), `"not_ready"`) {
		t.Fatalf("readiness snapshot missing from structured output: %s (%v)", healthJSON, err)
	}

	metrics, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "arc_metrics", Arguments: map[string]any{"family": "timeseries", "type": "api"}})
	if err != nil || metrics.IsError {
		t.Fatalf("metrics result = %#v, %v", metrics, err)
	}
	metricsJSON, err := json.Marshal(metrics.StructuredContent)
	if err != nil || len(metricsJSON) == 0 {
		t.Fatalf("metrics structured output missing: %s (%v)", metricsJSON, err)
	}
	if text := metrics.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "9007199254740993") {
		t.Fatalf("exact metric value missing from the MCP text and wire output: %s", text)
	}
}

func TestRegisterOpsRejectsInvalidMetricsArgs(t *testing.T) {
	cs := newSession(t, http.NotFoundHandler(), func(server *mcp.Server, client *arc.Client) { RegisterOps(server, client, 50_000) })
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "arc_metrics", Arguments: map[string]any{"family": "timeseries", "type": "api", "duration_minutes": 1441}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("invalid duration accepted: %#v", result)
	}
}

func TestOpsTimeoutHonoursRequestContext(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	c := arc.NewClient(slow.URL, "", time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	_, err := c.GetJSON(ctx, "/health", nil)
	apiErr, ok := err.(*arc.Error)
	if !ok || apiErr.Kind != arc.KindTimeout {
		t.Fatalf("error = %#v, want a timeout", err)
	}
	if got := opsError(err).Error(); got != "The Arc observability request timed out." {
		t.Errorf("timeout message = %q", got)
	}
}

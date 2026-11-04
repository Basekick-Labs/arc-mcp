package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EmptyObservabilityArgs is used by snapshot tools that need no input.
type EmptyObservabilityArgs struct{}

// TimeSeriesMetricsArgs selects a supported Arc time-series metric family.
type TimeSeriesMetricsArgs struct {
	Type            string `json:"type" jsonschema:"Metric family: system, application, or api"`
	DurationMinutes int    `json:"duration_minutes,omitempty" jsonschema:"Look-back window in minutes (default 30, maximum 1440)"`
}

// ApplicationLogsArgs filters Arc's buffered application logs. These are
// server logs, not log records stored in Arc measurements.
type ApplicationLogsArgs struct {
	Level        string `json:"level,omitempty" jsonschema:"Minimum severity: debug, info, warn, error, or fatal"`
	SinceMinutes int    `json:"since_minutes,omitempty" jsonschema:"Look-back window in minutes (default 60, maximum 1440)"`
	Limit        int    `json:"limit,omitempty" jsonschema:"Maximum entries (default 100, maximum 1000)"`
}

// RegisterObservability registers read-only health, readiness, metrics, and application-log tools.
func RegisterObservability(server *mcp.Server, client *arc.Client, maxResponseChars int) {
	registerSnapshotTool(server, client, maxResponseChars, "arc_health", "Check Arc liveness and return the health snapshot, including configured storage and license status when present.", "/health")
	registerSnapshotTool(server, client, maxResponseChars, "arc_readiness", "Check whether Arc is ready to receive traffic. A not-ready response (HTTP 503) is returned as a normal readiness snapshot with its reason.", "/ready")
	registerSnapshotTool(server, client, maxResponseChars, "arc_metrics", "Return Arc's aggregate JSON metrics snapshot.", "/api/v1/metrics")
	registerSnapshotTool(server, client, maxResponseChars, "arc_memory_metrics", "Return Go runtime, heap, stack, and garbage-collection metrics from Arc.", "/api/v1/metrics/memory")
	registerSnapshotTool(server, client, maxResponseChars, "arc_query_pool_metrics", "Return Arc DuckDB connection-pool and query metrics.", "/api/v1/metrics/query-pool")
	registerSnapshotTool(server, client, maxResponseChars, "arc_endpoint_metrics", "Return Arc HTTP, ingest, query, buffer, storage, compaction, and auth endpoint metrics.", "/api/v1/metrics/endpoints")

	mcp.AddTool(server, &mcp.Tool{
		Name:        "arc_timeseries_metrics",
		Description: "Return bounded Arc time-series metrics for the system, application, or API family. Duration is limited to 1–1440 minutes.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args TimeSeriesMetricsArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)
		query, err := buildTimeSeriesQuery(args)
		if err != nil {
			return observabilityError(err), nil, nil
		}
		metricType := strings.ToLower(strings.TrimSpace(args.Type))
		return fetchObservability(ctx, client, "/api/v1/metrics/timeseries/"+metricType, query, maxResponseChars), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "arc_application_logs",
		Description: "Read Arc's buffered application/server logs (not log records stored in measurements). Filters are bounded by severity, look-back minutes, and result limit. Arc requires an admin token for this endpoint.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ApplicationLogsArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)
		query, err := buildApplicationLogsQuery(args)
		if err != nil {
			return observabilityError(err), nil, nil
		}
		return fetchObservability(ctx, client, "/api/v1/logs", query, maxResponseChars), nil, nil
	})
}

func registerSnapshotTool(server *mcp.Server, client *arc.Client, maxResponseChars int, name, description, endpoint string) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        name,
		Description: description,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyObservabilityArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)
		return fetchObservability(ctx, client, endpoint, nil, maxResponseChars), nil, nil
	})
}

func fetchObservability(ctx context.Context, client *arc.Client, endpoint string, query url.Values, maxResponseChars int) *mcp.CallToolResult {
	body, err := client.GetJSON(ctx, endpoint, query)
	if err != nil {
		return observabilityError(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		return observabilityError(fmt.Errorf("formatting Arc response: %w", err))
	}
	text := pretty.String()
	if maxResponseChars > 0 {
		text = TruncateResponse(text, maxResponseChars)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func observabilityError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: arc.UserMessage(err)}},
		IsError: true,
	}
}

func buildTimeSeriesQuery(args TimeSeriesMetricsArgs) (url.Values, error) {
	metricType := strings.ToLower(strings.TrimSpace(args.Type))
	switch metricType {
	case "system", "application", "api":
	default:
		return nil, fmt.Errorf("metric type must be one of: system, application, api")
	}
	duration := args.DurationMinutes
	if duration == 0 {
		duration = 30
	}
	if duration < 1 || duration > 1440 {
		return nil, fmt.Errorf("duration_minutes must be between 1 and 1440")
	}
	query := make(url.Values)
	query.Set("duration_minutes", strconv.Itoa(duration))
	return query, nil
}

func buildApplicationLogsQuery(args ApplicationLogsArgs) (url.Values, error) {
	level := strings.ToLower(strings.TrimSpace(args.Level))
	switch level {
	case "", "debug", "info", "warn", "error", "fatal":
	default:
		return nil, fmt.Errorf("level must be one of: debug, info, warn, error, fatal")
	}
	limit := args.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("limit must be between 1 and 1000")
	}
	since := args.SinceMinutes
	if since == 0 {
		since = 60
	}
	if since < 1 || since > 1440 {
		return nil, fmt.Errorf("since_minutes must be between 1 and 1440")
	}
	query := make(url.Values)
	if level != "" {
		query.Set("level", level)
	}
	query.Set("limit", strconv.Itoa(limit))
	query.Set("since_minutes", strconv.Itoa(since))
	return query, nil
}

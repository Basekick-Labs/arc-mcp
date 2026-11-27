package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MetricsArgs selects one bounded Arc metrics family.
type MetricsArgs struct {
	Family          string `json:"family" jsonschema:"Metrics family: aggregate, memory, query-pool, endpoints, or timeseries"`
	Type            string `json:"type,omitempty" jsonschema:"Timeseries family: system, application, or api (required for timeseries)"`
	DurationMinutes int    `json:"duration_minutes,omitempty" jsonschema:"Timeseries look-back in minutes (default 30, maximum 1440)"`
}

type noArgs struct{}

// RegisterOps exposes the two opt-in operational tools. It deliberately omits
// Arc application logs: that endpoint requires an admin token, unlike the
// read-only token recommended for arc-mcp.
func RegisterOps(server *mcp.Server, client *arc.Client, maxResponseChars int) {
	mcp.AddTool[noArgs, *json.RawMessage](server, &mcp.Tool{
		Name:         "arc_health",
		Title:        "Check Arc health and readiness",
		Annotations:  readOnly("Check Arc health and readiness"),
		Description:  "Return the Arc liveness and readiness snapshots together. A not-ready response is a normal state with a reason, not a tool failure.",
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"health":{"type":"object","additionalProperties":true},"readiness":{"type":"object","additionalProperties":true}},"required":["health","readiness"],"additionalProperties":false}`),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, *json.RawMessage, error) {
		health, err := client.GetJSON(ctx, "/health", nil)
		if err != nil {
			return nil, nil, opsError(err)
		}
		readiness, err := client.GetJSON(ctx, "/ready", nil)
		if err != nil {
			return nil, nil, opsError(err)
		}
		body, err := json.Marshal(struct {
			Health    json.RawMessage `json:"health"`
			Readiness json.RawMessage `json:"readiness"`
		}{Health: health, Readiness: readiness})
		if err != nil {
			return nil, nil, fmt.Errorf("encoding Arc health response: %w", err)
		}
		return opsResult(body, maxResponseChars, false)
	})

	mcp.AddTool[MetricsArgs, *json.RawMessage](server, &mcp.Tool{
		Name:         "arc_metrics",
		Title:        "Read Arc metrics",
		Annotations:  readOnly("Read Arc metrics"),
		Description:  "Read one Arc metrics family: aggregate, memory, query-pool, endpoints, or timeseries. Timeseries requires type=system, application, or api and accepts a 1–1440 minute look-back (default 30). Metrics describe only the Arc node at ARC_URL.",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"family":{"type":"string","enum":["aggregate","memory","query-pool","endpoints","timeseries"]},"type":{"type":"string","enum":["system","application","api"]},"duration_minutes":{"type":"integer","minimum":1,"maximum":1440}},"required":["family"],"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args MetricsArgs) (*mcp.CallToolResult, *json.RawMessage, error) {
		endpoint, query, timeseries, err := metricsRequest(args)
		if err != nil {
			return nil, nil, err
		}
		body, err := client.GetJSON(ctx, endpoint, query)
		if err != nil {
			return nil, nil, opsError(err)
		}
		return opsResult(body, maxResponseChars, timeseries)
	})
}

func opsError(err error) error {
	var apiErr *arc.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Kind {
		case arc.KindTimeout:
			return errors.New("arc observability request timed out")
		case arc.KindForbidden:
			return errors.New("arc denied this observability request; check token permissions and server configuration")
		}
	}
	return errors.New(arc.UserMessage(err))
}

func metricsRequest(args MetricsArgs) (string, url.Values, bool, error) {
	switch args.Family {
	case "aggregate":
		if args.Type != "" || args.DurationMinutes != 0 {
			return "", nil, false, errors.New("type and duration_minutes are only valid for timeseries")
		}
		return "/api/v1/metrics", nil, false, nil
	case "memory":
		if args.Type != "" || args.DurationMinutes != 0 {
			return "", nil, false, errors.New("type and duration_minutes are only valid for timeseries")
		}
		return "/api/v1/metrics/memory", nil, false, nil
	case "query-pool":
		if args.Type != "" || args.DurationMinutes != 0 {
			return "", nil, false, errors.New("type and duration_minutes are only valid for timeseries")
		}
		return "/api/v1/metrics/query-pool", nil, false, nil
	case "endpoints":
		if args.Type != "" || args.DurationMinutes != 0 {
			return "", nil, false, errors.New("type and duration_minutes are only valid for timeseries")
		}
		return "/api/v1/metrics/endpoints", nil, false, nil
	case "timeseries":
		if args.Type != "system" && args.Type != "application" && args.Type != "api" {
			return "", nil, false, errors.New("timeseries type must be system, application, or api")
		}
		minutes := args.DurationMinutes
		if minutes == 0 {
			minutes = 30
		}
		if minutes < 1 || minutes > 1440 {
			return "", nil, false, errors.New("duration_minutes must be between 1 and 1440")
		}
		query := make(url.Values)
		query.Set("duration_minutes", strconv.Itoa(minutes))
		return "/api/v1/metrics/timeseries/" + args.Type, query, true, nil
	default:
		return "", nil, false, errors.New("family must be aggregate, memory, query-pool, endpoints, or timeseries")
	}
}

// opsResult keeps the structured JSON and text response under the same size
// limit. Time-series responses retain the newest points and report how many
// older points were omitted; bounded snapshots fail clearly if they exceed it.
func opsResult(body json.RawMessage, maxResponseChars int, timeseries bool) (*mcp.CallToolResult, *json.RawMessage, error) {
	if maxResponseChars > 0 {
		var compact bytes.Buffer
		if err := json.Compact(&compact, body); err != nil {
			return nil, nil, fmt.Errorf("compacting Arc response: %w", err)
		}
		body = compact.Bytes()
		if len(body) > maxResponseChars {
			if !timeseries {
				return nil, nil, errors.New("arc metrics response exceeds --max-response-size")
			}
			bounded, err := trimTimeSeries(body, maxResponseChars)
			if err != nil {
				return nil, nil, err
			}
			body = bounded
		}
	}
	structured := json.RawMessage(append([]byte(nil), body...))
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, &structured, nil
}

func trimTimeSeries(body []byte, maxChars int) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		return nil, errors.New("arc returned a timeseries response that could not be bounded")
	}
	points, ok := payload["data"].([]any)
	if !ok {
		return nil, errors.New("arc timeseries response exceeds --max-response-size and has no data array to trim")
	}
	lo, hi := 0, len(points)
	for lo < hi {
		keep := lo + (hi-lo+1)/2
		payload["data"] = points[len(points)-keep:]
		payload["truncated_points"] = json.Number(strconv.Itoa(len(points) - keep))
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encoding bounded Arc timeseries response: %w", err)
		}
		if len(encoded) <= maxChars {
			lo = keep
		} else {
			hi = keep - 1
		}
	}
	payload["data"] = points[len(points)-lo:]
	payload["truncated_points"] = json.Number(strconv.Itoa(len(points) - lo))
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding bounded Arc timeseries response: %w", err)
	}
	if len(encoded) > maxChars {
		return nil, errors.New("arc timeseries metadata exceeds --max-response-size")
	}
	return encoded, nil
}

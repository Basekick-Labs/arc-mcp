package tools

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// QueryArgs is the input for the query tool.
type QueryArgs struct {
	Database string `json:"database" jsonschema:"Database name"`
	SQL      string `json:"sql"      jsonschema:"Read-only SQL query (DuckDB dialect). Arc supports standard analytical SQL including aggregations, JOINs, CTEs, window functions, and time helpers like time_bucket() and date_trunc()."`
}

// GetSampleDataArgs is the input for get_sample_data.
type GetSampleDataArgs struct {
	Database    string `json:"database"    jsonschema:"Database name"`
	Measurement string `json:"measurement" jsonschema:"Measurement (table) name"`
	Limit       int    `json:"limit"       jsonschema:"Number of rows to return (default 10 and max 100)"`
}

// RegisterQuery registers the query tool.
func RegisterQuery(server *mcp.Server, client *arc.Client, maxRows int, maxResponseChars int) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "query",
		Description: "Execute a read-only SQL query against Arc (DuckDB SQL dialect). Supports SELECT, aggregations, JOINs, CTEs, time_bucket(), date_trunc(), and more. Write operations (INSERT, UPDATE, DELETE, DROP) are blocked.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args QueryArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)
		if args.Database == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: database name is required"}},
				IsError: true,
			}, nil, nil
		}
		if err := ValidateIdentifier(args.Database); err != nil {
			log.Printf("query: invalid database identifier %q: %v", args.Database, err)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: invalid database name"}},
				IsError: true,
			}, nil, nil
		}
		if args.SQL == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: SQL query is required"}},
				IsError: true,
			}, nil, nil
		}

		// Safety: reject write operations. Advisory only — Arc server-side is authoritative.
		if err := ValidateReadOnly(args.SQL); err != nil {
			log.Printf("query: rejected by read-only validator: %v; sql=%q", err, truncateForLog(args.SQL))
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Blocked: %v", err)}},
				IsError: true,
			}, nil, nil
		}

		// Enforce row limit
		sql := EnforceRowLimit(args.SQL, maxRows)

		result, err := client.Query(ctx, args.Database, sql)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: arc.UserMessage(err)}},
				IsError: true,
			}, nil, nil
		}

		text := formatQueryResult(result)
		if maxResponseChars > 0 {
			text = TruncateResponse(text, maxResponseChars)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	})
}

// RegisterGetSampleData registers the get_sample_data tool.
func RegisterGetSampleData(server *mcp.Server, client *arc.Client, maxResponseChars int) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_sample_data",
		Description: "Get recent sample rows from a measurement, ordered by time descending. Useful for understanding the data shape and recent values.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args GetSampleDataArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)
		if args.Database == "" || args.Measurement == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: both database and measurement are required"}},
				IsError: true,
			}, nil, nil
		}
		if err := ValidateIdentifier(args.Database); err != nil {
			log.Printf("get_sample_data: invalid database identifier %q: %v", args.Database, err)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: invalid database name"}},
				IsError: true,
			}, nil, nil
		}
		if err := ValidateIdentifier(args.Measurement); err != nil {
			log.Printf("get_sample_data: invalid measurement identifier %q: %v", args.Measurement, err)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Error: invalid measurement name"}},
				IsError: true,
			}, nil, nil
		}

		limit := args.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 100 {
			limit = 100
		}

		// Safe to interpolate: ValidateIdentifier ensured args.Measurement is [A-Za-z_][A-Za-z0-9_]*.
		sql := fmt.Sprintf("SELECT * FROM %s ORDER BY time DESC LIMIT %d", args.Measurement, limit)
		result, err := client.Query(ctx, args.Database, sql)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: arc.UserMessage(err)}},
				IsError: true,
			}, nil, nil
		}

		text := formatQueryResult(result)
		if maxResponseChars > 0 {
			text = TruncateResponse(text, maxResponseChars)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	})
}

// formatQueryResult formats a QueryResponse as a readable markdown table.
func formatQueryResult(result *arc.QueryResponse) string {
	if len(result.Columns) == 0 {
		return fmt.Sprintf("Query returned no columns. (%d rows, %.1fms)", result.RowCount, result.ExecutionTimeMs)
	}

	var sb strings.Builder

	// Header
	sb.WriteString("| ")
	for i, col := range result.Columns {
		if i > 0 {
			sb.WriteString(" | ")
		}
		sb.WriteString(escapeMarkdownCell(col))
	}
	sb.WriteString(" |\n")

	// Separator
	sb.WriteString("|")
	for range result.Columns {
		sb.WriteString("---|")
	}
	sb.WriteString("\n")

	// Rows
	for _, row := range result.Data {
		sb.WriteString("| ")
		for i, val := range row {
			if i > 0 {
				sb.WriteString(" | ")
			}
			sb.WriteString(escapeMarkdownCell(val))
		}
		sb.WriteString(" |\n")
	}

	fmt.Fprintf(&sb, "\n*%d rows returned in %.1fms*", result.RowCount, result.ExecutionTimeMs)

	return sb.String()
}

// truncateForLog shortens a SQL preview for stderr logging to avoid filling
// operator logs with large queries. Never appears in LLM-visible output.
func truncateForLog(s string) string {
	const maxLogLen = 256
	if len(s) <= maxLogLen {
		return s
	}
	return s[:maxLogLen] + "..."
}

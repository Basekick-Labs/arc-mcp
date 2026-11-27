package tools

import (
	"context"
	"fmt"
	"log"
	"strconv"
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
	Limit       int    `json:"limit,omitempty" jsonschema:"Number of rows to return (default 10 and max 100)"`
}

// RegisterQuery registers the query tool. maxRows must be within
// MinRowLimit..MaxRowLimit (see ClampRowLimit).
func RegisterQuery(server *mcp.Server, client *arc.Client, maxRows int, maxResponseChars int) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "query",
		Title:       "Run a read-only SQL query",
		Annotations: readOnly("Run a read-only SQL query"),
		Description: "Execute one read-only SQL query against Arc (DuckDB SQL dialect). Supports SELECT, aggregations, JOINs, CTEs, time_bucket(), date_trunc(), and more. " +
			"Bound it by time with a top-level WHERE on time (time > now() - INTERVAL '1 hour') in a single-table query: Arc then reads only those partitions; a JOIN, CTE or subquery reads all history. " +
			"At most " + strconv.Itoa(maxRows) + " rows come back; when there are more, the answer says so at the top. " +
			"Arc refuses some words anywhere in a query, even as column names or aliases (load, copy, set, pragma, call, ...): double-quote such a name, as in AS \"load\".",
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

		// Answer early, with a reason, for anything Arc would refuse. Arc's
		// own checks remain the boundary.
		q, err := ValidateReadOnly(args.SQL)
		if err != nil {
			log.Printf("query: rejected by read-only validator: %v; sql=%q", err, truncateForLog(args.SQL))
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Blocked: %v", err)}},
				IsError: true,
			}, nil, nil
		}

		result, err := client.Query(ctx, args.Database, q.WithRowLimit(maxRows))
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: arc.UserMessage(err)}},
				IsError: true,
			}, nil, nil
		}

		// Arc was asked for maxRows+1 rows; getting them all means there are
		// more. Statements sent unchanged are cut here too.
		var notes []string
		if len(result.Data) > maxRows {
			result.Data = result.Data[:maxRows]
			hint := "Narrow the statement to see the rest."
			if q.Selectable() {
				hint = "Aggregate (time_bucket, GROUP BY) or narrow the time range to see all of it."
			}
			notes = append(notes, fmt.Sprintf("**Showing the first %s; the query returned more.** %s", plural(maxRows, "row"), hint))
		}
		if result.RowsCapped {
			notes = append(notes, fmt.Sprintf("**Arc's row cap (%d) cut this result; more rows exist.** Aggregate or narrow the time range.", result.RowCap))
		}
		if result.Truncated {
			log.Printf("query: Arc truncated the result: %s", result.TruncationReason)
			if why := result.FixableTruncation(); why != "" {
				// A failure the query itself causes (a bad cast on a later
				// row, say) happens again on every retry: say what it is.
				notes = append(notes, "**Arc stopped partway through this result, so it is incomplete:** "+why+" Fix the query; running it again fails the same way.")
			} else {
				notes = append(notes, "**Arc stopped partway through this result, so it is incomplete.** Try again, or narrow the query.")
			}
		}
		noData := fmt.Sprintf("No data: Arc found nothing to read for this query. A measurement that does not exist in database '%s', "+
			"or has no data written yet, reads as empty (an aggregate over it returns nothing, not 0). Check the name with list_measurements.", args.Database)
		text := formatQueryResult(result, notes, noData)
		if maxResponseChars > 0 {
			text = TruncateResponse(text, maxResponseChars)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	})
}

// sampleWindows are the time bounds get_sample_data tries in turn, each a
// top-level condition Arc prunes partitions by.
var sampleWindows = []string{
	" WHERE time > now() - INTERVAL '1 day'",
	" WHERE time > now() - INTERVAL '30 days'",
	"",
}

// RegisterGetSampleData registers the get_sample_data tool.
func RegisterGetSampleData(server *mcp.Server, client *arc.Client, maxResponseChars int) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_sample_data",
		Title:       "Sample recent rows",
		Annotations: readOnly("Sample recent rows"),
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

		// The most recent rows, without reading all history to find them:
		// the last day first, then the last 30 days, then everything. A
		// measurement with no files answers with no columns at the first
		// step, and that answer is final.
		var result *arc.QueryResponse
		for _, window := range sampleWindows {
			// Safe to interpolate: ValidateIdentifier ensured args.Measurement is [A-Za-z_][A-Za-z0-9_]*.
			sql := fmt.Sprintf("SELECT * FROM %s%s ORDER BY time DESC LIMIT %d", args.Measurement, window, limit)
			r, err := client.Query(ctx, args.Database, sql)
			if err != nil {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: arc.UserMessage(err)}},
					IsError: true,
				}, nil, nil
			}
			result = r
			if len(r.Columns) == 0 || len(r.Data) > 0 {
				break
			}
		}

		noData := fmt.Sprintf("No data: measurement '%s' does not exist in database '%s', or has no data written yet. Check the name with list_measurements.", args.Measurement, args.Database)
		text := formatQueryResult(result, nil, noData)
		if maxResponseChars > 0 {
			text = TruncateResponse(text, maxResponseChars)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	})
}

// formatQueryResult formats a QueryResponse as a readable markdown table.
// notes say the result is incomplete and why. They go first, so they
// survive when TruncateResponse later cuts the text from the end. noData is
// the answer when Arc read nothing at all: it returns no columns only when it
// found no files, which means a measurement that does not exist or has no
// data (a time range that matches nothing still returns the columns).
func formatQueryResult(result *arc.QueryResponse, notes []string, noData string) string {
	if len(result.Columns) == 0 {
		return noData
	}

	var sb strings.Builder
	for _, n := range notes {
		sb.WriteString(n)
		sb.WriteString("\n\n")
	}

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

	verb := "returned"
	if len(notes) > 0 {
		verb = "shown"
	}
	fmt.Fprintf(&sb, "\n*%s %s in %.1fms*", plural(len(result.Data), "row"), verb, result.ExecutionTimeMs)

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

// plural renders "1 row" / "2 rows".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

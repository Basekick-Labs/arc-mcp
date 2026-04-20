package tools

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ListMeasurementsArgs is the input for list_measurements.
type ListMeasurementsArgs struct {
	Database string `json:"database" jsonschema:"Database name"`
}

// DescribeMeasurementArgs is the input for describe_measurement.
type DescribeMeasurementArgs struct {
	Database    string `json:"database"    jsonschema:"Database name"`
	Measurement string `json:"measurement" jsonschema:"Measurement (table) name"`
}

// RegisterListMeasurements registers the list_measurements tool.
func RegisterListMeasurements(server *mcp.Server, client *arc.Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_measurements",
		Description: "List all measurements (tables) in a database. Returns measurement names.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListMeasurementsArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)
		if args.Database == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "Error: database name is required"},
				},
				IsError: true,
			}, nil, nil
		}
		if err := ValidateIdentifier(args.Database); err != nil {
			log.Printf("list_measurements: invalid database identifier %q: %v", args.Database, err)
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "Error: invalid database name"},
				},
				IsError: true,
			}, nil, nil
		}

		result, err := client.ListMeasurements(ctx, args.Database)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: arc.UserMessage(err)},
				},
				IsError: true,
			}, nil, nil
		}

		if len(result.Measurements) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: fmt.Sprintf("No measurements found in database '%s'.", args.Database)},
				},
			}, nil, nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "Database '%s' has %d measurement(s):\n\n", args.Database, result.Count)
		for _, m := range result.Measurements {
			fmt.Fprintf(&sb, "- %s\n", m.Name)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: sb.String()},
			},
		}, nil, nil
	})
}

// RegisterDescribeMeasurement registers the describe_measurement tool.
func RegisterDescribeMeasurement(server *mcp.Server, client *arc.Client, maxResponseChars int) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "describe_measurement",
		Description: "Describe a measurement's schema — column names, types, row count, and time range. Use this before writing queries to understand the data structure.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args DescribeMeasurementArgs) (out *mcp.CallToolResult, _ any, _ error) {
		defer RecoverToolPanic(&out)
		if args.Database == "" || args.Measurement == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "Error: both database and measurement are required"},
				},
				IsError: true,
			}, nil, nil
		}
		if err := ValidateIdentifier(args.Database); err != nil {
			log.Printf("describe_measurement: invalid database identifier %q: %v", args.Database, err)
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "Error: invalid database name"},
				},
				IsError: true,
			}, nil, nil
		}
		if err := ValidateIdentifier(args.Measurement); err != nil {
			log.Printf("describe_measurement: invalid measurement identifier %q: %v", args.Measurement, err)
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "Error: invalid measurement name"},
				},
				IsError: true,
			}, nil, nil
		}

		// Safe to interpolate: ValidateIdentifier ensured args.Measurement is [A-Za-z_][A-Za-z0-9_]*.
		schemaSQL := fmt.Sprintf("SELECT * FROM %s LIMIT 0", args.Measurement)
		schemaResult, err := client.Query(ctx, args.Database, schemaSQL)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: arc.UserMessage(err)},
				},
				IsError: true,
			}, nil, nil
		}

		// Get stats: count, min/max time
		statsSQL := fmt.Sprintf("SELECT count(*) as row_count, min(time) as earliest, max(time) as latest FROM %s", args.Measurement)
		statsResult, err := client.Query(ctx, args.Database, statsSQL)

		var sb strings.Builder
		fmt.Fprintf(&sb, "## Measurement: %s.%s\n\n", args.Database, args.Measurement)

		// Schema
		sb.WriteString("### Columns\n\n")
		if len(schemaResult.Columns) > 0 {
			sb.WriteString("| Column | Type |\n|--------|------|\n")
			// We get columns from the schema query; types come from the first row if DESCRIBE is used,
			// but LIMIT 0 only gives column names. Use DESCRIBE for types.
			for _, col := range schemaResult.Columns {
				fmt.Fprintf(&sb, "| %s | — |\n", col)
			}
		}

		// Try to get column types via DESCRIBE
		describeSQL := fmt.Sprintf("DESCRIBE %s", args.Measurement)
		descResult, descErr := client.Query(ctx, args.Database, describeSQL)
		if descErr == nil && len(descResult.Data) > 0 {
			sb.Reset()
			fmt.Fprintf(&sb, "## Measurement: %s.%s\n\n", args.Database, args.Measurement)
			sb.WriteString("### Columns\n\n")
			sb.WriteString("| Column | Type | Nullable |\n|--------|------|----------|\n")
			for _, row := range descResult.Data {
				if len(row) == 0 {
					continue
				}
				colName := escapeMarkdownCell(row[0])
				colType := ""
				nullable := ""
				if len(row) > 1 {
					colType = escapeMarkdownCell(row[1])
				}
				if len(row) > 2 {
					nullable = escapeMarkdownCell(row[2])
				}
				fmt.Fprintf(&sb, "| %s | %s | %s |\n", colName, colType, nullable)
			}
		}

		// Stats
		if err == nil && len(statsResult.Data) > 0 && len(statsResult.Data[0]) >= 3 {
			row := statsResult.Data[0]
			sb.WriteString("\n### Statistics\n\n")
			fmt.Fprintf(&sb, "- **Row count:** %v\n", row[0])
			fmt.Fprintf(&sb, "- **Earliest:** %v\n", row[1])
			fmt.Fprintf(&sb, "- **Latest:** %v\n", row[2])
		}

		text := sb.String()
		if maxResponseChars > 0 {
			text = TruncateResponse(text, maxResponseChars)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: text},
			},
		}, nil, nil
	})
}

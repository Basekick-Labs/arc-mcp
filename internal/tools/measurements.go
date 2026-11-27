package tools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
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
		Annotations: readOnly("List measurements"),
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
		if arc.Scoped(err) {
			// The token reads some databases but not this one. Naming a
			// different database is the only thing that resolves it, and
			// only the user knows which — so say that rather than
			// returning a tool error the model can only retry.
			return ScopedResult(args.Database, "list_measurements"), nil, nil
		}
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
		Annotations: readOnly("Describe a measurement"),
		Description: "Describe a measurement: column names and types, row count, and the time range of its data. Call it before querying; use the time range to choose the WHERE time bound. Its statistics scan the whole measurement.",
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

		columns, err := describeColumns(ctx, client, args.Database, args.Measurement)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: arc.UserMessage(err)},
				},
				IsError: true,
			}, nil, nil
		}
		if len(columns) == 0 {
			// Arc answers a measurement with no files, or no such
			// measurement, with an empty result rather than a 404.
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: fmt.Sprintf(
						"Measurement '%s' has no data in database '%s', or does not exist. Call list_measurements to see what exists.",
						args.Measurement, args.Database)},
				},
				IsError: true,
			}, nil, nil
		}

		// Safe to interpolate: ValidateIdentifier ensured args.Measurement is [A-Za-z_][A-Za-z0-9_]*.
		statsSQL := fmt.Sprintf("SELECT count(*) as row_count, min(time) as earliest, max(time) as latest FROM %s", args.Measurement)
		statsResult, err := client.Query(ctx, args.Database, statsSQL)

		var sb strings.Builder
		fmt.Fprintf(&sb, "## Measurement: %s.%s\n\n", args.Database, args.Measurement)
		sb.WriteString("### Columns\n\n")
		sb.WriteString("| Column | Type |\n|--------|------|\n")
		for _, c := range columns {
			fmt.Fprintf(&sb, "| %s | %s |\n", escapeMarkdownCell(c.name), escapeMarkdownCell(c.typ))
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

// unknownType stands in for a column type Arc did not report.
const unknownType = "—"

// column is one column of a measurement; typ is unknownType when unknown.
type column struct {
	name, typ string
}

// describeColumns returns a measurement's columns and types.
//
// `DESCRIBE <measurement>` does not work on Arc (the measurement is not a
// DuckDB table), but `DESCRIBE SELECT * FROM <measurement>` does: Arc
// rewrites the FROM as it does for any query. If an Arc version refuses
// that statement (see describeRefused), fall back to
// `SELECT * FROM <measurement> LIMIT 0` for the names alone. Any other
// failure (auth, a timeout, a broken file, Arc unreachable) is returned as
// is: a second query would fail the same way and double the wait.
//
// No columns and no error means Arc found no data for the measurement.
func describeColumns(ctx context.Context, client *arc.Client, database, measurement string) ([]column, error) {
	// Safe to interpolate: the caller validated measurement with ValidateIdentifier.
	desc, err := client.Query(ctx, database, fmt.Sprintf("DESCRIBE SELECT * FROM %s", measurement))
	if err == nil {
		nameIdx, typeIdx := indexOf(desc.Columns, "column_name", 0), indexOf(desc.Columns, "column_type", 1)
		var cols []column
		for _, row := range desc.Data {
			if nameIdx >= len(row) {
				continue
			}
			c := column{name: fmt.Sprintf("%v", row[nameIdx]), typ: unknownType}
			if typeIdx < len(row) && row[typeIdx] != nil {
				c.typ = fmt.Sprintf("%v", row[typeIdx])
			}
			cols = append(cols, c)
		}
		return cols, nil
	}
	if !describeRefused(err) {
		return nil, err
	}
	log.Printf("describe_measurement: DESCRIBE SELECT refused (%v); falling back to column names only", err)

	names, err := client.Query(ctx, database, fmt.Sprintf("SELECT * FROM %s LIMIT 0", measurement))
	if err != nil {
		return nil, err
	}
	cols := make([]column, len(names.Columns))
	for i, n := range names.Columns {
		cols[i] = column{name: n, typ: unknownType}
	}
	return cols, nil
}

// describeRefused reports whether Arc refused the DESCRIBE statement itself,
// as opposed to failing for a reason a second query would hit too: a
// validation refusal (400), a rejected query, or a 500 that is a DuckDB
// parser error. Other 500s (a corrupt file, out of memory)
// would fail the fallback query too.
func describeRefused(err error) bool {
	var ae *arc.Error
	if !errors.As(err, &ae) {
		return false
	}
	switch {
	case ae.Kind == arc.KindQuery:
		return true
	case ae.Kind != arc.KindServer:
		return false
	case ae.Status == http.StatusBadRequest:
		return true
	case ae.Status == http.StatusInternalServerError:
		// Not "mentions DESCRIBE": DuckDB echoes the statement in every
		// error, so that would match a broken file too.
		return strings.Contains(strings.ToLower(ae.Detail), "parser error")
	}
	return false
}

// indexOf returns the position of name in cols, or fallback when absent.
func indexOf(cols []string, name string, fallback int) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return fallback
}

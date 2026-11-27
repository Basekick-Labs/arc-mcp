package tools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

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
		Title:       "List measurements",
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
		Title:       "Describe a measurement",
		Annotations: readOnly("Describe a measurement"),
		Description: "Describe a measurement: column names and types, row count, and the time range of its data. Call it before querying; use the time range to choose the WHERE time bound. Its statistics read every file's metadata and may scan the time column; a statistic that cannot be computed says why.",
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

		budget := deadlineBudget(ctx, client)
		columns, describeMs, err := describeColumns(ctx, client, args.Database, args.Measurement)
		if err != nil {
			text := arc.UserMessage(err)
			if reason := failureReason(budget, err); reason != "" {
				text = "Could not describe the measurement: " + reason + "."
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: text},
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

		stats := measurementStats(ctx, client, args.Database, args.Measurement, describeMs)

		var sb strings.Builder
		fmt.Fprintf(&sb, "## Measurement: %s.%s\n\n", args.Database, args.Measurement)
		sb.WriteString("### Columns\n\n")
		sb.WriteString("| Column | Type |\n|--------|------|\n")
		for _, c := range columns {
			fmt.Fprintf(&sb, "| %s | %s |\n", escapeMarkdownCell(c.name), escapeMarkdownCell(c.typ))
		}

		sb.WriteString("\n### Statistics\n\n")
		for i, label := range statLabels {
			fmt.Fprintf(&sb, "- **%s:** %s\n", label, stats[i])
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

// statLabels are the lines of the Statistics block, in the order of the
// values measurementStats returns.
var statLabels = [3]string{"Row count", "Earliest", "Latest"}

// statsGateMs is the DESCRIBE time, as Arc reports it (execution_time_ms),
// at or above which measurementStats sends the single full statement rather
// than the three-part one. DESCRIBE reads every file's footer, and each part
// of the three-part statement reads them again, so the gate sorts by file
// count, not by rows per file. On measurements of large compacted files the
// parts skip most of the data and win (150M rows in 876 files: DESCRIBE
// 37-49 ms, 0.18 s against 0.49-0.53 s for the full scan); from about 1.4k
// files of up to 50k rows (DESCRIBE 59 ms and up) three footer passes cost
// more than one scan. 55 sits in that measured gap, on Arc main and v26.09.4,
// local storage (arc-mcp#28). Below it the three-part statement can still
// lose on many small files, by three to four DESCRIBE times (four when the
// measurement is stale and the Latest needs its own statement). A time of 0
// (absent, or not Arc) is unknown, and takes the full statement: it is the
// one whose cost is known.
const statsGateMs = 55

// statsLatestWindow bounds the Latest part of the three-part statement. The
// bound is a subquery on purpose, not get_sample_data's top-level
// "time > now() - INTERVAL" (sampleWindows): Arc does not prune partitions by
// a subquery, so rows stamped in the future still count. Released Arc
// (v26.09.4) prunes the plain form up to now+24h only and would report an
// older Latest. DuckDB's own row-group statistics keep it cheap.
const statsLatestWindow = "(SELECT now() - INTERVAL '30 days')"

// measurementStats returns the values of the Row count, Earliest and Latest
// lines: each as Arc rendered it, or "not computed" with the reason.
//
// Below statsGateMs (and above 0) it sends one statement whose parts are each cheap on a
// measurement with few files: count(*) (answered from file metadata), the
// first row by time (DuckDB skips row groups that cannot hold an earlier one)
// and the newest row of the last 30 days. Only when that window is empty and
// the measurement has rows does it send a second statement for the Latest:
// max(time) over everything, the scan today's statement pays. At or above
// the gate, or with no time, it sends the single full statement. Every
// value is exact either
// way and rendered as Arc sent it, so a computed block reads as it always
// has.
//
// All statements share one deadline, arc-mcp's --timeout from the start of
// the statistics (or the caller's, if sooner): a describe takes at most one
// --timeout more than its DESCRIBE. A statement that fails (timed out, refused by Arc's governance,
// a query error) marks its lines "not computed" with the reason, and nothing
// further is sent.
func measurementStats(ctx context.Context, client *arc.Client, database, measurement string, describeMs float64) [3]string {
	budget := deadlineBudget(ctx, client)
	if t := client.Timeout(); t > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	folded := describeMs > 0 && describeMs < statsGateMs
	var out [3]string
	// Safe to interpolate: the caller validated measurement with ValidateIdentifier.
	sql := fmt.Sprintf("SELECT count(*) as row_count, min(time) as earliest, max(time) as latest FROM %s", measurement)
	if folded {
		sql = fmt.Sprintf("SELECT (SELECT count(*) FROM %[1]s) AS row_count, (SELECT time FROM %[1]s ORDER BY time LIMIT 1) AS earliest, "+
			"(SELECT max(time) FROM %[1]s WHERE time > %[2]s) AS latest", measurement, statsLatestWindow)
	}
	row, err := statsRow(ctx, client, database, sql, 3)
	for i := range out {
		out[i] = statValue(budget, row, i, err)
	}
	// An aggregate over an empty window still returns its row, with a null:
	// test the value. A count of 0 leaves the Latest null, as the full
	// statement does. The full statement's max(time) is already unbounded:
	// a null there is final (only rows whose times are all null give one).
	if err != nil || !folded || row[2] != nil || fmt.Sprintf("%v", row[0]) == "0" {
		return out
	}
	row, err = statsRow(ctx, client, database, fmt.Sprintf("SELECT max(time) AS latest FROM %s", measurement), 1)
	out[2] = statValue(budget, row, 0, err)
	return out
}

// statsRow runs one statistics statement and returns its first row, which
// must have at least n values.
func statsRow(ctx context.Context, client *arc.Client, database, sql string, n int) ([]any, error) {
	r, err := client.Query(ctx, database, sql)
	if err != nil {
		return nil, err
	}
	if len(r.Data) == 0 || len(r.Data[0]) < n {
		return nil, &arc.Error{Kind: arc.KindParse, Detail: fmt.Sprintf("statistics: %d rows, want one with %d values", len(r.Data), n)}
	}
	return r.Data[0], nil
}

// statValue renders value i of row, or, when err is set, why it is missing
// (see failureReason).
func statValue(budget time.Duration, row []any, i int, err error) string {
	switch reason := failureReason(budget, err); {
	case err == nil:
		return fmt.Sprintf("%v", row[i])
	case reason != "":
		return "not computed (" + reason + ")"
	default:
		return "not computed: " + arc.UserMessage(err)
	}
}

// failureReason is a short reason for a describe statement that ran out of
// time or memory, or "" for any other failure (UserMessage then says it).
// The query tool's text for those advises aggregating or narrowing the time
// range: advice for SQL the model wrote, which it cannot apply to describe's
// own statements. budget is the time the statement had, 0 when unknown.
func failureReason(budget time.Duration, err error) string {
	var ae *arc.Error
	switch {
	case !errors.As(err, &ae):
		return ""
	case ae.Kind == arc.KindTimeout && budget > 0:
		return "timed out after " + budget.String()
	case ae.Kind == arc.KindTimeout:
		return "timed out"
	case ae.Status == http.StatusGatewayTimeout:
		return "Arc timed out"
	case arc.OutOfMemory(err):
		return "Arc ran out of memory"
	}
	return ""
}

// deadlineBudget is the time a describe statement starting now has: the
// client's --timeout, or less when ctx's own deadline comes sooner; 0 when
// neither is set.
func deadlineBudget(ctx context.Context, client *arc.Client) time.Duration {
	budget := client.Timeout()
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl).Round(time.Millisecond); budget <= 0 || left < budget {
			budget = left
		}
	}
	return budget
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
// No columns and no error means Arc found no data for the measurement. The
// float is the time Arc reports for the statement that answered
// (execution_time_ms; whole milliseconds, 0 when absent), which
// measurementStats gates on.
func describeColumns(ctx context.Context, client *arc.Client, database, measurement string) ([]column, float64, error) {
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
		return cols, desc.ExecutionTimeMs, nil
	}
	if !describeRefused(err) {
		return nil, 0, err
	}
	log.Printf("describe_measurement: DESCRIBE SELECT refused (%v); falling back to column names only", err)

	names, err := client.Query(ctx, database, fmt.Sprintf("SELECT * FROM %s LIMIT 0", measurement))
	if err != nil {
		return nil, 0, err
	}
	cols := make([]column, len(names.Columns))
	for i, n := range names.Columns {
		cols[i] = column{name: n, typ: unknownType}
	}
	return cols, names.ExecutionTimeMs, nil
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

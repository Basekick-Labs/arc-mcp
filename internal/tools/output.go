package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Structured output (#20). Every tool declares an outputSchema and, on
// success, returns structuredContent alongside its markdown text.
//
// The handlers set StructuredContent themselves from json.Marshal of the
// output value, and register with an untyped Out. With a typed Out the SDK
// marshals the value, then decodes it into map[string]any without UseNumber
// to validate it, and re-encodes it: integers above 2^53 change value on the
// wire, the bug #11 fixed for the text. Marshalling here keeps Arc's numbers
// exactly as Arc sent them (json.Number). Error results carry no structured
// content, which the spec allows.
//
// The text stays markdown rather than the serialized JSON the spec suggests
// (a SHOULD): models read a table better than JSON, and clients that want
// the data read structuredContent.

// QueryOutput is the structured result of query and get_sample_data.
type QueryOutput struct {
	// Columns are the result's column names, in order.
	Columns []string `json:"columns"`
	// Rows are the rows shown, each with one value per column. Numbers are
	// exactly as Arc sent them; strings longer than 512 characters are cut
	// with an ellipsis, as in the text.
	Rows [][]any `json:"rows"`
	// RowCount is len(Rows).
	RowCount int `json:"row_count"`
	// Truncated is true when the result is incomplete; Truncation says why:
	// row_limit (arc-mcp's row cap), arc_row_cap (Arc's own cap),
	// size_budget (the answer's size limit), stream_incomplete (Arc stopped
	// partway). The first three leave a valid prefix of the result;
	// stream_incomplete may need the query fixed (see Notes).
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation,omitempty"`
	// Notes say, in plain text, why the result is incomplete and what to do.
	Notes []string `json:"notes,omitempty"`
	// NoData is true when Arc found nothing to read: a measurement that does
	// not exist or has no data.
	NoData          bool    `json:"no_data,omitempty"`
	ExecutionTimeMs float64 `json:"execution_time_ms"`
}

// DatabaseInfo is one database in list_databases.
type DatabaseInfo struct {
	Name             string `json:"name"`
	MeasurementCount int    `json:"measurement_count"`
}

// DatabasesOutput is the structured result of list_databases.
type DatabasesOutput struct {
	Databases []DatabaseInfo `json:"databases"`
	Count     int            `json:"count"`
}

// MeasurementsOutput is the structured result of list_measurements.
type MeasurementsOutput struct {
	Database     string   `json:"database"`
	Measurements []string `json:"measurements"`
	Count        int      `json:"count"`
}

// ColumnInfo is one column in describe_measurement; Type is "" when Arc did
// not report it.
type ColumnInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// StatsOutput is describe_measurement's statistics. A statistic that could
// not be computed is null, with the reason in Errors under its name.
type StatsOutput struct {
	RowCount *int64            `json:"row_count"`
	Earliest *string           `json:"earliest"`
	Latest   *string           `json:"latest"`
	Errors   map[string]string `json:"errors,omitempty"`
}

// DescribeOutput is the structured result of describe_measurement.
type DescribeOutput struct {
	Database    string       `json:"database"`
	Measurement string       `json:"measurement"`
	Columns     []ColumnInfo `json:"columns"`
	Statistics  StatsOutput  `json:"statistics"`
}

// Output schemas. The listing tools can also answer with the scoped outcome
// of #8 (ScopedResult), unchanged, so their schema is the union of both
// shapes with nothing required.
var (
	queryOutputSchema        = mustSchema[QueryOutput]()
	describeOutputSchema     = mustSchema[DescribeOutput]()
	databasesOutputSchema    = unionSchema(mustSchema[DatabasesOutput](), mustSchema[ScopedOutcome]())
	measurementsOutputSchema = unionSchema(mustSchema[MeasurementsOutput](), mustSchema[ScopedOutcome]())
)

func mustSchema[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("output schema for %T: %v", *new(T), err))
	}
	return s
}

// unionSchema is an object schema accepting the properties of a and b, with
// none required.
func unionSchema(a, b *jsonschema.Schema) *jsonschema.Schema {
	u := &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}}
	for _, s := range []*jsonschema.Schema{a, b} {
		for name, p := range s.Properties {
			u.Properties[name] = p
		}
	}
	return u
}

// withStructured adds v as the result's structured content, marshalled here
// so numbers stay exact (see the package comment above).
func withStructured(res *mcp.CallToolResult, v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		// Unreachable for these types; the text still carries the answer.
		return res
	}
	res.StructuredContent = json.RawMessage(b)
	return res
}

// note is one reason a result is incomplete: a short headline, shown in
// bold in the text, and what to do about it.
type note struct {
	reason, headline, hint string
}

func (n note) markdown() string { return strings.TrimSpace("**" + n.headline + "** " + n.hint) }
func (n note) plain() string    { return strings.TrimSpace(n.headline + " " + n.hint) }

// maxCellRunes is where a string cell is cut, in the text and in the
// structured rows alike.
const maxCellRunes = 512

// renderResult renders a query result as markdown and as QueryOutput. When
// maxChars > 0 the table stops at the last row that fits, and the structured
// rows are the same rows, so text and structured content always describe
// the same prefix of the result. noData is the text when Arc read nothing.
func renderResult(result *arc.QueryResponse, notes []note, noData string, maxChars int) (string, QueryOutput) {
	out := QueryOutput{Columns: []string{}, Rows: [][]any{}, ExecutionTimeMs: result.ExecutionTimeMs}
	if len(result.Columns) == 0 {
		out.NoData = true
		return noData, out
	}
	out.Columns = result.Columns

	var table strings.Builder
	table.WriteString("| ")
	for i, col := range result.Columns {
		if i > 0 {
			table.WriteString(" | ")
		}
		table.WriteString(escapeMarkdownCell(col))
	}
	table.WriteString(" |\n|")
	for range result.Columns {
		table.WriteString("---|")
	}
	table.WriteString("\n")

	// Room for the notes, the footer and a size note.
	budget := maxChars - 600
	for _, n := range notes {
		budget -= len(n.markdown()) + 2
	}
	shown := 0
	for _, row := range result.Data {
		var line strings.Builder
		line.WriteString("| ")
		for i, val := range row {
			if i > 0 {
				line.WriteString(" | ")
			}
			line.WriteString(escapeMarkdownCell(val))
		}
		line.WriteString(" |\n")
		if maxChars > 0 && table.Len()+line.Len() > budget && shown > 0 {
			break
		}
		table.WriteString(line.String())
		out.Rows = append(out.Rows, structuredRow(row))
		shown++
	}
	if shown < len(result.Data) {
		notes = append(notes, note{
			reason:   "size_budget",
			headline: fmt.Sprintf("Showing the first %s of %d: the answer reached its size limit.", plural(shown, "row"), len(result.Data)),
			hint:     "Select fewer columns, or aggregate.",
		})
	}
	out.RowCount = shown

	var sb strings.Builder
	for _, n := range notes {
		sb.WriteString(n.markdown())
		sb.WriteString("\n\n")
		out.Notes = append(out.Notes, n.plain())
		out.Truncation = append(out.Truncation, n.reason)
	}
	out.Truncated = len(notes) > 0
	sb.WriteString(table.String())
	verb := "returned"
	if len(notes) > 0 {
		verb = "shown"
	}
	fmt.Fprintf(&sb, "\n*%s %s in %.1fms*", plural(shown, "row"), verb, result.ExecutionTimeMs)
	return sb.String(), out
}

// structuredRow is a row for QueryOutput: values as Arc sent them, strings
// cut at maxCellRunes like the text's cells.
func structuredRow(row []any) []any {
	out := make([]any, len(row))
	for i, v := range row {
		if s, ok := v.(string); ok {
			if r := []rune(s); len(r) > maxCellRunes {
				v = string(r[:maxCellRunes]) + "…"
			}
		}
		out[i] = v
	}
	return out
}

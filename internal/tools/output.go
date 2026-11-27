package tools

import (
	"encoding/json"
	"fmt"
	"log"
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

// Truncation is one reason a result is incomplete.
type Truncation struct {
	Reason  string `json:"reason" jsonschema:"row_limit: arc-mcp's row cap; arc_row_cap: Arc's own row cap; size_budget: the answer's size limit; stream_incomplete: Arc stopped partway. The first three leave a valid prefix of the result."`
	Message string `json:"message" jsonschema:"What happened and what to change, in plain text."`
}

// QueryOutput is the structured result of query and get_sample_data.
type QueryOutput struct {
	Columns     []string `json:"columns" jsonschema:"The result's column names, in order."`
	ColumnTypes []string `json:"column_types" jsonschema:"Arc's type name for each column, parallel to columns: int64, float64, utf8, bool, timestamp[us], date32, decimal(18, 2) and so on. Use it only when its length equals columns'; it is empty when Arc did not report types (an Arc before 27.01.1, the node that answered in a cluster being upgraded, or Arc's fallback path). The names describe the column, not the JSON value: timestamps and dates arrive as RFC 3339 strings in UTC, binary as base64, string_encoded, list, struct, map and unknown: columns as strings, and a float NaN or infinity as null."`
	Rows        [][]any  `json:"rows" jsonschema:"The rows shown, one value per column. Numbers are exactly as Arc sent them (a JavaScript client parsing them as doubles loses integers above 2^53; the text is exact). Strings and nested values longer than 512 characters are cut with an ellipsis, as in the text."`
	// RowCount is len(Rows).
	RowCount        int          `json:"row_count" jsonschema:"The number of rows in rows."`
	Truncated       bool         `json:"truncated" jsonschema:"True when the result is incomplete; truncation says why."`
	Truncation      []Truncation `json:"truncation" jsonschema:"Why the result is incomplete, empty when it is complete."`
	NoData          bool         `json:"no_data" jsonschema:"True when Arc found nothing to read: a measurement that does not exist or has no data."`
	ExecutionTimeMs float64      `json:"execution_time_ms" jsonschema:"Arc's execution time for the query, in milliseconds."`
}

// DatabaseInfo is one database in list_databases.
type DatabaseInfo struct {
	Name             string `json:"name"`
	MeasurementCount int    `json:"measurement_count"`
}

// DatabasesOutput is the structured result of list_databases.
type DatabasesOutput struct {
	Databases []DatabaseInfo `json:"databases" jsonschema:"The databases this token can see."`
	Count     int            `json:"count"`
}

// MeasurementsOutput is the structured result of list_measurements.
type MeasurementsOutput struct {
	Database     string   `json:"database"`
	Measurements []string `json:"measurements" jsonschema:"The measurement (table) names in the database."`
	Count        int      `json:"count"`
}

// ColumnInfo is one column in describe_measurement.
type ColumnInfo struct {
	Name string `json:"name"`
	Type string `json:"type" jsonschema:"The column's type, or empty when Arc did not report it."`
}

// StatsOutput is describe_measurement's statistics.
type StatsOutput struct {
	RowCount *int64            `json:"row_count" jsonschema:"Rows in the measurement, or null when not computed (see errors)."`
	Earliest *string           `json:"earliest" jsonschema:"Time of the earliest row, or null."`
	Latest   *string           `json:"latest" jsonschema:"Time of the latest row, or null."`
	Errors   map[string]string `json:"errors,omitempty" jsonschema:"Why a statistic is null, keyed by its name."`
}

// DescribeOutput is the structured result of describe_measurement.
type DescribeOutput struct {
	Database    string       `json:"database"`
	Measurement string       `json:"measurement"`
	Columns     []ColumnInfo `json:"columns" jsonschema:"The columns shown, in order."`
	Truncated   bool         `json:"truncated" jsonschema:"True when not every column fits the answer's size limit."`
	Statistics  StatsOutput  `json:"statistics"`
}

// Output schemas. The listing tools can also answer with the scoped outcome
// of #8 (ScopedResult), unchanged, so their schema is either shape: anyOf,
// each branch with its own required fields.
var (
	queryOutputSchema        = queryOutputSchemaFor()
	describeOutputSchema     = nonNullArrays(mustSchema[DescribeOutput](), "columns")
	databasesOutputSchema    = eitherSchema(nonNullArrays(mustSchema[DatabasesOutput](), "databases"), mustSchema[ScopedOutcome]())
	measurementsOutputSchema = eitherSchema(nonNullArrays(mustSchema[MeasurementsOutput](), "measurements"), mustSchema[ScopedOutcome]())
)

func queryOutputSchemaFor() *jsonschema.Schema {
	s := nonNullArrays(mustSchema[QueryOutput](), "columns", "column_types", "rows", "truncation")
	s.Properties["truncation"].Items.Properties["reason"].Enum = []any{"row_limit", "arc_row_cap", "size_budget", "stream_incomplete"}
	return s
}

func mustSchema[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("output schema for %T: %v", *new(T), err))
	}
	return s
}

// nonNullArrays marks array properties the handlers always send (as [] when
// empty) as plain arrays rather than the nullable arrays a Go slice infers.
func nonNullArrays(s *jsonschema.Schema, names ...string) *jsonschema.Schema {
	for _, n := range names {
		if p := s.Properties[n]; p != nil {
			p.Type, p.Types = "array", nil
		}
	}
	return s
}

// eitherSchema is an object schema matching a or b.
func eitherSchema(a, b *jsonschema.Schema) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", AnyOf: []*jsonschema.Schema{a, b}}
}

// withStructured adds v as the result's structured content, marshalled here
// so numbers stay exact (see the package comment above). A value that does
// not marshal (unreachable for Arc's decoded JSON) becomes an error result:
// a success without structured content would break clients that check it.
func withStructured(res *mcp.CallToolResult, v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("structured output: %v", err)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Internal error: the result could not be encoded. See server logs."}},
			IsError: true,
		}
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

// renderResult renders a query result as markdown and as QueryOutput.
// When maxChars > 0, rows are added while both the table text and the
// structured rows' JSON stay within the budget, so text and structured
// content always describe the same prefix of the result and neither passes
// the limit; a row too large to fit alone is not shown, and a note says so.
// noData is the text when Arc read nothing.
func renderResult(result *arc.QueryResponse, notes []note, noData string, maxChars int) (string, QueryOutput) {
	notes = append([]note(nil), notes...)
	out := QueryOutput{Columns: []string{}, ColumnTypes: []string{}, Rows: [][]any{}, Truncation: []Truncation{}, ExecutionTimeMs: result.ExecutionTimeMs}
	if len(result.Columns) == 0 && len(notes) == 0 {
		out.NoData = true
		return noData, out
	}
	if result.Columns != nil {
		out.Columns = result.Columns
	}
	out.ColumnTypes = columnTypes(result)

	var table strings.Builder
	if len(out.Columns) > 0 {
		table.WriteString("| ")
		for i, col := range out.Columns {
			if i > 0 {
				table.WriteString(" | ")
			}
			table.WriteString(escapeMarkdownCell(col))
		}
		table.WriteString(" |\n|")
		for range out.Columns {
			table.WriteString("---|")
		}
		table.WriteString("\n")
	}

	// Room for the notes, the footer and a size note.
	budget := maxChars - min(600, maxChars/4)
	for _, n := range notes {
		budget -= len(n.markdown()) + 2
	}
	// The structured header (columns and their types) is sent whole; it
	// counts against the same budget as the rows, so rows are added only
	// while header and rows together fit.
	header, _ := json.Marshal(struct {
		C []string `json:"columns"`
		T []string `json:"column_types"`
	}{out.Columns, out.ColumnTypes})
	jsonSize := len(header)
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
		srow := structuredRow(row)
		b, _ := json.Marshal(srow)
		if maxChars > 0 && (table.Len()+line.Len() > budget || jsonSize+len(b)+1 > budget) {
			break
		}
		table.WriteString(line.String())
		out.Rows = append(out.Rows, srow)
		jsonSize += len(b) + 1
		shown++
	}
	switch {
	case shown == 0 && len(result.Data) > 0 && maxChars > 0 && jsonSize > budget:
		notes = append(notes, note{"size_budget", "No rows shown: the column names and types alone fill the answer's size limit.", "Select fewer columns."})
	case shown == 0 && len(result.Data) > 0:
		notes = append(notes, note{"size_budget", "No rows shown: a single row is larger than the answer's size limit.", "Select fewer columns."})
	case shown < len(result.Data):
		notes = append(notes, note{"size_budget", fmt.Sprintf("Showing the first %s: the answer reached its size limit.", plural(shown, "row")), "Select fewer columns, or aggregate."})
	}
	out.RowCount = shown

	var sb strings.Builder
	for _, n := range notes {
		sb.WriteString(n.markdown())
		sb.WriteString("\n\n")
		out.Truncation = append(out.Truncation, Truncation{Reason: n.reason, Message: n.plain()})
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

// structuredRow is a row for QueryOutput: values as Arc sent them, with a
// string, or a nested value's JSON, longer than maxCellRunes cut with an
// ellipsis, as the text's cells are.
func structuredRow(row []any) []any {
	out := make([]any, len(row))
	for i, v := range row {
		switch x := v.(type) {
		case string:
			if r := []rune(x); len(r) > maxCellRunes {
				v = string(r[:maxCellRunes]) + "…"
			}
		case map[string]any, []any:
			if b, err := json.Marshal(x); err == nil {
				if r := []rune(string(b)); len(r) > maxCellRunes {
					v = string(r[:maxCellRunes]) + "…"
				}
			}
		}
		out[i] = v
	}
	return out
}

// arcNotes are the notes for what Arc itself reported about a result: its
// own row cap, and a stream it cut short. Both query and get_sample_data
// add them.
func arcNotes(result *arc.QueryResponse) []note {
	var notes []note
	if result.RowsCapped {
		notes = append(notes, note{"arc_row_cap", fmt.Sprintf("Arc's row cap (%d) cut this result; more rows exist.", result.RowCap), "Aggregate or narrow the time range."})
	}
	if result.Truncated {
		log.Printf("query: Arc truncated the result: %s", result.TruncationReason)
		if why := result.FixableTruncation(); why != "" {
			// A failure the query itself causes (a bad cast on a later row,
			// say) happens again on every retry: say what it is.
			notes = append(notes, note{"stream_incomplete", "Arc stopped partway through this result, so it is incomplete:", why + " Fix the query; running it again fails the same way."})
		} else {
			notes = append(notes, note{"stream_incomplete", "Arc stopped partway through this result, so it is incomplete.", "Try again, or narrow the query."})
		}
	}
	return notes
}

// maxTypeRunes is where a type name is cut: an unknown: name carries Arrow's
// own description of the type, of any length.
const maxTypeRunes = 128

// columnTypes is QueryOutput.ColumnTypes: Arc's types when it sent one per
// column, each cut at maxTypeRunes; empty otherwise. A count that does not
// match the columns is Arc's bug, logged, and the types are dropped rather
// than misaligned.
func columnTypes(result *arc.QueryResponse) []string {
	if len(result.Types) != len(result.Columns) {
		if result.Types != nil {
			log.Printf("query: Arc sent %d column types for %d columns; leaving column_types empty", len(result.Types), len(result.Columns))
		}
		return []string{}
	}
	types := make([]string, len(result.Types))
	for i, t := range result.Types {
		if r := []rune(t); len(r) > maxTypeRunes {
			t = string(r[:maxTypeRunes]) + "…"
		}
		types[i] = t
	}
	return types
}

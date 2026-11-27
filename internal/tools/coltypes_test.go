package tools

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// structuredQuery is a call's structured content as QueryOutput.
func structuredQuery(t *testing.T, res *mcp.CallToolResult) QueryOutput {
	t.Helper()
	b, _ := json.Marshal(res.StructuredContent)
	var out QueryOutput
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// #38: Arc's types reach column_types exactly, parallel to columns, through
// query and get_sample_data, including names with brackets, commas, quotes
// and non-ASCII (an unknown: name carries Arrow's own text).
func TestColumnTypesPassThrough(t *testing.T) {
	types := []string{"timestamp[ns]", "date32", "string_encoded", "decimal(18, 2)", `unknown:dense_union<a: int32|"é">`}
	body, _ := json.Marshal(map[string]any{
		"success": true, "columns": []string{"time", "day", "tag", "amount", "u"}, "types": types,
		"data": [][]any{{"2026-11-27T00:00:00Z", "2026-11-27T00:00:00Z", "x", 1.25, "?"}}, "row_count": 1,
	})
	for _, tool := range []string{"query", "get_sample_data"} {
		t.Run(tool, func(t *testing.T) {
			cs := allToolsSession(t, "", "", func(string) (int, string) { return 200, string(body) })
			args := map[string]any{"database": "db", "sql": "SELECT * FROM t"}
			if tool == "get_sample_data" {
				args = map[string]any{"database": "db", "measurement": "t"}
			}
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			out := structuredQuery(t, res)
			if strings.Join(out.ColumnTypes, "|") != strings.Join(types, "|") {
				t.Errorf("column_types %q, want %q", out.ColumnTypes, types)
			}
			// The text is unchanged: no types in it.
			if strings.Contains(resultText(t, res), "timestamp[ns]") {
				t.Error("the text shows types")
			}
		})
	}
}

// Without types (an Arc before 27.01.1), or with a count that does not
// match the columns, column_types is [] (never null), and the result is
// otherwise unchanged; a mismatch is logged.
func TestColumnTypesAbsentOrMismatched(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(&bytes.Buffer{}) })
	for name, tt := range map[string]struct {
		types  []string
		logged bool
	}{
		"absent":    {nil, false},
		"shorter":   {[]string{"int64"}, true},
		"longer":    {[]string{"int64", "utf8", "bool"}, true},
		"no column": {[]string{}, true},
	} {
		buf.Reset()
		r := &arc.QueryResponse{Columns: []string{"a", "b"}, Types: tt.types, Data: [][]any{{json.Number("1"), "x"}}}
		_, out := renderResult(r, nil, "none", 50_000)
		b, _ := json.Marshal(out)
		if !strings.Contains(string(b), `"column_types":[]`) || len(out.Rows) != 1 {
			t.Errorf("%s: %s", name, b)
		}
		if logged := strings.Contains(buf.String(), "column types for"); logged != tt.logged {
			t.Errorf("%s: logged %v, want %v", name, logged, tt.logged)
		}
	}
	// No data at all: [] too.
	_, out := renderResult(&arc.QueryResponse{}, nil, "none", 50_000)
	if b, _ := json.Marshal(out); !out.NoData || !strings.Contains(string(b), `"column_types":[]`) {
		t.Errorf("no data: %s", b)
	}
}

// Types come through on a result Arc cut (its row cap, or a stream that
// stopped): they describe the columns, which are complete.
func TestColumnTypesOnIncompleteResults(t *testing.T) {
	for _, r := range []*arc.QueryResponse{
		{Columns: []string{"n"}, Types: []string{"int64"}, Data: [][]any{{json.Number("1")}}, RowsCapped: true, RowCap: 1},
		{Columns: []string{"n"}, Types: []string{"int64"}, Data: [][]any{{json.Number("1")}}, Truncated: true, TruncationReason: "x"},
	} {
		_, out := renderResult(r, arcNotes(r), "none", 50_000)
		if len(out.ColumnTypes) != 1 || out.ColumnTypes[0] != "int64" || !out.Truncated {
			t.Errorf("%+v", out)
		}
	}
}

// A very long type name is cut, as cells are.
func TestColumnTypesAreCut(t *testing.T) {
	long := "unknown:" + strings.Repeat("x", 1000)
	_, out := renderResult(&arc.QueryResponse{Columns: []string{"u"}, Types: []string{long}, Data: [][]any{{"v"}}}, nil, "none", 50_000)
	if n := len([]rune(out.ColumnTypes[0])); n != maxTypeRunes+1 || !strings.HasSuffix(out.ColumnTypes[0], "…") {
		t.Errorf("cut to %d runes: %q", n, out.ColumnTypes[0])
	}
}

// The structured header (columns and types) counts against the size
// budget, so structured content stays within it; when the header alone
// fills the budget, the note says so rather than blaming a row.
func TestColumnTypesShareTheBudget(t *testing.T) {
	const maxChars = 4000
	var cols, types []string
	row := []any{}
	for i := 0; i < 60; i++ {
		cols = append(cols, strings.Repeat("c", 20)+string(rune('a'+i%26)))
		types = append(types, "timestamp[ns]")
		row = append(row, "2026-11-27T00:00:00Z")
	}
	data := make([][]any, 50)
	for i := range data {
		data[i] = row
	}
	_, out := renderResult(&arc.QueryResponse{Columns: cols, Types: types, Data: data}, nil, "none", maxChars)
	if b, _ := json.Marshal(out); len(b) > maxChars {
		t.Errorf("structured content is %d bytes, over %d", len(b), maxChars)
	}

	// Header alone over the budget.
	for i := 0; i < 300; i++ {
		cols = append(cols, strings.Repeat("d", 30))
		types = append(types, "float64")
	}
	wide := make([]any, len(cols))
	for i := range wide {
		wide[i] = json.Number("1")
	}
	_, out = renderResult(&arc.QueryResponse{Columns: cols, Types: types, Data: [][]any{wide}}, nil, "none", maxChars)
	if len(out.Rows) != 0 || len(out.Truncation) != 1 || !strings.Contains(out.Truncation[0].Message, "column names and types alone") {
		t.Errorf("rows %d, truncation %+v", len(out.Rows), out.Truncation)
	}
}

// Counting the header does not change what a narrow numeric result shows
// at the default limit: all 500 rows, as before.
func TestColumnTypesKeepNarrowResultsWhole(t *testing.T) {
	data := make([][]any, 500)
	for i := range data {
		data[i] = []any{json.Number("1700000000000"), json.Number("42.5")}
	}
	_, out := renderResult(&arc.QueryResponse{Columns: []string{"time", "v"}, Types: []string{"int64", "float64"}, Data: data}, nil, "none", 50_000)
	if len(out.Rows) != 500 || out.Truncated {
		t.Errorf("%d rows shown, truncated %v", len(out.Rows), out.Truncated)
	}
}

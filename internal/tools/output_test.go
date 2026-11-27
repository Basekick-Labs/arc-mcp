package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// allToolsSession registers every tool against an Arc that answers the
// listing routes and the query route.
func allToolsSession(t *testing.T, dbs, meas string, query func(string) (int, string)) *mcp.ClientSession {
	t.Helper()
	return newSession(t, routeHandler{dbs: dbs, meas: meas, query: &fakeArc{reply: query}}, func(server *mcp.Server, client *arc.Client) {
		RegisterAll(server, client, Groups{Data: true}, 500, 50_000)
	})
}

// #20: every tool declares an output schema, and every success result's
// structured content validates against it, empty answers included.
func TestStructuredOutputMatchesItsSchema(t *testing.T) {
	describe := `{"success":true,"columns":["column_name","column_type"],"data":[["time","TIMESTAMP"],["v","BIGINT"]],"row_count":2,"execution_time_ms":1}`
	stats := `{"success":true,"columns":["row_count","earliest","latest"],"data":[[9007199254740993,"2026-11-01T00:00:00Z","2026-11-26T00:00:00Z"]],"row_count":1}`
	query := func(sql string) (int, string) {
		switch {
		case strings.HasPrefix(sql, "DESCRIBE"):
			return 200, describe
		case strings.Contains(sql, "count(*)") && strings.Contains(sql, "statsfail"):
			return 500, `{"error":"boom"}`
		case strings.Contains(sql, "count(*)"):
			return 200, stats
		case strings.Contains(sql, "nosuch"):
			return 200, `{"success":true,"columns":[],"data":[],"row_count":0}`
		case strings.Contains(sql, "many"):
			return 200, rowsBody(501)
		case strings.Contains(sql, "capped"):
			return 200, `{"success":true,"columns":["n"],"data":[[1]],"row_count":1,"rows_capped":true,"row_cap":1}`
		}
		return 200, rowsBody(3)
	}
	tests := []struct {
		name, dbs, meas, tool string
		args                  map[string]any
	}{
		{"databases", `{"databases":[{"name":"db","measurement_count":2}],"count":1}`, "", "list_databases", nil},
		{"no databases", `{"databases":[],"count":0}`, "", "list_databases", nil},
		{"measurements", "", `{"database":"db","measurements":[{"name":"cpu"}],"count":1}`, "list_measurements", map[string]any{"database": "db"}},
		{"no measurements", "", `{"database":"db","measurements":[],"count":0}`, "list_measurements", map[string]any{"database": "db"}},
		{"describe", "", "", "describe_measurement", map[string]any{"database": "db", "measurement": "cpu"}},
		{"query", "", "", "query", map[string]any{"database": "db", "sql": "SELECT n FROM t"}},
		{"query, no data", "", "", "query", map[string]any{"database": "db", "sql": "SELECT n FROM nosuch"}},
		{"sample", "", "", "get_sample_data", map[string]any{"database": "db", "measurement": "cpu"}},
		{"databases, scoped token", "403:" + scopedAllBody, "", "list_databases", nil},
		{"measurements, scoped token", "", "403:" + scoped403Body, "list_measurements", map[string]any{"database": "metrics"}},
		{"query, row limit", "", "", "query", map[string]any{"database": "db", "sql": "SELECT n FROM many"}},
		{"query, Arc's cap", "", "", "query", map[string]any{"database": "db", "sql": "SELECT n FROM capped"}},
		{"describe, a statistic failed", "", "", "describe_measurement", map[string]any{"database": "db", "measurement": "statsfail"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := allToolsSession(t, tt.dbs, tt.meas, query)
			tools, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var schema *jsonschema.Schema
			for _, tool := range tools.Tools {
				if tool.OutputSchema == nil {
					t.Errorf("%s has no outputSchema", tool.Name)
				}
				if tool.Name == tt.tool {
					b, _ := json.Marshal(tool.OutputSchema)
					schema = &jsonschema.Schema{}
					if err := json.Unmarshal(b, schema); err != nil {
						t.Fatal(err)
					}
				}
			}
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tt.tool, Arguments: tt.args})
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError || res.StructuredContent == nil {
				t.Fatalf("isError=%v structured=%v text=%q", res.IsError, res.StructuredContent, resultText(t, res))
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := resolved.Validate(res.StructuredContent); err != nil {
				t.Errorf("structured content does not match the schema: %v\n%v", err, res.StructuredContent)
			}
		})
	}
}

// Numbers reach the client exactly as Arc sent them. Checked on the raw
// JSON-RPC bytes: the SDK client itself decodes into float64.
func TestStructuredNumbersAreExactOnTheWire(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) {
		return 200, `{"success":true,"columns":["big","dec"],"data":[[9007199254740993,1.10]],"row_count":1,"execution_time_ms":1}`
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterQuery(server, arc.NewClient(srv.URL, "tok", 5*time.Second), 500, 50_000)
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	var wire bytes.Buffer
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "c"}, nil).Connect(t.Context(), &mcp.LoggingTransport{Transport: clientT, Writer: &wire}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "query", Arguments: map[string]any{"database": "db", "sql": "SELECT 1"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wire.String(), `"rows":[[9007199254740993,1.10]]`) {
		t.Errorf("structured rows not exact on the wire:\n%s", wire.String())
	}
}

// Error results carry no structured content.
func TestErrorResultsHaveNoStructuredContent(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) { return 500, `{"error":"boom"}` }}
	cs := dataSession(t, fake, 500, 50_000)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "query", Arguments: map[string]any{"database": "db", "sql": "SELECT 1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.StructuredContent != nil {
		t.Errorf("isError=%v structured=%v", res.IsError, res.StructuredContent)
	}
}

func TestDescribeStatisticsAreTyped(t *testing.T) {
	ok := statsOutput([3]statResult{{value: json.Number("1784100")}, {value: "2026-11-01T00:00:00Z"}, {value: nil}})
	if ok.RowCount == nil || *ok.RowCount != 1784100 || ok.Earliest == nil || ok.Latest != nil || ok.Errors != nil {
		t.Errorf("%+v", ok)
	}
	failed := statsOutput([3]statResult{{reason: "not computed (timed out after 30s)"}, {value: "x"}, {value: "y"}})
	if failed.RowCount != nil || failed.Errors["row_count"] != "not computed (timed out after 30s)" {
		t.Errorf("%+v", failed)
	}
}

func TestStructuredCellsAreCutLikeTheText(t *testing.T) {
	long := strings.Repeat("é", 600)
	row := structuredRow([]any{long, json.Number("1"), nil})
	if s := row[0].(string); len([]rune(s)) != maxCellRunes+1 || !strings.HasSuffix(s, "…") {
		t.Errorf("cell not cut: %d runes", len([]rune(s)))
	}
	if row[1] != json.Number("1") || row[2] != nil {
		t.Errorf("other cells changed: %v", row)
	}
}

// routeHandler answers Arc's listing routes with fixed bodies and passes
// the query route to query.
type routeHandler struct {
	dbs, meas string
	query     *fakeArc
}

// listingReply sends body, or a 403 with the rest of the body when it starts
// with "403:".
func listingReply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	if rest, ok := strings.CutPrefix(body, "403:"); ok {
		w.WriteHeader(http.StatusForbidden)
		body = rest
	}
	_, _ = io.WriteString(w, body)
}

func (h routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/measurements"):
		listingReply(w, h.meas)
	case strings.HasSuffix(r.URL.Path, "/databases"):
		listingReply(w, h.dbs)
	default:
		h.query.ServeHTTP(w, r)
	}
}

// The listing tools' schema accepts either shape, not anything.
func TestEitherSchemaRejectsEmptyObject(t *testing.T) {
	resolved, err := databasesOutputSchema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Validate(map[string]any{}) == nil {
		t.Error("an empty object validated")
	}
}

// A row larger than the whole size limit is not shown, and the answer says
// so, in text and structured content alike.
func TestSizeBudgetRowLargerThanTheLimit(t *testing.T) {
	cols := make([]string, 300)
	cells := make([]string, 300)
	for i := range cols {
		cols[i] = fmt.Sprintf("%q", fmt.Sprintf("c%d", i))
		cells[i] = fmt.Sprintf("%q", strings.Repeat("x", 500))
	}
	body := `{"success":true,"columns":[` + strings.Join(cols, ",") + `],"data":[[` + strings.Join(cells, ",") + `]],"row_count":1}`
	var result arc.QueryResponse
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	text, out := renderResult(&result, nil, "", 50_000)
	if out.RowCount != 0 || len(out.Rows) != 0 || !out.Truncated || out.Truncation[0].Reason != "size_budget" {
		t.Errorf("structured: rows %d, truncation %+v", out.RowCount, out.Truncation)
	}
	if len(text) > 50_000 || !strings.Contains(text, "No rows shown") {
		t.Errorf("text %d chars: %.200s", len(text), text)
	}
}

// Nested values are cut like strings, in both outputs.
func TestStructuredNestedCellsAreCut(t *testing.T) {
	list := make([]any, 5000)
	for i := range list {
		list[i] = json.Number("1")
	}
	row := structuredRow([]any{list, map[string]any{"k": strings.Repeat("v", 600)}})
	for i, c := range row {
		s, ok := c.(string)
		if !ok || len([]rune(s)) != maxCellRunes+1 {
			t.Errorf("cell %d not cut: %T", i, c)
		}
	}
}

// get_sample_data reports what Arc said about its result, like query.
func TestSampleReportsArcTruncation(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) {
		return 200, `{"success":true,"columns":["n"],"data":[[1]],"row_count":1,"rows_capped":true,"row_cap":1}`
	}}
	cs := dataSession(t, fake, 500, 50_000)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_sample_data", Arguments: map[string]any{"database": "db", "measurement": "cpu"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out QueryOutput
	_ = json.Unmarshal(b, &out)
	if !out.Truncated || len(out.Truncation) != 1 || out.Truncation[0].Reason != "arc_row_cap" {
		t.Errorf("%+v", out)
	}
}

// A result Arc cut short before any column is not "no data".
func TestNoColumnsButTruncatedIsNotNoData(t *testing.T) {
	result := arc.QueryResponse{Truncated: true, TruncationReason: "x"}
	text, out := renderResult(&result, arcNotes(&result), "No data", 50_000)
	if out.NoData || !out.Truncated || strings.HasPrefix(text, "No data") {
		t.Errorf("no_data=%v truncated=%v text=%q", out.NoData, out.Truncated, text)
	}
}

// describe lists the same columns in text and structured content, within
// the size limit, and sends "" for a type Arc did not report.
func TestDescribeColumnsShareTheBudget(t *testing.T) {
	var rows []string
	for i := 0; i < 2000; i++ {
		rows = append(rows, fmt.Sprintf(`["column_%04d","VARCHAR"]`, i))
	}
	describe := `{"success":true,"columns":["column_name","column_type"],"data":[` + strings.Join(rows, ",") + `],"row_count":2000}`
	fake := &fakeArc{reply: func(sql string) (int, string) {
		if strings.HasPrefix(sql, "DESCRIBE") {
			return 200, describe
		}
		return 200, `{"success":true,"columns":["row_count","earliest","latest"],"data":[[1,null,null]],"row_count":1}`
	}}
	cs := dataSession(t, fake, 500, 8_000)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "describe_measurement", Arguments: map[string]any{"database": "db", "measurement": "cpu"}})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, res)
	b, _ := json.Marshal(res.StructuredContent)
	var out DescribeOutput
	_ = json.Unmarshal(b, &out)
	inText := strings.Count(text, "| VARCHAR |")
	if !out.Truncated || len(out.Columns) == 0 || len(out.Columns) != inText || len(text) > 8_000 {
		t.Errorf("structured %d columns (truncated %v), text %d columns, %d chars", len(out.Columns), out.Truncated, inText, len(text))
	}
}

// A type Arc did not report is "" in the structured columns, not the
// text's placeholder.
func TestDescribeUnknownTypeIsEmpty(t *testing.T) {
	fake := &fakeArc{reply: func(sql string) (int, string) {
		switch {
		case strings.HasPrefix(sql, "DESCRIBE"):
			return 400, `{"success":false,"error":"refused"}`
		case strings.HasSuffix(sql, "LIMIT 0"):
			return 200, `{"success":true,"columns":["time","v"],"data":[],"row_count":0}`
		}
		return 200, `{"success":true,"columns":["row_count","earliest","latest"],"data":[[1,null,null]],"row_count":1}`
	}}
	cs := dataSession(t, fake, 500, 50_000)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "describe_measurement", Arguments: map[string]any{"database": "db", "measurement": "cpu"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out DescribeOutput
	_ = json.Unmarshal(b, &out)
	if len(out.Columns) != 2 || out.Columns[1].Type != "" {
		t.Errorf("%+v", out.Columns)
	}
}

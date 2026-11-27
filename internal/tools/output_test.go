package tools

import (
	"bytes"
	"encoding/json"
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
		RegisterAll(server, client, 500, 50_000)
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
		case strings.Contains(sql, "count(*)"):
			return 200, stats
		case strings.Contains(sql, "nosuch"):
			return 200, `{"success":true,"columns":[],"data":[],"row_count":0}`
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

func (h routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/measurements"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, h.meas)
	case strings.HasSuffix(r.URL.Path, "/databases"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, h.dbs)
	default:
		h.query.ServeHTTP(w, r)
	}
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/basekick-labs/arc-mcp/internal/arc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeArc answers /api/v1/query with whatever reply returns for the SQL it
// received, and records every statement in order.
type fakeArc struct {
	mu    sync.Mutex
	sqls  []string
	reply func(sql string) (status int, body string)
	// replyCtx, when set, is used instead of reply: it sees the request's
	// context, so it can wait for the client to give up.
	replyCtx func(ctx context.Context, sql string) (status int, body string)
}

func (f *fakeArc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SQL string `json:"sql"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	f.sqls = append(f.sqls, req.SQL)
	f.mu.Unlock()
	var status int
	var body string
	if f.replyCtx != nil {
		status, body = f.replyCtx(r.Context(), req.SQL)
	} else {
		status, body = f.reply(req.SQL)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeArc) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sqls...)
}

// dataRow matches one rendered row of rowsBody's integer column.
var dataRow = regexp.MustCompile(`(?m)^\| \d+ \|$`)

// rowsBody is an Arc success body with n rows of one integer column.
func rowsBody(n int) string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf("[%d]", i)
	}
	return fmt.Sprintf(`{"success":true,"columns":["n"],"data":[%s],"row_count":%d,"execution_time_ms":2}`, strings.Join(rows, ","), n)
}

// dataSession registers the data tools against fake.
func dataSession(t *testing.T, fake *fakeArc, maxRows, maxChars int) *mcp.ClientSession {
	t.Helper()
	return newSession(t, fake, func(server *mcp.Server, client *arc.Client) {
		RegisterQuery(server, client, maxRows, maxChars)
		RegisterGetSampleData(server, client, maxChars)
		RegisterDescribeMeasurement(server, client, maxChars)
	})
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", tool, err)
	}
	return resultText(t, res), res.IsError
}

func TestQuery_LimitsAndCutsRows(t *testing.T) {
	tests := []struct {
		name      string
		arcRows   int
		wantShown int
		wantNote  bool
	}{
		{"more rows than the limit", 6, 5, true},
		{"exactly the limit", 5, 5, false},
		{"fewer rows", 1, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeArc{reply: func(string) (int, string) { return 200, rowsBody(tt.arcRows) }}
			cs := dataSession(t, fake, 5, 50_000)

			text, isErr := call(t, cs, "query", map[string]any{"database": "db", "sql": "SELECT n FROM t -- recent"})
			if isErr {
				t.Fatalf("unexpected error: %s", text)
			}
			sent := fake.received()
			if len(sent) != 1 || sent[0] != "SELECT n FROM t -- recent\nLIMIT 6" {
				t.Fatalf("Arc received %q", sent)
			}
			if got := len(dataRow.FindAllString(text, -1)); got != tt.wantShown {
				t.Errorf("rows shown = %d, want %d\n%s", got, tt.wantShown, text)
			}
			if hasNote := strings.HasPrefix(text, "**Showing the first 5 rows; the query returned more.**"); hasNote != tt.wantNote {
				t.Errorf("truncation note at top = %v, want %v\n%s", hasNote, tt.wantNote, text)
			}
			wantFooter := plural(tt.wantShown, "row") + " returned in"
			if tt.wantNote {
				wantFooter = plural(tt.wantShown, "row") + " shown in"
			}
			if !strings.Contains(text, wantFooter) {
				t.Errorf("footer missing %q\n%s", wantFooter, text)
			}
		})
	}
}

// When the size budget is reached the table stops at a row boundary: the
// row-limit note stays first, a size note says how many rows are shown, and
// the structured rows are the same rows.
func TestQuery_SizeBudgetStopsAtARowBoundary(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) { return 200, rowsBody(501) }}
	cs := newSession(t, fake, func(server *mcp.Server, client *arc.Client) {
		RegisterQuery(server, client, 500, 900)
	})
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "query", Arguments: map[string]any{"database": "db", "sql": "SELECT n FROM t"}})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, res)
	if !strings.HasPrefix(text, "**Showing the first 500 rows") || !strings.Contains(text, "the answer reached its size limit") {
		t.Fatalf("notes wrong:\n%s", text)
	}
	shown := len(dataRow.FindAllString(text, -1))
	b, _ := json.Marshal(res.StructuredContent)
	var out QueryOutput
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if shown == 0 || out.RowCount != shown || len(out.Rows) != shown || !out.Truncated {
		t.Errorf("text shows %d rows, structured %d (%d), truncated %v", shown, out.RowCount, len(out.Rows), out.Truncated)
	}
	var reasons []string
	for _, tr := range out.Truncation {
		reasons = append(reasons, tr.Reason)
	}
	if strings.Join(reasons, ",") != "row_limit,size_budget" {
		t.Errorf("truncation %v", out.Truncation)
	}
	if len(text) > 900 || strings.Contains(text, "[truncated") {
		t.Errorf("text is %d chars or was cut by the backstop", len(text))
	}
	if b, _ := json.Marshal(out.Rows); len(b) > 900 {
		t.Errorf("structured rows are %d bytes, over the 900 budget", len(b))
	}
}

// SHOW goes to Arc unwrapped, and its rows are still cut.
func TestQuery_PassThroughStatementsAreCut(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) { return 200, rowsBody(9) }}
	cs := dataSession(t, fake, 5, 50_000)

	text, _ := call(t, cs, "query", map[string]any{"database": "db", "sql": "SHOW TABLES"})
	if sent := fake.received(); len(sent) != 1 || sent[0] != "SHOW TABLES" {
		t.Fatalf("Arc received %q", sent)
	}
	if !strings.HasPrefix(text, "**Showing the first 5 rows; the query returned more.** Narrow the statement to see the rest.") {
		t.Errorf("SHOW result not cut, or wrong hint:\n%s", text)
	}
}

func TestQuery_IntegersReachTheModelExactly(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) {
		return 200, `{"success":true,"columns":["n","big_id"],"data":[[1784100,9007199254740993]],"row_count":1,"execution_time_ms":1}`
	}}
	cs := dataSession(t, fake, 500, 50_000)

	text, _ := call(t, cs, "query", map[string]any{"database": "db", "sql": "SELECT count(*) AS n, 9007199254740993 AS big_id FROM t"})
	if !strings.Contains(text, "| 1784100 | 9007199254740993 |") {
		t.Errorf("numbers changed:\n%s", text)
	}
	if !strings.Contains(text, "*1 row returned") {
		t.Errorf("singular footer missing:\n%s", text)
	}
}

func TestQuery_RefusalTellsTheModelWhatToChange(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) { return 200, rowsBody(1) }}
	cs := dataSession(t, fake, 500, 50_000)

	text, isErr := call(t, cs, "query", map[string]any{"database": "db", "sql": "SELECT avg(cpu) AS load FROM host"})
	if !isErr || !strings.Contains(text, `double-quote it: "load"`) {
		t.Errorf("isErr=%v text=%s", isErr, text)
	}
	if len(fake.received()) != 0 {
		t.Errorf("a refused query reached Arc")
	}
	text, isErr = call(t, cs, "query", map[string]any{"database": "db", "sql": "SELECT avg(v) AS export FROM grid"})
	if isErr {
		t.Errorf("export alias refused: %s", text)
	}
}

const describeBody = `{"success":true,"columns":["column_name","column_type","null","key","default","extra"],` +
	`"data":[["time","TIMESTAMP WITH TIME ZONE","YES",null,null,null],["altitude","BIGINT","YES",null,null,null]],"row_count":2}`

const statsBody = `{"success":true,"columns":["row_count","earliest","latest"],"data":[[1784100,"2026-11-12T00:00:00Z","2026-11-26T20:00:00Z"]],"row_count":1}`

func TestDescribe_TypesFromDescribeSelect(t *testing.T) {
	fake := &fakeArc{reply: func(sql string) (int, string) {
		if strings.HasPrefix(sql, "DESCRIBE SELECT") {
			return 200, describeBody
		}
		return 200, statsBody
	}}
	cs := dataSession(t, fake, 500, 50_000)

	text, isErr := call(t, cs, "describe_measurement", map[string]any{"database": "db", "measurement": "adsb"})
	if isErr {
		t.Fatalf("error: %s", text)
	}
	for _, want := range []string{"| time | TIMESTAMP WITH TIME ZONE |", "| altitude | BIGINT |", "**Row count:** 1784100"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q\n%s", want, text)
		}
	}
	sent := fake.received()
	if len(sent) != 2 || sent[0] != "DESCRIBE SELECT * FROM adsb" {
		t.Errorf("queries sent = %q, want DESCRIBE SELECT then stats only", sent)
	}
}

func TestDescribe_EmptyMeasurementIsSaid(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) {
		return 200, `{"success":true,"columns":[],"data":[],"row_count":0}`
	}}
	cs := dataSession(t, fake, 500, 50_000)

	text, isErr := call(t, cs, "describe_measurement", map[string]any{"database": "db", "measurement": "nosuch"})
	if !isErr || !strings.Contains(text, "has no data in database 'db', or does not exist") {
		t.Errorf("isErr=%v text=%s", isErr, text)
	}
	if n := len(fake.received()); n != 1 {
		t.Errorf("%d queries sent, want 1 (no fallback, no stats)", n)
	}
}

func TestDescribe_FallbackOnlyWhenDescribeIsRefused(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantFallback bool
	}{
		{"400 refusal", 400, `{"success":false,"error":"Dangerous SQL operation not allowed"}`, true},
		{"500 parser error", 500, `{"success":false,"error":"Parser Error: syntax error at or near DESCRIBE"}`, true},
		{"500 other DuckDB error", 500, `{"success":false,"error":"IO Error: corrupt file. LINE 1: DESCRIBE SELECT * FROM read_parquet(...)"}`, false},
		{"200 with success false", 200, `{"success":false,"error":"Catalog Error"}`, true},
		{"504 timeout", 504, `{"error":"timeout"}`, false},
		{"503 unavailable", 503, `{"error":"cold tier unavailable"}`, false},
		{"401 auth", 401, `{"error":"Authentication required"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeArc{reply: func(sql string) (int, string) {
				switch {
				case strings.HasPrefix(sql, "DESCRIBE"):
					return tt.status, tt.body
				case strings.HasSuffix(sql, "LIMIT 0"):
					return 200, `{"success":true,"columns":["time","altitude"],"data":[],"row_count":0}`
				}
				return 200, statsBody
			}}
			cs := dataSession(t, fake, 500, 50_000)

			text, isErr := call(t, cs, "describe_measurement", map[string]any{"database": "db", "measurement": "adsb"})
			fellBack := false
			for _, s := range fake.received() {
				fellBack = fellBack || strings.HasSuffix(s, "LIMIT 0")
			}
			if fellBack != tt.wantFallback {
				t.Errorf("fallback = %v, want %v (sent %q)", fellBack, tt.wantFallback, fake.received())
			}
			if tt.wantFallback && (isErr || !strings.Contains(text, "| altitude | — |")) {
				t.Errorf("fallback result wrong: isErr=%v\n%s", isErr, text)
			}
			if !tt.wantFallback && !isErr {
				t.Errorf("want an error result, got:\n%s", text)
			}
		})
	}
}

// #16: limit has a default, so the schema must not require it, and a call
// without it uses 10.
func TestGetSampleData_LimitIsOptional(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) { return 200, rowsBody(1) }}
	cs := dataSession(t, fake, 500, 50_000)

	if text, isErr := call(t, cs, "get_sample_data", map[string]any{"database": "db", "measurement": "cpu"}); isErr {
		t.Fatalf("call without limit failed: %s", text)
	}
	if sent := fake.received(); len(sent) != 1 || sent[0] != "SELECT * FROM cpu WHERE time > now() - INTERVAL '1 day' ORDER BY time DESC LIMIT 10" {
		t.Errorf("Arc received %q", sent)
	}

	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "get_sample_data" {
			continue
		}
		b, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Required []string `json:"required"`
		}
		_ = json.Unmarshal(b, &schema)
		if strings.Join(schema.Required, ",") != "database,measurement" {
			t.Errorf("required = %v, want [database measurement]", schema.Required)
		}
		return
	}
	t.Fatal("get_sample_data not registered")
}

// A refusal Arc makes that arc-mcp does not (file functions, for one)
// reaches the model in Arc's words, not as "see server logs".
func TestQuery_ArcValidationMessageReachesTheModel(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) {
		return 400, `{"success":false,"error":"File I/O function not allowed in user queries"}`
	}}
	cs := dataSession(t, fake, 500, 50_000)

	text, isErr := call(t, cs, "query", map[string]any{"database": "db", "sql": "SELECT * FROM read_csv('/etc/hostname')"})
	if !isErr || text != "Arc refused the request: File I/O function not allowed in user queries" {
		t.Errorf("isErr=%v text=%q", isErr, text)
	}
}

// Arc's own governance cap and a stream that failed partway both still say
// success:true; the model must be told the rows are incomplete.
func TestQuery_ArcCapAndTruncationAreSaid(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"governance cap", `{"success":true,"columns":["n"],"data":[[1],[2]],"row_count":2,"rows_capped":true,"row_cap":2}`,
			"**Arc's row cap (2) cut this result; more rows exist.**"},
		{"stream truncated", `{"success":true,"columns":["n"],"data":[[1]],"row_count":1,"truncated":true,"truncation_reason":"IO Error: /data/x.parquet"}`,
			"**Arc stopped partway through this result, so it is incomplete.**"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeArc{reply: func(string) (int, string) { return 200, tt.body }}
			cs := dataSession(t, fake, 500, 50_000)
			text, isErr := call(t, cs, "query", map[string]any{"database": "db", "sql": "SELECT n FROM t"})
			if isErr || !strings.HasPrefix(text, tt.want) {
				t.Errorf("isErr=%v, want prefix %q, got:\n%s", isErr, tt.want, text)
			}
			if strings.Contains(text, "/data/") {
				t.Errorf("truncation reason leaked to the model:\n%s", text)
			}
			if !strings.Contains(text, " shown in ") {
				t.Errorf("footer should say shown:\n%s", text)
			}
		})
	}
}

// #17: every tool is marked read-only and closed-world, with a title, so
// clients can run it without asking the user each time.
func TestEveryToolIsAnnotatedReadOnly(t *testing.T) {
	cs := newSession(t, &fakeArc{reply: func(string) (int, string) { return 200, rowsBody(1) }}, func(server *mcp.Server, client *arc.Client) {
		RegisterAll(server, client, Groups{Data: true, Ops: true}, 500, 50_000)
	})
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 7 {
		t.Fatalf("%d tools registered, want 7 (data and ops)", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || a.Title == "" || tool.Title == "" || !a.IdempotentHint ||
			a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s: annotations %+v", tool.Name, a)
		}
	}
}

// Arc returns no columns only when it found nothing to read: a measurement
// that does not exist or has no data. Say that, naming where to look.
func TestEmptyResultNamesTheLikelyCause(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) {
		return 200, `{"success":true,"columns":[],"data":[],"row_count":0,"execution_time_ms":1}`
	}}
	cs := dataSession(t, fake, 500, 50_000)

	text, isErr := call(t, cs, "query", map[string]any{"database": "telemetry", "sql": "SELECT avg(v) FROM cpuu"})
	if isErr || !strings.Contains(text, "No data") || !strings.Contains(text, "database 'telemetry'") || !strings.Contains(text, "not 0") {
		t.Errorf("query: isErr=%v %q", isErr, text)
	}
	text, _ = call(t, cs, "get_sample_data", map[string]any{"database": "telemetry", "measurement": "cpuu"})
	if !strings.Contains(text, "measurement 'cpuu' does not exist in database 'telemetry'") {
		t.Errorf("get_sample_data: %q", text)
	}
}

// A failure the query causes partway through (a bad cast on a later row) is
// said as it is, without inviting a retry that fails the same way.
func TestMidStreamQueryErrorIsSaid(t *testing.T) {
	fake := &fakeArc{reply: func(string) (int, string) {
		// As Arc sends it: its stream prefix, one line, quoted values masked
		// (SanitizeErrText in the arc repo).
		return 200, `{"success":true,"columns":["n"],"data":[[1]],"row_count":1,"truncated":true,` +
			`"truncation_reason":"arrow reader error after 1000 rows: Conversion Error: Could not convert string '...' to INT64"}`
	}}
	cs := dataSession(t, fake, 500, 50_000)
	text, _ := call(t, cs, "query", map[string]any{"database": "db", "sql": "SELECT CAST(s AS BIGINT) FROM t"})
	if !strings.Contains(text, "Conversion Error: Could not convert string '...' to INT64") || strings.Contains(text, "Try again") || strings.Contains(text, "arrow reader error") {
		t.Errorf("got %q", text)
	}
}

// get_sample_data widens its window only while the measurement exists and
// the window is empty, and never reads all history first.
func TestGetSampleData_WidensTheWindowWhenEmpty(t *testing.T) {
	empty := `{"success":true,"columns":["time","v"],"data":[],"row_count":0}`
	fake := &fakeArc{reply: func(sql string) (int, string) {
		if strings.Contains(sql, "30 days") {
			return 200, `{"success":true,"columns":["time","v"],"data":[["2026-11-01T00:00:00Z",7]],"row_count":1}`
		}
		return 200, empty
	}}
	cs := dataSession(t, fake, 500, 50_000)
	text, _ := call(t, cs, "get_sample_data", map[string]any{"database": "db", "measurement": "cpu"})
	sent := fake.received()
	if len(sent) != 2 || !strings.Contains(sent[0], "1 day") || !strings.Contains(sent[1], "30 days") || !strings.Contains(text, "| 7 |") {
		t.Errorf("sent %q, got %q", sent, text)
	}
}
